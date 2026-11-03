#!/usr/bin/env node
// ui-check: load pages in a real headless browser (Chromium through Playwright) across a device matrix,
// scroll each page to its end and fail on layout defects a first-screen check misses.
//
// Usage: node ui-check.mjs <base-url> <path>... [--out dir] [--schemes auto|light|dark|light,dark]
//                          [--timeout ms] [--dwell ms] [--limit n]
//
// Checks per device x scheme x page: no 4xx/5xx or failed response, no horizontal page scroll
// (documentElement.scrollWidth <= clientWidth), no visible element sticking out of the viewport outside a
// scroll container, no broken images, CLS < 0.1, no word split inside a word (a word whose rendered
// rects fall on more than one line, e.g. a table cell narrower than its longest word). A full-page
// screenshot of each run and results.json go to --out (default ./ui-check). Layout shifts are observed
// from navigation until the page is scrolled through and at least --dwell ms (default 3000) have passed.
//
// Playwright is not a dependency of this repository: it is resolved from the caller's environment, in this
// order: node_modules above the current directory, each NODE_PATH entry, the global npm root. Install it
// with `npm i -D playwright && npx playwright install chromium` (or globally) where you run the check.
//
// Exit: 0 every run passed, 1 a check failed, 2 the check could not run (usage error, no Playwright, no
// browser); on 2 the last line starts with "BROWSER CHECK NOT RUN:".
//
// The functions exported below are pure (no Node or Playwright state) so scripts/ui-check.test.mjs can
// test them; wordSegments, countLines, sticksOut and pageProbe also run inside the page, serialized by
// probeExpression, so they must not refer to anything outside their own bodies.

