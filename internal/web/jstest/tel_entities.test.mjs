// Telegram's own formatting, rendered.
//
// Entities ride along with the message, so this costs nothing on the wire --
// but without it a post written with bold, quotes or spoilers reads as flat
// text and loses the structure its author put there.
//
// The text is attacker-written, so every case here is also an injection case:
// formatting is placed AROUND escaped pieces, never interpolated as markup.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const jsDir = join(here, '..', 'static', 'js');
const tel = readFileSync(join(jsDir, 'tel.js'), 'utf8');
const ui = readFileSync(join(jsDir, 'ui.js'), 'utf8');

let pass = 0, fail = 0;
const eq = (name, got, want) => {
  if (JSON.stringify(got) === JSON.stringify(want)) { pass++; return; }
  fail++;
  console.error(`FAIL ${name}\n  got  ${JSON.stringify(got)}\n  want ${JSON.stringify(want)}`);
};
const ok = (name, cond, detail) => {
  if (cond) { pass++; return; }
  fail++;
  console.error(`FAIL ${name}${detail ? '\n  ' + detail : ''}`);
};

// A DOM shim for esc(), then the real functions out of the real files.
globalThis.document = {
  createElement: () => ({
    _t: '',
    appendChild(n) { this._t += n.t; },
    get innerHTML() { return this._t.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;'); },
  }),
  createTextNode: (t) => ({ t }),
};
const pick = (src, re) => src.match(re)[0];
const env = {};
env.esc = new Function(pick(ui, /function esc\(s\)[^\n]*/) + '; return esc;')();
globalThis.esc = env.esc;
globalThis.escAttr = new Function(pick(ui, /function escAttr\(s\)[^\n]*/) + '; return escAttr;')();
globalThis.linkify = new Function(pick(ui, /function linkify\(raw\) \{[\s\S]*?\n\}/) + '; return linkify;')();
globalThis.telEsc = new Function(pick(tel, /function telEsc\(s\)[^\n]*/) + '; return telEsc;')();
globalThis.telAttrQ = new Function(pick(tel, /function telAttrQ\([\s\S]*?\n\}/) + '; return telAttrQ;')();
globalThis.telFlag = new Function(pick(tel, /function telFlag\([\s\S]*?\n\}/) + '; return telFlag;')();
globalThis.telSafeText = new Function(pick(tel, /function telSafeText\(s\) \{[\s\S]*?\n\}/) + '; return telSafeText;')();
globalThis.telSafeURL = new Function(pick(tel, /function telSafeURL\([\s\S]*?\n\}/) + '; return telSafeURL;')();
const telFormatText = new Function(
  pick(tel, /function telFormatText\(text, entities\) \{[\s\S]*?\n\}\n/) + '; return telFormatText;')();

const E = (type, offset, length, extra) => Object.assign({ type, offset, length }, extra || {});

// --- the formats a channel actually uses ----------------------------------

ok('bold', telFormatText('hello world', [E('bold', 0, 5)]).includes('<b>hello</b>'));
ok('italic', telFormatText('hello world', [E('italic', 6, 5)]).includes('<i>world</i>'));
ok('strike', telFormatText('gone', [E('strike', 0, 4)]).includes('<s>gone</s>'));
ok('underline', telFormatText('line', [E('underline', 0, 4)]).includes('<u>line</u>'));
ok('code', telFormatText('x = 1', [E('code', 0, 5)]).includes('tel-code'));
ok('pre block', telFormatText('body', [E('pre', 0, 4)]).includes('tel-pre'));
ok('spoiler', telFormatText('secret', [E('spoiler', 0, 6)]).includes('tel-spoiler'));

// The newest one, and the reason channels format long posts again.
const quoted = telFormatText('quoted line', [E('blockquote', 0, 11, { collapsed: true })]);
ok('collapsed blockquote', quoted.includes('tel-quote-collapsed'), quoted);
ok('plain blockquote is not clamped',
  !telFormatText('q', [E('blockquote', 0, 1)]).includes('tel-quote-collapsed'));

// --- offsets are UTF-16, as Telegram defines them -------------------------

// An emoji outside the BMP is TWO code units. Getting this wrong shifts every
// following entity, which is how formatting lands on the wrong words.
const withEmoji = '😀 bold';
eq('a non-BMP character counts as two units',
  telFormatText(withEmoji, [E('bold', 3, 4)]).includes('<b>bold</b>'), true);

// --- nothing from the message becomes markup ------------------------------

const payload = '<img src=x onerror=alert(1)>';
const rendered = telFormatText(payload, [E('bold', 0, payload.length)]);
ok('markup in the text is escaped', !rendered.includes('<img'), rendered);
ok('and the formatting still applies', rendered.includes('<b>'), rendered);

// A link entity whose URL is a script scheme must not become an href.
const js = telFormatText('click', [E('text_url', 0, 5, { url: 'javascript:alert(1)' })]);
ok('javascript: never becomes an href', !js.toLowerCase().includes('javascript:'), js);
const good = telFormatText('click', [E('text_url', 0, 5, { url: 'https://example.com/a' })]);
ok('a real link does', good.includes('href="https://example.com/a"'), good);
ok('and opens detached', good.includes('rel="noopener'), good);

// --- it degrades to exactly what it replaced ------------------------------

eq('no entities renders as before',
  telFormatText('plain text', []), telFlag(linkify(telSafeText('plain text'))));
eq('undefined entities too',
  telFormatText('plain text', undefined), telFlag(linkify(telSafeText('plain text'))));

// --- bad input cannot break the render ------------------------------------

for (const bad of [
  E('bold', -5, 3), E('bold', 0, -3), E('bold', 100, 5),
  E('bold', 0, 99999), E('unknown-type', 0, 2),
]) {
  const out = telFormatText('abc', [bad]);
  ok(`out-of-range entity ${bad.type} ${bad.offset}/${bad.length} is survivable`,
    typeof out === 'string' && out.includes('a'), out);
}

// Overlapping entities must not drop text.
const overlap = telFormatText('abcdef', [E('bold', 0, 4), E('italic', 2, 4)]);
for (const ch of 'abcdef') {
  ok(`overlapping entities keep ${ch}`, overlap.includes(ch), overlap);
}

console.log(`${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
