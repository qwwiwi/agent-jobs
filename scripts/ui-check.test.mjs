// Static tests of scripts/ui-check.mjs (node --test; run by ui_check_test.go from scripts/check.sh).
// No browser starts here: the pure helpers are tested directly, and the in-page probe runs against a
// small fake DOM in a vm context, through the same expression the script hands to page.evaluate.
import assert from 'node:assert/strict';
import { test } from 'node:test';
import vm from 'node:vm';

import {
  CLS_LIMIT, DEVICES, UsageError, clsFromShifts, countLines, failuresOf, parseArgs, probeExpression, slug, sticksOut,
  wordSegments,
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

// A fake DOM: a table cell whose word "Configuration" renders one letter per line (the incident), a cell
// with a word on one line, a hyphens:auto paragraph, a script, a box sticking out of the page, one inside a
// scroll container, a broken image and a hidden one.
function fakeDOM() {
  const vw = 360;
  const rect = (left, right, top = 0, h = 20) => ({ left, right, top, bottom: top + h, width: right - left, height: h });
  const el = (tagName, parent, opts = {}) => ({
    tagName, id: opts.id || '', className: opts.className || '', parentElement: parent, style: opts.style || {},
    box: opts.box || rect(0, 100),
    getBoundingClientRect() { return this.box; },
    contains(o) { for (let p = o; p; p = p.parentElement) if (p === this) return true; return false; },
  });
  const html = el('HTML', null);
  const body = el('BODY', html);
  const table = el('TABLE', body);
  const td1 = el('TD', table, { className: 'price wide' });
  const td2 = el('TD', table);
  const para = el('P', body, { style: { hyphens: 'auto' } });
  const script = el('SCRIPT', body);
  const wide = el('DIV', body, { id: 'banner', box: rect(0, 500) });
  const wideChild = el('SPAN', wide, { box: rect(0, 480) });
  const scroller = el('DIV', body, { style: { overflowX: 'auto' } });
  const inScroller = el('PRE', scroller, { box: rect(0, 900) });
  const elements = [table, td1, td2, para, script, wide, wideChild, scroller, inScroller];
  // Each text node says how a [start, end) range of it renders.
  const perLetter = (s, e) => Array.from({ length: e - s }, (_, i) => rect(0, 8, i * 20));
  const oneLine = (s, e) => [rect(s * 8, e * 8)];
  const texts = [
    { data: 'Configuration', parentElement: td1, rects: perLetter },
    { data: 'Short words', parentElement: td2, rects: oneLine },
    { data: 'Internationalization', parentElement: para, rects: perLetter },
    { data: 'var longidentifier', parentElement: script, rects: perLetter },
  ];
  const img = (src, complete, naturalWidth, rendered = true) => ({
    currentSrc: src, complete, naturalWidth, getAttribute: () => src, getClientRects: () => (rendered ? [rect(0, 10)] : []),
  });
  const document = {
    documentElement: { clientWidth: vw, scrollWidth: 500, scrollHeight: 3000 },
    body: { ...body, querySelectorAll: () => elements },
    images: [img('/ok.png', true, 10), img('/missing.png', true, 0), img('/lazy-hidden.png', false, 0, false)],
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
  const getComputedStyle = (e) => ({ overflowX: 'visible', visibility: 'visible', opacity: '1', hyphens: 'manual', ...e.style });
  return { document, getComputedStyle };
}

test('pageProbe finds split words, stick-outs and broken images in a fake DOM', () => {
  const ctx = vm.createContext(fakeDOM());
  // Serialized like page.evaluate's result (this also drops the vm realm's prototypes).
  const out = JSON.parse(JSON.stringify(vm.runInContext(probeExpression(5), ctx)));
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

test('probeExpression is a self-contained expression', () => {
  // It must parse and must not reach module scope: an empty context with no DOM fails on document only.
  assert.doesNotThrow(() => new vm.Script(probeExpression(3)));
  assert.throws(() => vm.runInContext(probeExpression(3), vm.createContext({})), /document is not defined/);
});
