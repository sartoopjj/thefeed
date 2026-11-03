// The sign-in picker: every calling code, names kept out of plain text, the
// picked region (not just its code) remembered.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const js = (f) => readFileSync(join(here, '..', 'static', 'js', f), 'utf8');
const src = js('tel.js'), table = js('telcountries.js'), i18n = js('i18n.js');
const html = readFileSync(join(here, '..', 'static', 'index.html'), 'utf8');

let pass = 0, fail = 0;
const eq = (name, got, want) => {
  if (JSON.stringify(got) === JSON.stringify(want)) { pass++; return; }
  fail++;
  console.error(`FAIL ${name}\n  got  ${JSON.stringify(got)}\n  want ${JSON.stringify(want)}`);
};
const fn = (name) => {
  const m = src.match(new RegExp('function ' + name + '\\([^)]*\\) \\{[\\s\\S]*?\\n\\}\\n'));
  if (!m) throw new Error('missing ' + name);
  return m[0];
};
const top = src.match(/var TEL_DIAL_TOP = \[[^\]]*\];/)[0];

function picker(lang, dial) {
  const state = { dial };
  return new Function('telState', 'lang', 'telEsc', 'telAttrQ',
    table + top + 'var telCountryList = null;' + fn('telCountries') + fn('telCountryName') +
    fn('telDialCountry') + fn('telDialCode') + fn('telDialSelectHTML') +
    'return { all: telCountries(), country: telDialCountry, code: telDialCode, html: telDialSelectHTML, name: telCountryName };',
  )(state, lang, (s) => String(s), (s) => String(s));
}

const en = picker('en', '');
const ids = en.all.map((c) => c.id);
eq('every region decoded', en.all.length >= 240, true);
eq('ids unique', new Set(ids).size, ids.length);
eq('codes well formed', en.all.every((c) => /^\+\d{1,3}$/.test(c.code)), true);
eq('every row named in both languages', en.all.every((c) => c.en && c.fa), true);
const topIds = JSON.parse(top.match(/\[[^\]]*\]/)[0].replace(/'/g, '"'));
eq('the likeliest are all there', topIds.filter((id) => !ids.includes(id)), []);
eq('the default is the first of them', en.country().id, topIds[0]);

// One code, several regions: the pick is the region.
const ca = picker('en', 'ca'), us = picker('en', 'us');
eq('shared code', [ca.code(), us.code()], ['+1', '+1']);
eq('the region picked stays picked', (ca.html().match(/<option value="([a-z]+)" selected>/g) || []), ['<option value="ca" selected>']);
eq('an unknown pick falls back to the default', picker('en', 'zz').country().id, topIds[0]);

// Order: the likeliest first, a divider, then the rest by name.
const vals = [...en.html().matchAll(/<option value="([a-z]+)"/g)].map((m) => m[1]);
eq('likeliest first, in order', vals.slice(0, topIds.length), topIds);
eq('a divider after them', /<option disabled>/.test(en.html()), true);
const rest = vals.slice(topIds.length);
eq('then every other region once', rest.length + topIds.length, ids.length);
const restNames = rest.map((id) => en.name(en.all.find((c) => c.id === id)));
eq('sorted by name', restNames, [...restNames].sort((a, b) => a.localeCompare(b, 'en')));
const fa = picker('fa', '');
const ir = fa.all.find((c) => c.id === topIds[0]);
eq('names follow the language', fa.name(ir) === ir.fa && en.name(ir) === ir.en && ir.fa !== ir.en, true);

// Not plain text in the app: only base64 in the table, none in the strings.
const b64 = table.match(/TEL_COUNTRIES_B64 = '([A-Za-z0-9+/=]+)';/);
eq('the table is one base64 string', !!b64, true);
const word = (n) => new RegExp('(^|[^\\p{L}])' + n.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') + '($|[^\\p{L}])', 'u');
eq('no region name readable in the shipped files', en.all.filter((c) =>
  [src, table, i18n, html].some((f) => word(c.en).test(f) || (c.id !== 'ml' && word(c.fa).test(f)))).map((c) => c.id), []);
// (ml's Persian name is also the everyday word for "financial".)
eq('no old name keys', /tel_country_/.test(i18n), false);
eq('loaded before tel.js', html.indexOf('telcountries.js') > 0 && html.indexOf('telcountries.js') < html.indexOf('/tel.js'), true);

console.log(`tel_dial: ${pass} passed, ${fail} failed`);
if (fail) process.exit(1);
