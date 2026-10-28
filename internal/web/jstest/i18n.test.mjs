// Coverage checks for i18n.js. Zero dependencies — run with:
//   node internal/web/jstest/i18n.test.mjs
// Also wired into `go test ./internal/web` (TestJSI18NCoverage).
//
// Regression under test: t() falls back to the key itself
//   function t(k) { return (I18N[lang] && I18N[lang][k]) || I18N.en[k] || k }
// so a missing key renders as the raw key — and the usual
//   t('update_downloading') || 'Downloading {v}…'
// guard never fires, because the returned key string is truthy. The update
// dialog shipped showing a literal "update_downloading" heading. Any key used
// via t('literal') must therefore exist in BOTH language tables.
import { readFileSync, readdirSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import vm from 'node:vm';

const jsDir = join(dirname(fileURLToPath(import.meta.url)), '..', 'static', 'js');

let failed = 0, passed = 0;
function assert(name, ok, detail) {
  if (ok) { passed++; return; }
  failed++;
  console.error(`FAIL ${name}\n  ${detail}`);
}

// Evaluate only the I18N table; everything after it (the `var lang` IIFE and
// friends) touches localStorage and the DOM at load time.
const src = readFileSync(join(jsDir, 'i18n.js'), 'utf8');
const cut = src.search(/^var lang = /m);
if (cut === -1) throw new Error('i18n.js: `var lang =` boundary not found');
const ctx = vm.createContext({});
vm.runInContext(src.slice(0, cut), ctx, { filename: 'i18n.js' });

const tables = ctx.I18N;
assert('i18n.js exposes I18N', !!tables, 'I18N is undefined');
const langs = Object.keys(tables);
assert('has fa and en tables', langs.includes('fa') && langs.includes('en'), `got ${langs}`);

// --- Every language defines exactly the same keys ---------------------------
const en = new Set(Object.keys(tables.en));
for (const lang of langs) {
  if (lang === 'en') continue;
  const other = new Set(Object.keys(tables[lang]));
  const missing = [...en].filter(k => !other.has(k));
  const extra = [...other].filter(k => !en.has(k));
  assert(`${lang} covers every en key`, missing.length === 0,
    `${lang} is missing: ${missing.join(', ')}`);
  assert(`${lang} has no keys en lacks`, extra.length === 0,
    `en is missing: ${extra.join(', ')}`);
}

// --- No key is defined twice in one table -----------------------------------
// vm evaluation collapses duplicate literal keys (last wins) before
// Object.keys can see them, so this walks the source text instead.
const dupes = [];
const seen = new Map();
{
  const head = src.slice(0, cut);
  let depth = 0, quote = null, table = null, line = 1;
  for (let i = 0; i < head.length; i++) {
    const c = head[i];
    if (c === '\n') line++;
    if (quote) {
      if (c === '\\') i++;
      else if (c === quote) quote = null;
      continue;
    }
    if (c === "'" || c === '"' || c === '`') { quote = c; continue; }
    if (c === '/' && head[i + 1] === '/') { while (i < head.length && head[i] !== '\n') i++; continue; }
    if (c === '/' && head[i + 1] === '*') {
      const end = head.indexOf('*/', i + 2);
      line += (head.slice(i, end + 2).match(/\n/g) || []).length;
      i = end + 1;
      continue;
    }
    if (c === '{') { depth++; continue; }
    if (c === '}') { depth--; if (depth < 2) table = null; continue; }
    const m = /^([A-Za-z_$][\w$]*)\s*:/.exec(head.slice(i, i + 64));
    if (!m) continue;
    if (depth === 1) table = m[1];
    else if (depth === 2 && table) {
      const id = `${table}.${m[1]}`;
      if (seen.has(id)) dupes.push({ key: m[1], id, first: seen.get(id), line });
      else seen.set(id, line);
    }
    i += m[0].length - 1;
  }
}
assert('no key is defined twice in a table', dupes.length === 0,
  dupes.map(d => `${d.id} defined at lines ${d.first} and ${d.line}`).join('\n  '));
// Guards the scanner itself: a broken masker would silently find no keys.
for (const lang of langs) {
  const n = [...seen.keys()].filter(id => id.startsWith(lang + '.')).length;
  assert(`${lang} source scan matches parsed table`, n === Object.keys(tables[lang]).length,
    `scanned ${n}, parsed ${Object.keys(tables[lang]).length}`);
}

// --- Every t('literal') in the app resolves in every language ---------------
// index.html counts too: applyLang() resolves its data-i18n* attributes
// through the same tables, with the same key-as-fallback behaviour.
const unresolved = [];
let scanned = 0;
const scan = (name, text, re) => {
  scanned++;
  text.split('\n').forEach((line, i) => {
    for (const m of line.matchAll(re)) {
      const key = m[1];
      const absent = langs.filter(l => !(key in tables[l]));
      if (absent.length) unresolved.push(`${name}:${i + 1} '${key}' missing in ${absent.join(',')}`);
    }
  });
};
const T_CALL = /\bt\(\s*['"]([\w.]+)['"]\s*\)/g;
for (const f of readdirSync(jsDir)) {
  if (!f.endsWith('.js') || f === 'i18n.js') continue;
  scan(f, readFileSync(join(jsDir, f), 'utf8'), T_CALL);
}
const html = readFileSync(join(jsDir, '..', 'index.html'), 'utf8');
scan('index.html', html, T_CALL);
scan('index.html', html, /data-i18n(?:-ph|-title|-body)?="([\w.]+)"/g);
assert('every t() key is defined', unresolved.length === 0, unresolved.join('\n  '));
// Guards against a silent pass if the tree is reorganised into subfolders.
assert('scanned the frontend', scanned > 10, `only scanned ${scanned} files`);

// --- Keys carrying placeholders must keep them in translation ---------------
// e.g. update_downloading: 'Downloading {v}…' — a translation that drops {v}
// silently loses the version number.
const placeholderMismatch = [];
for (const key of en) {
  const enVal = tables.en[key];
  if (typeof enVal !== 'string') continue;
  const want = (enVal.match(/\{\w+\}/g) || []).sort().join(',');
  if (!want) continue;
  for (const lang of langs) {
    if (lang === 'en') continue;
    const val = tables[lang][key];
    if (typeof val !== 'string') continue;
    const got = (val.match(/\{\w+\}/g) || []).sort().join(',');
    if (got !== want) placeholderMismatch.push(`${lang}.${key}: want ${want || '(none)'}, got ${got || '(none)'}`);
  }
}
assert('placeholders survive translation', placeholderMismatch.length === 0,
  placeholderMismatch.join('\n  '));

// The progress strip is built by concatenating these keys into one line. A
// Latin-script word among them makes the bidi algorithm reorder that line in a
// right-to-left language, which is how "512 B" ended up split across the text
// around it. Units belong in the table, translated, not hardcoded in tel.js.
const stripKeys = [
  'tel_connecting', 'tel_working', 'tel_checking_new', 'tel_attempt',
  'tel_of_queries', 'tel_receiving', 'tel_awaiting_reply', 'tel_err_msg_not_found', 'tel_join', 'tel_step', 'tel_updating_views', 'tel_updating_reactions',
  'tel_phase_discover', 'tel_phase_tunnel', 'tel_phase_telegram',
  'tel_phase_keyexchange', 'tel_phase_session',
  'tel_unit_b', 'tel_unit_kb', 'tel_unit_mb', 'tel_unit_gb',
];
const rtlLangs = ['fa'];
const latinInRTL = [];
for (const key of stripKeys) {
  if (typeof tables.en[key] !== 'string') {
    latinInRTL.push(`en.${key}: missing, so the strip falls back to the raw key`);
    continue;
  }
  for (const lang of rtlLangs) {
    const val = tables[lang] && tables[lang][key];
    if (typeof val !== 'string') {
      latinInRTL.push(`${lang}.${key}: missing`);
      continue;
    }
    // Placeholders are substituted before display, so they are not text.
    const bare = val.replace(/\{\w+\}/g, '');
    const latin = bare.match(/[A-Za-z]+/g);
    if (latin) latinInRTL.push(`${lang}.${key}: Latin text ${JSON.stringify(latin)} in ${JSON.stringify(val)}`);
  }
}
assert('progress strip carries no Latin text in an RTL language', latinInRTL.length === 0,
  latinInRTL.join('\n  '));

console.log(`${passed} passed, ${failed} failed  (${en.size} keys x ${langs.length} langs)`);
// Not process.exit(): stdout is a pipe under `go test`, and exit() drops what
// has not flushed — truncating the very list that explains the failure.
process.exitCode = failed ? 1 : 0;
