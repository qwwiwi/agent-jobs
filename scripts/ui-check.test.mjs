// Static tests of scripts/ui-check.mjs (node --test; run by ui_check_test.go from scripts/check.sh).
// No browser starts here: the pure helpers are tested directly, and the in-page probe runs against a
// small fake DOM in a vm context, through the same expression the script hands to page.evaluate.
import assert from 'node:assert/strict';
import { mkdirSync, mkdtempSync, realpathSync, rmSync, writeFileSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { test } from 'node:test';
import vm from 'node:vm';

import {
  CLS_LIMIT, DEVICES, UsageError, clsFromShifts, countLines, failuresOf, hyphenationExempt, lineFragments, parseArgs,
  probeExpression, resolvePlaywright, scrollExpression, slug, sticksOut, wordSegments,
} from './ui-check.mjs';

test('device matrix is the required minimum', () => {
  assert.deepEqual(DEVICES.map((d) => d.name), ['360x740', '390x844', '768x1024', '1366x768', '1920x1080']);
  for (const d of DEVICES) assert.equal(d.name, `${d.width}x${d.height}`);
  assert.equal(CLS_LIMIT, 0.1);
});

test('parseArgs', () => {
  const o = parseArgs(['http://127.0.0.1:8000', '/', '/blog/post', '--out', 'shots', '--schemes=light,dark']);
  assert.equal(o.base, 'http://127.0.0.1:8000');
  assert.deepEqual(o.paths, ['/', '/blog/post']);
  assert.equal(o.out, 'shots');
  assert.equal(o.schemes, 'light,dark');
  assert.equal(o.timeout, 30000);
  assert.equal(o.dwell, 3000);
  assert.equal(parseArgs(['http://x', '/', '--dwell', '0']).dwell, 0);
  assert.equal(parseArgs(['--help']).help, true);
  assert.equal(o.trusted, false);
  assert.equal(o.playwright, '');
  const t = parseArgs(['http://x', '/', '--trusted', '--playwright', '/opt/pw']);
  assert.equal(t.trusted, true);
  assert.equal(t.playwright, '/opt/pw');
  assert.throws(() => parseArgs(['http://x', '/', '--trusted=1']), UsageError);
  assert.throws(() => parseArgs(['http://x', '/', '--playwright', '']), UsageError);
  for (const bad of [[], ['http://x'], ['not a url', '/'], ['ftp://x', '/'], ['http://x', '/', '--schemes', 'blue'],
    ['http://x', '/', '--out'], ['http://x', '/', '--bogus', '1'], ['http://x', '/', '--timeout', '-5'], ['http://x', '/', '--dwell', '-1'], ['http://x', '/', '--limit', '0']]) {
    assert.throws(() => parseArgs(bad), UsageError, JSON.stringify(bad));
  }
});

test('wordSegments splits at break opportunities only', () => {
  assert.deepEqual(wordSegments('Configuration of tables').map((s) => s.word), ['Configuration', 'of', 'tables']);
  assert.deepEqual(wordSegments('well-known a/b soft­hyphen x').map((s) => s.word), ['well', 'known', 'soft', 'hyphen']);
  assert.deepEqual(wordSegments('Таблица цен').map((s) => s.word), ['Таблица', 'цен']);
  assert.deepEqual(wordSegments('日本語のテキスト'), []);
  const text = '😀 Hello';
  const [seg] = wordSegments(text);
  assert.equal(text.slice(seg.start, seg.end), 'Hello'); // UTF-16 offsets
});

test('countLines groups rects by line', () => {
  const r = (top, left, w = 10, h = 20) => ({ top, bottom: top + h, left, right: left + w });
  assert.equal(countLines([]), 0);
  assert.equal(countLines([r(0, 0, 80)]), 1);
  assert.equal(countLines([r(0, 0), r(1, 10)]), 1); // an inline split on one line
  assert.equal(countLines([r(0, 0), r(20, 0), r(40, 0)]), 3); // letter by letter
  assert.equal(countLines([r(0, 0), r(20, 0, 0)]), 1); // zero-width rect at the line end
});

test('sticksOut', () => {
  const box = (left, right) => ({ left, right, width: right - left, height: 10 });
  assert.equal(sticksOut(box(0, 360), 360), false);
  assert.equal(sticksOut(box(10, 400), 360), true);
  assert.equal(sticksOut(box(-50, 100), 360), true);
  assert.equal(sticksOut(box(400, 700), 360), false); // entirely off-screen
  assert.equal(sticksOut(box(0, 361), 360), false); // 1px tolerance
  assert.equal(sticksOut({ left: 0, right: 900, width: 900, height: 0 }, 360), false);
});

test('clsFromShifts takes the largest session window', () => {
  assert.equal(clsFromShifts([]), 0);
  const s = (startTime, value, hadRecentInput = false) => ({ startTime, value, hadRecentInput });
  assert.ok(Math.abs(clsFromShifts([s(0, 0.05), s(500, 0.06)]) - 0.11) < 1e-9);
  assert.equal(clsFromShifts([s(0, 0.05), s(2000, 0.06)]), 0.06); // a gap over 1 s starts a new window
  assert.equal(clsFromShifts([s(0, 0.5, true), s(10, 0.01)]), 0.01); // after input: excluded
  const steady = Array.from({ length: 12 }, (_, i) => s(i * 900, 0.01)); // windows end at 5 s
  assert.ok(clsFromShifts(steady) < 0.065);
});

test('failuresOf and slug', () => {
  const clean = { error: '', badResponses: [], cls: 0.01, probe: { scrollWidth: 360, clientWidth: 360, stickOutCount: 0, brokenImageCount: 0, splitWordCount: 0 } };
  assert.deepEqual(failuresOf(clean), []);
  const f = failuresOf({ ...clean, badResponses: ['404 http://x/a.png'], cls: 0.2,
    probe: { ...clean.probe, scrollWidth: 420, splitWordCount: 1, splitWords: [{ word: 'Configuration', el: 'tr > td' }] } });
  assert.equal(f.length, 4);
  assert.match(f.join('\n'), /http: 1 bad/);
  assert.match(f.join('\n'), /hscroll: scrollWidth 420 > clientWidth 360/);
  assert.match(f.join('\n'), /split-words: 1 \("Configuration" in tr > td\)/);
  assert.match(f.join('\n'), /cls: 0\.200/);
  assert.deepEqual(failuresOf({ ...clean, cls: null }), ['cls: not measured']);
  assert.equal(slug('/'), 'root');
  assert.equal(slug('/blog/post-1?x=2'), 'blog-post-1-x-2');
});

const rect = (left, right, top = 0, h = 20) => ({ left, right, top, bottom: top + h, width: right - left, height: h });

// An inline style object with the CSSStyleDeclaration methods the probe uses.
function inlineStyle(init = {}) {
  const props = { ...init };
  const prio = {};
  return {
    props,
    getPropertyValue: (k) => props[k] || '',
    getPropertyPriority: (k) => prio[k] || '',
    setProperty(k, v, p = '') { props[k] = v; prio[k] = p; },
    removeProperty(k) { delete props[k]; delete prio[k]; },
  };
}

// A fake page: elements (each {tagName, parent, style, box}) and text nodes whose rects(start, end, el) say
// how a [start, end) range renders; getComputedStyle reads the element's current inline style, so a
// rendering can depend on it (hyphens switched off by the probe).
function fakePage(build) {
  const vw = 360;
  const el = (tagName, parent, opts = {}) => ({
    tagName, id: opts.id || '', className: opts.className || '', parentElement: parent, style: inlineStyle(opts.style),
    box: opts.box || rect(0, 100),
    getBoundingClientRect() { return this.box; },
    contains(o) { for (let p = o; p; p = p.parentElement) if (p === this) return true; return false; },
  });
  const html = el('HTML', null);
  const body = el('BODY', html);
  const elements = [];
  const texts = [];
  const images = [];
  const add = (tagName, parent, opts) => {
    const e = el(tagName, parent || body, opts);
    elements.push(e);
    return e;
  };
  const text = (data, parentElement, rects) => texts.push({ data, parentElement, rects: (s, e) => rects(s, e, parentElement) });
  build({ add, text, images, body });
  const document = {
    documentElement: { clientWidth: vw, scrollWidth: 500, scrollHeight: 3000 },
    body: { ...body, querySelectorAll: () => elements },
    images,
    createTreeWalker: () => {
      let i = 0;
      return { nextNode: () => texts[i++] || null };
    },
    createRange: () => {
      let node = null;
      let start = 0;
      let end = 0;
      return {
        setStart(n, o) { node = n; start = o; },
        setEnd(n, o) { end = o; },
        getClientRects() { return node.rects(start, end); },
      };
    },
  };
  for (const e of elements) if (e.parentElement === body) e.parentElement = document.body;
  const getComputedStyle = (e) => ({
    overflowX: 'visible', visibility: 'visible', opacity: '1', hyphens: 'manual', wordBreak: 'normal', overflowWrap: 'normal',
    position: 'static', display: 'block', transform: 'none', filter: 'none', perspective: 'none', contain: 'none',
    willChange: 'auto', ...e.style.props,
  });
  return { document, getComputedStyle };
}

// Renderings of a text range: one letter per line (the incident), all on one line, or broken before the
// given offsets (breaksFor(el) picks them from the element's current style).
const perLetter = (s, e) => Array.from({ length: e - s }, (_, i) => rect(0, 8, (s + i) * 20));
const oneLine = (s, e) => [rect(s * 8, e * 8)];
const brokenAt = (breaksFor) => (s, e, el) => {
  const breaks = breaksFor(el);
  const line = (k) => breaks.filter((b) => b <= k).length;
  const lineStart = (k) => [0, ...breaks].filter((b) => b <= k).pop();
  const out = [];
  for (let k = s; k < e; k++) {
    const x = (k - lineStart(k)) * 8;
    out.push(rect(x, x + 8, line(k) * 20));
  }
  return out;
};
// Broken at auto when the element's hyphens is auto, at manual otherwise.
const byHyphens = (auto, manual) => brokenAt((el) => (el.style.getPropertyValue('hyphens') === 'manual' ? manual : auto));

function runProbe(page) {
  const ctx = vm.createContext(page);
  // Serialized like page.evaluate's result (this also drops the vm realm's prototypes).
  return JSON.parse(JSON.stringify(vm.runInContext(probeExpression(5), ctx)));
}

// The base fixture: a table cell whose word "Configuration" renders one letter per line (the incident), a
// cell with a word on one line, a paragraph hyphenated by hyphens: auto (its breaks vanish under hyphens:
// manual), a script, a box sticking out of the page, one inside a scroll container, a broken image and a
// hidden one.
function fakeDOM() {
  return fakePage(({ add, text, images }) => {
    const table = add('TABLE');
    const td1 = add('TD', table, { className: 'price wide' });
    const td2 = add('TD', table);
    const para = add('P', null, { style: { hyphens: 'auto' } });
    const script = add('SCRIPT');
    const wide = add('DIV', null, { id: 'banner', box: rect(0, 500) });
    add('SPAN', wide, { box: rect(0, 480) });
    const scroller = add('DIV', null, { style: { overflowX: 'auto' } });
    add('PRE', scroller, { box: rect(0, 900) });
    text('Configuration', td1, perLetter);
    text('Short words', td2, oneLine);
    text('Internationalization', para, byHyphens([10], []));
    text('var longidentifier', script, perLetter);
    const img = (src, complete, naturalWidth, rendered = true) => ({
      currentSrc: src, complete, naturalWidth, getAttribute: () => src, getClientRects: () => (rendered ? [rect(0, 10)] : []),
    });
    images.push(img('/ok.png', true, 10), img('/missing.png', true, 0), img('/lazy-hidden.png', false, 0, false));
  });
}

test('pageProbe finds split words, stick-outs and broken images in a fake DOM', () => {
  const out = runProbe(fakeDOM());
  assert.equal(out.scrollWidth, 500);
  assert.equal(out.clientWidth, 360);
  assert.equal(out.splitWordCount, 1, JSON.stringify(out.splitWords));
  assert.deepEqual(out.splitWords, [{ word: 'Configuration', el: 'table > td.price.wide' }]);
  assert.equal(out.stickOutCount, 1, JSON.stringify(out.stickOut)); // banner only: not its child, not the scroller's pre
  assert.deepEqual(out.stickOut, [{ el: 'div#banner', left: 0, right: 500 }]);
  assert.deepEqual(out.brokenImages, ['/missing.png']);
  const failures = failuresOf({ error: '', badResponses: [], cls: 0, probe: out });
  assert.equal(failures.length, 4); // hscroll, overflow, images, split-words
});

test('lineFragments groups code points by line', () => {
  const at = (top, units = 1) => ({ rects: [rect(0, 8, top)], units });
  assert.deepEqual(lineFragments([at(0), at(0), at(20), at(20), at(20)]), [{ start: 0, length: 2 }, { start: 2, length: 3 }]);
  // A line-end hyphen rect before the char's own rect: the char belongs to the next line.
  assert.deepEqual(lineFragments([at(0), { rects: [rect(8, 13, 0), rect(0, 8, 20)], units: 1 }, at(20)]),
    [{ start: 0, length: 1 }, { start: 1, length: 2 }]);
  assert.deepEqual(lineFragments([at(0, 2), at(20)]), [{ start: 0, length: 1 }, { start: 2, length: 1 }]); // UTF-16 offsets
  assert.deepEqual(lineFragments([]), []);
});

test('hyphenationExempt needs proven hyphenation', () => {
  const s = (extra = {}) => ({ hyphens: 'auto', wordBreak: 'normal', overflowWrap: 'normal', ...extra });
  const frags = (...lens) => lens.map((length) => ({ start: 0, length }));
  assert.equal(hyphenationExempt(s(), frags(10, 10), true), true);
  assert.equal(hyphenationExempt(s(), frags(18, 2), true), true);
  assert.equal(hyphenationExempt(s({ hyphens: 'manual' }), frags(10, 10), true), false);
  assert.equal(hyphenationExempt(s(), frags(10, 10), false), false); // no dictionary: breaks do not move
  assert.equal(hyphenationExempt(s({ wordBreak: 'break-all' }), frags(5, 5, 3), true), false);
  assert.equal(hyphenationExempt(s(), frags(12, 1), true), false); // a 1-letter fragment
  assert.equal(hyphenationExempt(s({ overflowWrap: 'anywhere' }), frags(9, 9, 2), true), false);
  assert.equal(hyphenationExempt(s({ overflowWrap: 'anywhere' }), frags(10, 10), true), true);
});

// Review finding: hyphens: auto alone exempted every multiline word, so letter-by-letter wrapping under
// break-all, or with no dictionary, escaped the check.
test('pageProbe reports words hyphens: auto did not hyphenate', () => {
  let td;
  const out = runProbe(fakePage(({ add, text }) => {
    const table = add('TABLE');
    // A narrow cell with hyphens: auto and break-all: broken mid-word with no hyphen, same breaks either way.
    td = add('TD', table, { id: 'narrow', style: { hyphens: 'auto', wordBreak: 'break-all' } });
    text('Configuration', td, byHyphens([5, 10], [5, 10]));
    // No dictionary: overflow-wrap breaks the word the same way with or without hyphenation.
    const nodict = add('P', null, { id: 'nodict', style: { hyphens: 'auto', overflowWrap: 'anywhere' } });
    text('Internationalization', nodict, byHyphens([11], [11]));
    // Hyphenated, but overflow-wrap left a 2-letter tail.
    const tail = add('P', null, { id: 'tail', style: { hyphens: 'auto', overflowWrap: 'anywhere' } });
    text('Internationalization', tail, byHyphens([9, 18], [11]));
    // Hyphenated cleanly: exempt.
    const ok = add('P', null, { id: 'ok', style: { hyphens: 'auto' } });
    text('Internationalization', ok, byHyphens([5, 13], []));
  }));
  assert.deepEqual(out.splitWords.map((w) => w.el), ['table > td#narrow', 'p#nodict', 'p#tail'], JSON.stringify(out.splitWords));
  assert.equal(out.splitWordCount, 3);
  assert.equal(td.style.getPropertyValue('hyphens'), 'auto'); // inline style left as it was
});

// Review finding: any ancestor with non-visible overflow hid a stick-out, even one that is not in the
// element's containing-block chain or that itself crosses the viewport.
test('pageProbe: only a clipping ancestor in the containing-block chain hides a stick-out', () => {
  const out = runProbe(fakePage(({ add }) => {
    const inside = rect(0, 300);
    // A fixed child of an overflow:auto panel that stays inside the viewport: not clipped by it.
    const panel = add('DIV', null, { id: 'panel', box: inside, style: { overflowX: 'auto' } });
    add('DIV', panel, { id: 'fixed', box: rect(0, 500), style: { position: 'fixed' } });
    // The same under a transformed panel: the panel is its containing block and clips it.
    const tpanel = add('DIV', null, { id: 'tpanel', box: inside, style: { overflowX: 'auto', transform: 'matrix(1, 0, 0, 1, 0, 0)' } });
    add('DIV', tpanel, { id: 'fixedin', box: rect(0, 500), style: { position: 'fixed' } });
    // An absolute child of an unpositioned overflow:hidden box: escapes it.
    const clip = add('DIV', null, { id: 'clip', box: inside, style: { overflowX: 'hidden' } });
    add('DIV', clip, { id: 'abs', box: rect(0, 520), style: { position: 'absolute' } });
    // An absolute child of a positioned overflow:hidden box: clipped.
    const pclip = add('DIV', null, { id: 'pclip', box: inside, style: { overflowX: 'hidden', position: 'relative' } });
    add('DIV', pclip, { id: 'absin', box: rect(0, 520), style: { position: 'absolute' } });
    // An in-flow child of a scroller that itself reaches past the viewport (hidden, so not reported itself).
    const big = add('DIV', null, { id: 'big', box: rect(0, 900), style: { overflowX: 'auto', visibility: 'hidden' } });
    add('DIV', big, { id: 'inbig', box: rect(0, 800), style: { visibility: 'visible' } });
    // An in-flow child of a scroller inside the viewport: scrolled there, not a defect.
    const sc = add('DIV', null, { id: 'sc', box: inside, style: { overflowX: 'auto' } });
    add('PRE', sc, { id: 'pre', box: rect(0, 900) });
  }));
  assert.deepEqual(out.stickOut.map((s) => s.el), ['div#panel > div#fixed', 'div#clip > div#abs', 'div#big > div#inbig'],
    JSON.stringify(out.stickOut));
  assert.equal(out.stickOutCount, 3);
});

// A fake scroll container for scrollThrough: content can grow when its end is reached (lazy content).
function fakeScroller({ tagName = 'MAIN', id = '', clientHeight, clientWidth = 360, content, grow = () => false, overflowY = 'auto' }) {
  const s = { tagName, id, className: '', scrollTop: 0, clientHeight, clientWidth, scrollHeight: content, overflowY, loads: 0, steps: 0 };
  const clamp = () => {
    s.scrollTop = Math.max(0, Math.min(s.scrollTop, s.scrollHeight - s.clientHeight));
    if (s.scrollTop + s.clientHeight >= s.scrollHeight - 2 && grow(s)) s.loads++;
  };
  s.scrollBy = ({ top }) => { s.steps++; s.order = s.order || ++fakeScroller.seq; s.scrollTop += top; clamp(); };
  s.scrollTo = ({ top }) => { s.scrollTop = top; clamp(); };
  return s;
}
fakeScroller.seq = 0;

// A fake scrollable page for scrollThrough: the window, an optional main inner scroller (inner), further
// scrollers (more) and an optional body that is itself a scroller (body), with the root's overflow-y.
function fakeScrollPage({ winHeight = 740, winContent = 740, inner = null, more = [], body = null, rootOverflowY = 'visible' }) {
  const win = { scrollY: 0, innerHeight: winHeight };
  const de = { scrollHeight: winContent, overflowY: rootOverflowY };
  const clampWin = () => { win.scrollY = Math.max(0, Math.min(win.scrollY, de.scrollHeight - win.innerHeight)); };
  win.scrollBy = ({ top }) => { win.scrollY += top; clampWin(); };
  win.scrollTo = ({ top }) => { win.scrollY = top; clampWin(); };
  // A small overflow-y:auto box (scrolls less than a viewport) that is not traversed.
  const small = fakeScroller({ tagName: 'DIV', clientHeight: 100, clientWidth: 100, content: 400 });
  const elements = [small];
  const scroller = inner ? fakeScroller({ id: 'app', ...inner }) : null;
  if (scroller) elements.push(scroller);
  const others = more.map((m) => fakeScroller(m));
  elements.push(...others);
  const bodyEl = body ? fakeScroller({ tagName: 'BODY', ...body }) : {};
  bodyEl.querySelectorAll = () => elements;
  const ctx = vm.createContext({
    window: win, setTimeout,
    document: { documentElement: de, body: bodyEl },
    getComputedStyle: (e) => ({ overflowY: e.overflowY || 'visible' }),
  });
  return { ctx, win, scroller, others, small, body: body ? bodyEl : null };
}

const scrollIn = (ctx, steps) => vm.runInContext(scrollExpression(steps, 0, 0), ctx);

// Review finding: only the window was scrolled, so an app shell with a 100vh overflow-y:auto main area
// was never traversed and its lazy content never loaded.
test('scrollThrough traverses the main inner scroller and loads its lazy content', async () => {
  let lazy = 2;
  const { ctx, win, scroller } = fakeScrollPage({
    inner: { clientHeight: 700, content: 2000, grow: (s) => lazy-- > 0 && (s.scrollHeight += 1500) },
  });
  const problems = await scrollIn(ctx, 400);
  assert.deepEqual(Array.from(problems), []);
  assert.equal(scroller.loads, 2); // both lazy chunks below loaded
  assert.equal(scroller.scrollHeight, 5000);
  assert.equal(scroller.scrollTop, 0); // back at the top for the probe and the screenshot
  assert.equal(win.scrollY, 0);
});

test('scrollThrough traverses a long window page', async () => {
  const { ctx, win } = fakeScrollPage({ winContent: 5000 });
  assert.deepEqual(Array.from(await scrollIn(ctx, 400)), []);
  assert.equal(win.scrollY, 0);
});

// Review finding: hitting the step cap (an endless feed) passed silently.
test('scrollThrough reports cap exhaustion and a scroller that cannot reach its end', async () => {
  const endless = fakeScrollPage({ inner: { clientHeight: 700, content: 2000, grow: (s) => (s.scrollHeight += 1000) } });
  const capped = Array.from(await scrollIn(endless.ctx, 5));
  assert.equal(capped.length, 1, JSON.stringify(capped));
  assert.match(capped[0], /^main#app: stopped after 5 steps/);
  assert.equal(endless.scroller.steps, 5);

  // A window whose content is taller than the viewport but does not move (overflow: hidden on html).
  const stuck = fakeScrollPage({ winContent: 3000 });
  stuck.win.scrollBy = () => {};
  const p = Array.from(await scrollIn(stuck.ctx, 400));
  assert.equal(p.length, 1, JSON.stringify(p));
  assert.match(p[0], /^window: cannot scroll past 0 of 3000/);

  const f = failuresOf({ error: '', badResponses: [], scrollProblems: capped, cls: 0, probe: null });
  assert.equal(f.length, 1);
  assert.match(f[0], /^scroll: not traversed to the end \(main#app: stopped after 5 steps/);
});

// Review finding: html{overflow:hidden} with body{height:100vh;overflow:auto} scrolls in body, which was
// never a candidate, so the lazy content at its bottom was never loaded or checked.
test('scrollThrough traverses body when it scrolls independently and loads its lazy content', async () => {
  let lazy = 1;
  const { ctx, win, body } = fakeScrollPage({
    rootOverflowY: 'hidden',
    body: { clientHeight: 740, content: 3000, grow: (s) => lazy-- > 0 && (s.scrollHeight += 2000) },
  });
  assert.deepEqual(Array.from(await scrollIn(ctx, 400)), []);
  assert.equal(body.loads, 1);
  assert.equal(body.scrollHeight, 5000);
  assert.equal(body.scrollTop, 0);
  assert.equal(win.scrollY, 0);

  // With a visible root, body's overflow applies to the viewport: body is the window, not traversed again.
  const propagated = fakeScrollPage({ body: { clientHeight: 740, content: 3000 } });
  assert.deepEqual(Array.from(await scrollIn(propagated.ctx, 400)), []);
  assert.equal(propagated.body.steps, 0);
});

// Review finding: only the single largest scroller was traversed; a second independent one was skipped.
test('scrollThrough traverses every independent scroller, largest first, within one step budget', async () => {
  let lazyA = 1;
  let lazyB = 1;
  const { ctx, others, small } = fakeScrollPage({
    more: [
      { tagName: 'ASIDE', id: 'side', clientWidth: 100, clientHeight: 700, content: 2000, grow: (s) => lazyB-- > 0 && (s.scrollHeight += 1000) },
      { tagName: 'SECTION', id: 'feed', clientWidth: 260, clientHeight: 700, content: 2000, grow: (s) => lazyA-- > 0 && (s.scrollHeight += 1000) },
    ],
  });
  assert.deepEqual(Array.from(await scrollIn(ctx, 400)), []);
  const [side, feed] = others;
  assert.equal(feed.loads, 1);
  assert.equal(side.loads, 1);
  assert.ok(feed.order < side.order, 'the larger scroller goes first');
  assert.equal(feed.scrollTop + side.scrollTop, 0);
  assert.equal(small.steps, 0); // scrolls less than a viewport: not traversed

  // The budget is shared: the first endless scroller uses it up, the second is reported, not skipped.
  const endless = fakeScrollPage({
    more: [
      { tagName: 'ASIDE', id: 'side', clientWidth: 100, clientHeight: 700, content: 2000 },
      { tagName: 'SECTION', id: 'feed', clientWidth: 260, clientHeight: 700, content: 2000, grow: (s) => (s.scrollHeight += 1000) },
    ],
  });
  const p = Array.from(await scrollIn(endless.ctx, 5));
  assert.equal(p.length, 2, JSON.stringify(p));
  assert.match(p[0], /^section#feed: stopped after 5 steps/);
  assert.match(p[1], /^aside#side: not traversed/);
});

test('probeExpression is a self-contained expression', () => {
  // It must parse and must not reach module scope: an empty context with no DOM fails on document only.
  assert.doesNotThrow(() => new vm.Script(probeExpression(3)));
  assert.throws(() => vm.runInContext(probeExpression(3), vm.createContext({})), /document is not defined/);
});

// A fake package dir/node_modules/playwright; returns its entry file.
function fakePlaywright(dir) {
  const pkg = path.join(dir, 'node_modules', 'playwright');
  mkdirSync(pkg, { recursive: true });
  writeFileSync(path.join(pkg, 'package.json'), '{"name":"playwright","main":"index.js"}\n');
  writeFileSync(path.join(pkg, 'index.js'), 'module.exports = {};\n');
  return realpathSync(path.join(pkg, 'index.js'));
}

// #135: a trusted caller (agent-jobs ui-check, the orchestrator) never resolves Playwright from the
// current directory, a job worktree it may stand in, nor from a relative NODE_PATH entry.
test('resolvePlaywright: trusted mode skips the current directory', (t) => {
  const tmp = mkdtempSync(path.join(os.tmpdir(), 'ui-check-resolve-'));
  t.after(() => rmSync(tmp, { recursive: true, force: true }));
  const worktree = path.join(tmp, 'worktree');
  const planted = fakePlaywright(worktree);
  const trustedRoot = path.join(tmp, 'global');
  const good = fakePlaywright(trustedRoot);
  const noGlobal = () => '';

  // A job (untrusted) still finds its own node_modules.
  assert.equal(resolvePlaywright(worktree, {}, noGlobal), planted);
  // Trusted: neither the cwd's node_modules nor a relative NODE_PATH entry.
  assert.equal(resolvePlaywright(worktree, {}, noGlobal, { trusted: true }), null);
  assert.equal(resolvePlaywright(worktree, { NODE_PATH: 'node_modules' }, noGlobal, { trusted: true }), null);
  // Trusted: the global root and absolute NODE_PATH entries count.
  const root = path.join(trustedRoot, 'node_modules');
  assert.equal(resolvePlaywright(worktree, {}, () => root, { trusted: true }), good);
  assert.equal(resolvePlaywright(worktree, { NODE_PATH: root }, noGlobal, { trusted: true }), good);
  // An explicit --playwright path wins over everything, and only it is used.
  const explicit = path.join(root, 'playwright');
  assert.equal(resolvePlaywright(worktree, {}, noGlobal, { trusted: true, playwright: explicit }), good);
  assert.equal(resolvePlaywright(worktree, {}, noGlobal, { playwright: explicit }), good);
  assert.equal(resolvePlaywright(worktree, {}, noGlobal, { trusted: true, playwright: path.join(tmp, 'missing') }), null);
  // A relative --playwright is refused in trusted mode (it would name a path inside the worktree).
  assert.equal(resolvePlaywright(worktree, {}, noGlobal, { trusted: true, playwright: 'node_modules/playwright' }), null);
});
