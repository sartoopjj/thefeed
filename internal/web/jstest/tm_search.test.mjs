// Searching the posts of the open Mirror channel: over what is loaded, no
// request, Persian matched leniently, kept across refreshes, closed on switch.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const src = readFileSync(join(here, '..', 'static', 'js', 'telemirror.js'), 'utf8');
const html = readFileSync(join(here, '..', 'static', 'index.html'), 'utf8');
const i18n = readFileSync(join(here, '..', 'static', 'js', 'i18n.js'), 'utf8');

let pass = 0, fail = 0;
const eq = (name, got, want) => {
  if (JSON.stringify(got) === JSON.stringify(want)) { pass++; return; }
  fail++;
  console.error(`FAIL ${name}\n  got  ${JSON.stringify(got)}\n  want ${JSON.stringify(want)}`);
};
const fn = (name) => {
  const m = src.match(new RegExp('  function ' + name + '\\([^)]*\\) \\{[\\s\\S]*?\\n  \\}\\n'));
  if (!m) throw new Error('missing ' + name);
  return m[0];
};

// A fake #tmContent with posts, enough for the filter.
function page(texts) {
  const posts = texts.map((t, i) => ({ pid: 'tmp_' + i, style: { display: '' }, getAttribute: () => 'tmp_' + i, textContent: t }));
  const extra = [];
  const box = {
    querySelectorAll: () => posts,
    appendChild: (el) => { extra.push(el); el.remove = () => extra.splice(extra.indexOf(el), 1); },
  };
  const doc = {
    getElementById: (id) => id === 'tmContent' ? box : (id === 'tmSearchNone' ? extra[0] || null : null),
    createElement: () => ({}),
  };
  const postText = {};
  texts.forEach((t, i) => { postText['tmp_' + i] = t; });
  const api = new Function('document', 'tmPostText', 'tmI18n',
    'var tmSearchQ = "";' + fn('tmNormSearch') + fn('tmApplySearch') +
    '; return { set: function (q) { tmSearchQ = q; tmApplySearch(); } };',
  )(doc, postText, (k, d) => d);
  return { api, posts, extra, visible: () => posts.filter((p) => p.style.display !== 'none').map((p) => p.textContent) };
}

{
  const p = page(['Meteor shower peaks tonight', 'New deep-field images', 'می‌شود فردا دید', 'كتاب جديد']);
  p.api.set('METEOR');
  eq('case does not matter', p.visible(), ['Meteor shower peaks tonight']);
  p.api.set('میشود');
  eq('the zero-width non-joiner does not matter', p.visible(), ['می‌شود فردا دید']);
  p.api.set('کتاب');
  eq('Arabic and Persian kaf/yeh are the same letter', p.visible(), ['كتاب جديد']);
  p.api.set('nothing like this');
  eq('no match: everything hidden', p.visible(), []);
  eq('and it says so', p.extra.length === 1 && p.extra[0].textContent, 'No posts match');
  p.api.set('');
  eq('cleared: everything back', p.visible().length, 4);
  eq('and the "no match" line goes', p.extra.length, 0);
}

eq('no request: the filter only reads the page', /fetch\(/.test(fn('tmApplySearch')), false);
eq('every render keeps the open search', /content\.innerHTML = html;[\s\S]{0,120}tmApplySearch\(\);/.test(src), true);
eq('switching channels closes it', /if \(tmActive !== username\) tmCloseSearch\(\);/.test(src), true);
eq('so does going back to the list', /function tmBackToList\(\) \{[^}]*tmCloseSearch\(\);\n  \}/.test(src), true);
eq('and opening Mirror again', /window\.openTelemirror = function[\s\S]{0,400}?tmCloseSearch\(\);/.test(src), true);
eq('the button is in the Mirror header', /id="tmSearchBtn" onclick="tmToggleSearch\(\)"/.test(html), true);
eq('and the box under it', /id="tmSearchInput"[\s\S]*oninput="tmFilterPosts\(this\.value\)"/.test(html), true);
eq('Escape closes it', /id="tmSearchInput"[^>]*onkeydown="if\(event\.key==='Escape'\)\{event\.preventDefault\(\);tmToggleSearch\(\)\}"/.test(html), true);
for (const k of ['telemirror_search', 'telemirror_search_none']) {
  eq(k + ' in fa and en', (i18n.match(new RegExp('\\b' + k + ':', 'g')) || []).length, 2);
}

console.log(`tm_search: ${pass} passed, ${fail} failed`);
if (fail) process.exit(1);
