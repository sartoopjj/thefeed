// A Telegram message is attacker-controlled input: anyone can send one. These
// check that nothing in it can become code.
//   node internal/web/jstest/tel_xss.test.mjs
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const jsDir = join(dirname(fileURLToPath(import.meta.url)), '..', 'static', 'js');
const src = readFileSync(join(jsDir, 'tel.js'), 'utf8');

let failed = 0, passed = 0;
function check(name, ok, detail) {
  if (ok) { passed++; return; }
  failed++;
  console.error(`FAIL ${name}\n  ${detail}`);
}

// telSafeURL is self-contained, so lift it out rather than loading the whole
// file (which wants a DOM).
const fn = src.match(/function telSafeURL\(u\) \{[\s\S]*?\n\}/)[0];
const telSafeURL = new Function(fn + '; return telSafeURL;')();

const blocked = [
  'javascript:alert(1)',
  'JaVaScRiPt:alert(1)',
  '  javascript:alert(1)',
  'java\tscript:alert(1)',
  'java\nscript:alert(1)',
  'java\u0000script:alert(1)',
  'data:text/html,<script>alert(1)</script>',
  'vbscript:msgbox(1)',
  'file:///etc/passwd',
  '//evil.example.com',
  '/relative/path',
  'blob:https://x/y',
];
for (const u of blocked) {
  check(`blocks ${JSON.stringify(u)}`, telSafeURL(u) === '',
    `returned ${JSON.stringify(telSafeURL(u))} — a tap would run it`);
}

const allowed = [
  'https://example.com/x?y=1#z',
  'http://example.com',
  'tg://resolve?domain=telegram',
  'HTTPS://EXAMPLE.COM',
];
for (const u of allowed) {
  check(`allows ${JSON.stringify(u)}`, telSafeURL(u) === u.trim(),
    `returned ${JSON.stringify(telSafeURL(u))}`);
}

// Direction overrides: the filename-spoof trick official clients neutralise.
const fnText = src.match(/function telSafeText\(s\) \{[\s\S]*?\n\}/)[0];
const telSafeText = new Function(fnText + '; return telSafeText;')();
const spoof = 'photo_\u202Egnp.exe';
check('strips the right-to-left override', !telSafeText(spoof).includes('\u202E'),
  `kept the override: ${JSON.stringify(telSafeText(spoof))}`);
for (const ch of ['\u202A', '\u202B', '\u202C', '\u202D', '\u202E',
                  '\u2066', '\u2067', '\u2068', '\u2069']) {
  check(`strips ${JSON.stringify(ch)}`, telSafeText('a' + ch + 'b') === 'ab',
    `got ${JSON.stringify(telSafeText('a' + ch + 'b'))}`);
}
check('leaves ordinary text alone', telSafeText('سلام hello 123') === 'سلام hello 123');
check('a message with an override is cleaned before display',
  /\((telEsc|linkify)\(telSafeText\(m\.text\)\)/.test(src) || src.includes('linkify(telSafeText(m.text))'),
  'message text is not cleaned');
check('a media name with an override is cleaned',
  src.includes('telSafeText(m.mediaName)'), 'media names are not cleaned');
check('a button shows its real target',
  /title="' \+\s*telAttrQ\(href\)/.test(src), 'the href is not shown as the title');

check('empty and missing are harmless',
  telSafeURL('') === '' && telSafeURL(null) === '' && telSafeURL(undefined) === '');

// Every place a message's own fields reach the DOM must escape them.
// Every message-borne string must pass through telEsc somewhere, with or
// without the direction cleaner wrapped inside it.
for (const [what, re] of [
  // linkify() escapes everything it does not turn into a link, and its regex
  // only matches http(s), so no scheme can ride in. It is the feed's, shared
  // so both sections handle links the same way.
  ['message text', /(telEsc|linkify)\((telSafeText\()?m\.text/],
  ['button label', /telEsc\((telSafeText\()?b\.text/],
  ['media label', /telEsc\((telSafeText\()?label/],
]) {
  check(`escaped: ${what}`, re.test(src), 'escaping was removed');
}
check('no raw href from a message', !/href="' \+ telAttrQ\(b\.url\)/.test(src),
  'a button URL is used without a scheme check');

// linkify is now on the message path, so its own rule matters: it only turns
// http(s) into links. A scheme it does not match is escaped as text.
const ui = readFileSync(join(jsDir, 'ui.js'), 'utf8');
const linkRe = ui.match(/function linkify\(raw\)[\s\S]*?var re = (\/.*?\/g);/);
check('linkify only linkifies http(s)', !!linkRe && !/javascript|data:|vbscript/i.test(linkRe[1]),
  `its pattern is ${linkRe ? linkRe[1] : 'not found'}`);
check('linkify requires an explicit http(s) scheme', !!linkRe && linkRe[1].includes('https?:'),
  'a schemeless match would resolve against our own origin');

// ---- the lion and sun -------------------------------------------------------
// The flag swap inserts an <img>, so it MUST run on already-escaped text. Run
// it on raw text and a message containing the flag could smuggle markup
// through: the escaping would have nothing left to escape.
{
  const fn = src.match(/function telFlag\([\s\S]*?\n}/)[0];
  const telFlag = new Function(fn + '; return telFlag;')();
  const FLAG = '\u{1F1EE}\u{1F1F7}';

  check('flag becomes the lion and sun',
    telFlag(FLAG).includes('/static/lion-sun.svg'));
  check('the flag itself is gone', !telFlag(FLAG).includes(FLAG));
  check('text without the flag is untouched', telFlag('hello') === 'hello');
  check('every occurrence is swapped, not just the first',
    telFlag(FLAG + 'x' + FLAG).match(/lion-sun\.svg/g).length === 2);

  // telFlag does not escape: it is applied after escaping, never instead of
  // it. Pinning this stops someone "simplifying" it into the escape path.
  check('telFlag is not an escaper — it must stay downstream of one',
    telFlag('<b>') === '<b>');

  // The real call sites: escaped (or linkified, which escapes) first.
  for (const call of [
    'telFlag(telEsc(d.peer.title || d.peer.username || key))',
    "telFlag(telEsc(d.preview || ''))",
    "telFlag(telEsc(peer.title || peer.username || ''))",
    'telFlag(telEsc(telSafeText(text)))',
    'telFlag(linkify(telSafeText(m.text)))',
  ]) {
    check(`flag swap runs on escaped text: ${call.slice(0, 40)}`, src.includes(call));
  }
  // Nothing may hand telFlag raw, unescaped input.
  check('telFlag is never given raw message text',
    !/telFlag\(\s*m\.text/.test(src) && !/telFlag\(\s*telSafeText\(/.test(src));
}


// The conversation picture reaches an attribute, so it is escaped like every
// other value from Telegram. Base64 cannot carry a quote, but nothing here is
// trusted on the grounds of what it is supposed to contain.
const avatarFn = src.match(/function telAvatar\([\s\S]*?\n}/)[0];
check('avatar picture goes through attribute escaping',
  /telAttrQ\(thumb\)/.test(avatarFn));
check('and falls back to the initial when there is none',
  avatarFn.indexOf('if (thumb)') < avatarFn.indexOf('charAt(0)'));

console.log(`${passed} passed, ${failed} failed`);
process.exitCode = failed ? 1 : 0;