import { execFileSync } from 'node:child_process';
import { mkdirSync, realpathSync, writeFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

// The minimum device matrix (owner requirement 2026-11-03).
export const DEVICES = Object.freeze([
  { name: '360x740', width: 360, height: 740, deviceScaleFactor: 3, isMobile: true, hasTouch: true },
  { name: '390x844', width: 390, height: 844, deviceScaleFactor: 3, isMobile: true, hasTouch: true },
  { name: '768x1024', width: 768, height: 1024, deviceScaleFactor: 2, isMobile: true, hasTouch: true },
  { name: '1366x768', width: 1366, height: 768, deviceScaleFactor: 1, isMobile: false, hasTouch: false },
  { name: '1920x1080', width: 1920, height: 1080, deviceScaleFactor: 1, isMobile: false, hasTouch: false },
]);

export const CLS_LIMIT = 0.1;

export const USAGE = 'usage: node ui-check.mjs <base-url> <path>... [--out dir] ' +
  '[--schemes auto|light|dark|light,dark] [--timeout ms] [--dwell ms] [--limit n]';

export class UsageError extends Error {}

/**
 * Parses the command line.
 * @param {string[]} argv arguments after the script name
 * @returns {{base: string, paths: string[], out: string, schemes: string, timeout: number, limit: number, help: boolean}}
 */
export function parseArgs(argv) {
  const opts = { base: '', paths: [], out: 'ui-check', schemes: 'auto', timeout: 30000, dwell: 3000, limit: 5, help: false };
  const valued = new Set(['--out', '--schemes', '--timeout', '--dwell', '--limit']);
  for (let i = 0; i < argv.length; i++) {
    let arg = argv[i];
    if (arg === '-h' || arg === '--help') {
      opts.help = true;
      continue;
    }
    if (arg.startsWith('--')) {
      let value;
      const eq = arg.indexOf('=');
      if (eq > 0) {
        value = arg.slice(eq + 1);
        arg = arg.slice(0, eq);
      }
      if (!valued.has(arg)) throw new UsageError(`unknown option ${arg}`);
      if (value === undefined) {
        value = argv[++i];
        if (value === undefined) throw new UsageError(`${arg} needs a value`);
      }
      opts[arg.slice(2)] = value;
      continue;
    }
    if (!opts.base) opts.base = arg;
    else opts.paths.push(arg);
  }
  if (opts.help) return opts;
  if (!opts.base) throw new UsageError('missing <base-url>');
  let url;
  try {
    url = new URL(opts.base);
  } catch {
    throw new UsageError(`bad <base-url> ${JSON.stringify(opts.base)}`);
  }
  if (!['http:', 'https:', 'file:'].includes(url.protocol)) throw new UsageError(`unsupported protocol ${url.protocol}`);
  if (opts.paths.length === 0) throw new UsageError('give at least one <path> (use / for the base page)');
  if (!['auto', 'light', 'dark', 'light,dark', 'dark,light'].includes(opts.schemes)) {
    throw new UsageError(`bad --schemes ${JSON.stringify(opts.schemes)}`);
  }
  for (const key of ['timeout', 'dwell', 'limit']) {
    const n = Number(opts[key]);
    if (!Number.isInteger(n) || n < (key === 'dwell' ? 0 : 1)) throw new UsageError(`--${key} must be a ${key === 'dwell' ? 'non-negative' : 'positive'} integer`);
    opts[key] = n;
  }
  if (!opts.out) throw new UsageError('--out must not be empty');
  return opts;
}

/**
 * Splits text into the runs between line-break opportunities (whitespace, hyphens, soft hyphens, dashes,
 * slashes, zero-width spaces). A run of 2+ characters is a word that must never break across lines.
 * Runs in scripts written without spaces (Han, Kana, Thai, ...) are left out: they break anywhere.
 * Offsets are UTF-16 indices, as Range.setStart expects. Runs inside the page: self-contained.
 * @param {string} text
 * @returns {{start: number, end: number, word: string}[]}
 */
export function wordSegments(text) {
  const out = [];
  const re = /[^\s\-­​‐-―\/]{2,}/gu;
  const breaksAnywhere = /[\p{Script=Han}\p{Script=Hiragana}\p{Script=Katakana}\p{Script=Thai}\p{Script=Lao}\p{Script=Khmer}\p{Script=Myanmar}]/u;
  let m;
  while ((m = re.exec(text)) !== null) {
    if (!breaksAnywhere.test(m[0])) out.push({ start: m.index, end: m.index + m[0].length, word: m[0] });
  }
  return out;
}

/**
 * Counts the text lines a word's client rects (Range.getClientRects) fall on: two rects share a line when
 * they overlap vertically by more than half the smaller height. Empty rects are ignored, so a word that
 * is not rendered counts 0 lines. Runs inside the page: self-contained.
 * @param {{top: number, bottom: number, left: number, right: number}[]} rects
 * @returns {number}
 */
export function countLines(rects) {
  const lines = [];
  for (const r of rects) {
    const h = r.bottom - r.top;
    if (!(r.right - r.left > 0.5) || !(h > 0)) continue;
    const same = lines.find((l) => Math.min(l.bottom, r.bottom) - Math.max(l.top, r.top) > 0.5 * Math.min(l.bottom - l.top, h));
    if (same) {
      same.top = Math.min(same.top, r.top);
      same.bottom = Math.max(same.bottom, r.bottom);
    } else {
      lines.push({ top: r.top, bottom: r.bottom });
    }
  }
  return lines.length;
}

/**
 * Whether an element's box is visible in the viewport horizontally and sticks out of it by more than
 * 1px on either side. A box entirely off-screen (an off-canvas menu) does not count. Runs inside the page.
 * @param {{left: number, right: number, width: number, height: number}} r getBoundingClientRect()
 * @param {number} vw viewport width (documentElement.clientWidth)
 * @returns {boolean}
 */
export function sticksOut(r, vw) {
  if (!(r.width > 0) || !(r.height > 0)) return false;
  const visible = r.right > 0 && r.left < vw;
  return visible && (r.left < -1 || r.right > vw + 1);
}

/**
 * Cumulative Layout Shift from layout-shift entries: the largest session window (shifts less than 1 s
 * apart, a window at most 5 s long), shifts right after user input excluded, as web-vitals computes it.
 * @param {{value: number, startTime: number, hadRecentInput: boolean}[]} entries
 * @returns {number}
 */
export function clsFromShifts(entries) {
  let max = 0;
  let sum = 0;
  let first = 0;
  let last = 0;
  for (const e of [...entries].sort((a, b) => a.startTime - b.startTime)) {
    if (e.hadRecentInput) continue;
    if (sum > 0 && e.startTime - last < 1000 && e.startTime - first < 5000) {
      sum += e.value;
    } else {
      sum = e.value;
      first = e.startTime;
    }
    last = e.startTime;
    max = Math.max(max, sum);
  }
  return max;
}

/**
 * The in-page probe, run after the page was scrolled to its end and back to the top. h carries
 * wordSegments, countLines and sticksOut (serialized in by probeExpression); limit caps the examples
 * kept per check (the counts are complete).
 * @param {{wordSegments: Function, countLines: Function, sticksOut: Function}} h
 * @param {number} limit
 */
export function pageProbe(h, limit) {
  const de = document.documentElement;
  const body = document.body;
  const vw = de.clientWidth;
  const out = {
    scrollWidth: de.scrollWidth, clientWidth: vw, scrollHeight: de.scrollHeight,
    stickOut: [], stickOutCount: 0, brokenImages: [], brokenImageCount: 0, splitWords: [], splitWordCount: 0,
  };
  if (!body) return out;
  const styles = new Map();
  const style = (el) => {
    let s = styles.get(el);
    if (!s) {
      s = getComputedStyle(el);
      styles.set(el, s);
    }
    return s;
  };
  const name = (el) => {
    let s = String(el.tagName).toLowerCase();
    if (el.id) s += '#' + el.id;
    const cls = typeof el.className === 'string' ? el.className.trim() : '';
    if (cls) s += '.' + cls.split(/\s+/).slice(0, 2).join('.');
    return s;
  };
  const describe = (el) => (el.parentElement && el.parentElement !== body ? name(el.parentElement) + ' > ' : '') + name(el);
  // An ancestor below body that does not let content overflow (auto, scroll, hidden, clip) is a scroll or
  // clip container: what sticks out of it is scrolled or cut there, not on the page. body and html are not
  // counted: content cut by an overflow-x: hidden body is still a defect.
  const contained = (el) => {
    for (let p = el.parentElement; p && p !== body && p !== de; p = p.parentElement) {
      if (style(p).overflowX !== 'visible') return true;
    }
    return false;
  };

  const reported = [];
  for (const el of body.querySelectorAll('*')) {
    const r = el.getBoundingClientRect();
    if (!h.sticksOut(r, vw)) continue;
    const s = style(el);
    if (s.visibility === 'hidden' || Number(s.opacity) === 0 || contained(el)) continue;
    if (reported.some((p) => p.contains(el))) continue; // report the outermost box only
    reported.push(el);
    out.stickOutCount++;
    if (out.stickOut.length < limit) out.stickOut.push({ el: describe(el), left: Math.round(r.left), right: Math.round(r.right) });
  }

  for (const img of Array.from(document.images)) {
    const src = img.currentSrc || img.getAttribute('src') || '';
    if (!src || img.getClientRects().length === 0) continue; // not rendered (display: none)
    if (img.complete && img.naturalWidth > 0) continue;
    out.brokenImageCount++;
    if (out.brokenImages.length < limit) out.brokenImages.push(src.slice(0, 200));
  }

  const skip = new Set(['SCRIPT', 'STYLE', 'NOSCRIPT', 'TEMPLATE', 'TEXTAREA', 'OPTION']);
  const walker = document.createTreeWalker(body, 4 /* NodeFilter.SHOW_TEXT */);
  const range = document.createRange();
  const seen = new Set();
  let words = 0;
  for (let n = walker.nextNode(); n && words < 200000; n = walker.nextNode()) {
    const p = n.parentElement;
    if (!p || skip.has(String(p.tagName).toUpperCase())) continue;
    const text = n.data;
    if (!text || text.trim().length < 2) continue;
    let hyphenated = null;
    for (const seg of h.wordSegments(text)) {
      words++;
      range.setStart(n, seg.start);
      range.setEnd(n, seg.end);
      if (h.countLines(Array.from(range.getClientRects())) < 2) continue;
      // hyphens: auto breaks words on purpose, with a visible hyphen.
      if (hyphenated === null) hyphenated = style(p).hyphens === 'auto';
      if (hyphenated) break;
      out.splitWordCount++;
      const where = describe(p);
      const key = seg.word + '\u0000' + where;
      if (!seen.has(key) && out.splitWords.length < limit) {
        seen.add(key);
        out.splitWords.push({ word: seg.word.slice(0, 80), el: where });
      }
    }
  }
  return out;
}

/**
 * The page.evaluate expression that runs pageProbe with its helpers.
 * @param {number} limit
 * @returns {string}
 */
export function probeExpression(limit) {
  return `(${pageProbe})({ wordSegments: ${wordSegments}, countLines: ${countLines}, sticksOut: ${sticksOut} }, ${Number(limit)})`;
}

/** In-page: whether the page's own CSS or meta declares a dark scheme. */
export function declaresDark() {
  const meta = document.querySelector('meta[name="color-scheme"]');
  if (meta && /\bdark\b/.test(meta.getAttribute('content') || '')) return true;
  if (/\bdark\b/.test(getComputedStyle(document.documentElement).colorScheme || '')) return true;
  const dark = /prefers-color-scheme\s*:\s*dark/;
  const walk = (rules) => {
    for (const r of Array.from(rules || [])) {
      if (r.media && dark.test(r.media.mediaText)) return true;
      if (r.styleSheet && walk(r.styleSheet.cssRules)) return true;
      if (r.cssRules && walk(r.cssRules)) return true;
    }
    return false;
  };
  for (const s of Array.from(document.styleSheets)) {
    try {
      if ((s.media && dark.test(s.media.mediaText)) || walk(s.cssRules)) return true;
    } catch {
      // A cross-origin stylesheet's rules cannot be read; the colour fingerprint still compares it.
    }
  }
  return false;
}

/** In-page: the colours a scheme switch changes (a site that toggles a class from JS shows here too). */
export function schemeFingerprint() {
  const els = [document.documentElement, document.body, document.querySelector('main'), document.body && document.body.firstElementChild];
  return els.filter(Boolean).map((el) => {
    const s = getComputedStyle(el);
    return [s.backgroundColor, s.color].join('|');
  }).join('/');
}

/**
 * The failures of one run, from its probe and the responses seen.
 * @returns {string[]}
 */
export function failuresOf(run) {
  const f = [];
  if (run.error) f.push(`error: ${run.error}`);
  if (run.badResponses.length) f.push(`http: ${run.badResponses.length} bad (first: ${run.badResponses[0]})`);
  const p = run.probe;
  if (p) {
    if (p.scrollWidth > p.clientWidth) f.push(`hscroll: scrollWidth ${p.scrollWidth} > clientWidth ${p.clientWidth}`);
    if (p.stickOutCount) {
      const e = p.stickOut[0];
      f.push(`overflow: ${p.stickOutCount} element(s) stick out (first: ${e.el} [${e.left}..${e.right}])`);
    }
    if (p.brokenImageCount) f.push(`images: ${p.brokenImageCount} broken (first: ${p.brokenImages[0]})`);
    if (p.splitWordCount) {
      const w = p.splitWords.map((s) => `"${s.word}" in ${s.el}`).join(', ');
      f.push(`split-words: ${p.splitWordCount} (${w})`);
    }
  }
  if (run.cls === null) f.push('cls: not measured');
  else if (run.cls !== undefined && run.cls >= CLS_LIMIT) f.push(`cls: ${run.cls.toFixed(3)} >= ${CLS_LIMIT}`);
  return f;
}

/** A file name part for a page path. */
export function slug(p) {
  const s = p.replace(/[^A-Za-z0-9]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 60);
  return s || 'root';
}

/**
 * Finds Playwright in the caller's environment (cwd's node_modules, NODE_PATH, the global npm root).
 * @returns {string|null} the resolved module path
 */
export function resolvePlaywright(cwd = process.cwd(), env = process.env, globalRoot = npmGlobalRoot) {
  const req = createRequire(path.join(cwd, 'ui-check-resolve.js'));
  const names = ['playwright', 'playwright-core', '@playwright/test'];
  const tryFrom = (paths) => {
    for (const n of names) {
      try {
        return paths ? req.resolve(n, { paths }) : req.resolve(n);
      } catch {
        // not there; try the next name
      }
    }
    return null;
  };
  let found = tryFrom(null);
  for (const dir of (env.NODE_PATH || '').split(path.delimiter).filter(Boolean)) {
    found = found || tryFrom([dir]);
  }
  if (!found) {
    const root = globalRoot();
    if (root) found = tryFrom([root]);
  }
  return found;
}

function npmGlobalRoot() {
  try {
    return execFileSync('npm', ['root', '-g'], { encoding: 'utf8', timeout: 15000, stdio: ['ignore', 'pipe', 'ignore'] }).trim();
  } catch {
    return '';
  }
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function settle(page, timeout) {
  try {
    await page.waitForLoadState('networkidle', { timeout: Math.min(timeout, 10000) });
  } catch {
    // a page that keeps polling never goes idle; the checks run anyway
  }
}

// Scrolls to the end in steps of 80% of the viewport, so lazy content loads, until the height stops growing
// (at most 400 steps for endless pages), then back to the top.
async function scrollFull(page) {
  await page.evaluate(async () => {
    const pause = (ms) => new Promise((r) => setTimeout(r, ms));
    const step = Math.max(200, Math.floor(window.innerHeight * 0.8));
    let lastHeight = -1;
    for (let i = 0; i < 400; i++) {
      window.scrollBy(0, step);
      await pause(100);
      const h = document.documentElement.scrollHeight;
      if (window.scrollY + window.innerHeight >= h - 2) {
        if (h === lastHeight) break;
        lastHeight = h;
        await pause(400);
      }
    }
  });
}

async function openPage(browser, device, scheme) {
  const context = await browser.newContext({
    viewport: { width: device.width, height: device.height }, deviceScaleFactor: device.deviceScaleFactor,
    isMobile: device.isMobile, hasTouch: device.hasTouch, colorScheme: scheme,
  });
  await context.addInitScript(() => {
    window.__uiCheckShifts = [];
    try {
      new PerformanceObserver((list) => {
        for (const e of list.getEntries()) window.__uiCheckShifts.push({ value: e.value, startTime: e.startTime, hadRecentInput: e.hadRecentInput });
      }).observe({ type: 'layout-shift', buffered: true });
    } catch {
      window.__uiCheckShifts = null;
    }
  });
  return { context, page: await context.newPage() };
}

async function runOne(browser, device, scheme, pagePath, url, shot, opts) {
  const run = { device: device.name, scheme, path: pagePath, url, screenshot: shot, badResponses: [], probe: null, cls: undefined, error: '' };
  const { context, page } = await openPage(browser, device, scheme);
  page.on('response', (r) => {
    if (r.status() >= 400) run.badResponses.push(`${r.status()} ${r.url()}`);
  });
  page.on('requestfailed', (r) => {
    const t = (r.failure() && r.failure().errorText) || 'failed';
    if (!/ERR_ABORTED|NS_BINDING_ABORTED/.test(t)) run.badResponses.push(`${t} ${r.url()}`);
  });
  try {
    const resp = await page.goto(url, { waitUntil: 'load', timeout: opts.timeout });
    if (!resp && !url.startsWith('file:')) throw new Error('no response');
    await settle(page, opts.timeout);
    await scrollFull(page);
    await settle(page, opts.timeout);
    await page.evaluate(() => window.scrollTo(0, 0));
    // Late shifts (a banner or an ad injected after load) count too: observe at least opts.dwell ms.
    const elapsed = await page.evaluate(() => performance.now());
    await sleep(Math.max(300, opts.dwell - elapsed));
    const shifts = await page.evaluate(() => window.__uiCheckShifts);
    run.cls = Array.isArray(shifts) ? clsFromShifts(shifts) : null;
    run.probe = await page.evaluate(probeExpression(opts.limit));
    await page.screenshot({ path: shot, fullPage: true });
  } catch (e) {
    run.error = String((e && e.message) || e).split('\n')[0];
  } finally {
    await context.close();
  }
  run.failures = failuresOf(run);
  return run;
}

// Whether the page has a dark scheme: its CSS or meta declares one, or its colours change under dark.
async function hasDarkScheme(browser, device, url, opts) {
  const fp = {};
  let declared = false;
  for (const scheme of ['light', 'dark']) {
    const { context, page } = await openPage(browser, device, scheme);
    try {
      await page.goto(url, { waitUntil: 'load', timeout: opts.timeout });
      await settle(page, opts.timeout);
      if (scheme === 'light') declared = await page.evaluate(`(${declaresDark})()`);
      fp[scheme] = await page.evaluate(`(${schemeFingerprint})()`);
    } catch {
      fp[scheme] = scheme; // unreadable: the runs report the error
    } finally {
      await context.close();
    }
  }
  return declared || fp.light !== fp.dark;
}

export async function main(argv, log = console.log) {
  let opts;
  try {
    opts = parseArgs(argv);
  } catch (e) {
    if (!(e instanceof UsageError)) throw e;
    log(`ui-check: ${e.message}\n${USAGE}`);
    log(`BROWSER CHECK NOT RUN: usage error: ${e.message}`);
    return 2;
  }
  if (opts.help) {
    log(USAGE);
    return 0;
  }
  const mod = resolvePlaywright();
  if (!mod) {
    log('BROWSER CHECK NOT RUN: Playwright not found (node_modules above the current directory, NODE_PATH, npm root -g); ' +
      'install it: npm i -D playwright && npx playwright install chromium');
    return 2;
  }
  const { chromium } = createRequire(import.meta.url)(mod);
  let browser;
  try {
    browser = await chromium.launch({ headless: true });
  } catch (e) {
    log(`BROWSER CHECK NOT RUN: cannot launch Chromium: ${String((e && e.message) || e).split('\n')[0]}`);
    return 2;
  }
  mkdirSync(opts.out, { recursive: true });
  const runs = [];
  try {
    for (const [i, pagePath] of opts.paths.entries()) {
      const url = new URL(pagePath, opts.base).href;
      let schemes = opts.schemes === 'auto' ? ['light'] : opts.schemes.split(',');
      if (opts.schemes === 'auto' && (await hasDarkScheme(browser, DEVICES[0], url, opts))) schemes = ['light', 'dark'];
      for (const device of DEVICES) {
        for (const scheme of schemes) {
          const shot = path.join(opts.out, `${String(i + 1).padStart(2, '0')}-${slug(pagePath)}__${device.name}__${scheme}.png`);
          const run = await runOne(browser, device, scheme, pagePath, url, shot, opts);
          runs.push(run);
          const head = `${run.failures.length ? 'FAIL' : 'PASS'} ${device.name.padEnd(9)} ${scheme.padEnd(5)} ${pagePath}`;
          log(run.failures.length ? `${head}: ${run.failures.join('; ')}` : `${head} (${shot})`);
        }
      }
    }
  } finally {
    await browser.close();
  }
  writeFileSync(path.join(opts.out, 'results.json'), JSON.stringify(runs, null, 2) + '\n');
  const failed = runs.filter((r) => r.failures.length).length;
  log(`ui-check: ${runs.length - failed}/${runs.length} passed; screenshots and results.json in ${opts.out}`);
  return failed ? 1 : 0;
}

function invokedDirectly() {
  if (!process.argv[1]) return false;
  try {
    return realpathSync(process.argv[1]) === realpathSync(fileURLToPath(import.meta.url));
  } catch {
    return false;
  }
}

if (invokedDirectly()) {
  main(process.argv.slice(2)).then(
    (code) => process.exit(code),
    (e) => {
      console.log(`BROWSER CHECK NOT RUN: ui-check crashed: ${String((e && e.stack) || e).split('\n')[0]}`);
      process.exit(2);
    },
  );
}
