// Every cache the backend reports has to be presentable.
//
// The storage screen looks each cache up in three tables: a translation key, an
// icon and a colour. A cache id missing from them is not a blank space — the
// label falls back to the server's English string (so a Persian user reads
// English), the tile shows an empty coloured square, and the bar turns grey.
// "telegram" was added to the backend and to none of the three.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const js = readFileSync(join(here, '..', 'static', 'js', 'settings.js'), 'utf8');
const go = readFileSync(join(here, '..', 'cache.go'), 'utf8');
const i18n = readFileSync(join(here, '..', 'static', 'js', 'i18n.js'), 'utf8');

let pass = 0, fail = 0;
const ok = (name, cond) => { if (cond) pass++; else { fail++; console.error('FAIL ' + name); } };

const ids = [...go.matchAll(/cacheEntry\{ID:\s*"([^"]+)"/g)].map(m => m[1]);
ok('the backend reports at least one cache', ids.length > 0);

const table = name => {
  const body = js.match(new RegExp(`var ${name} = \\{([\\s\\S]*?)\\n\\};`))[1];
  return Object.fromEntries([...body.matchAll(/'([a-z-]+)':\s*('[^']*'|"[^"]*"|[^,\n]+)/g)]
    .map(m => [m[1], m[2]]));
};
const labels = table('_storageLabelMap');
const icons = table('_storageIconMap');
const colours = table('_storageColorMap');

for (const id of ids) {
  ok(`"${id}" has a translated label`, !!labels[id]);
  ok(`"${id}" has an icon`, !!icons[id] && icons[id].includes('<svg'));
  ok(`"${id}" has a colour`, !!colours[id]);
  // The label must be a real i18n key, in both languages, or it renders as
  // the key itself.
  if (labels[id]) {
    const key = labels[id].replace(/['"]/g, '');
    const uses = (i18n.match(new RegExp('\\b' + key + ':', 'g')) || []).length;
    ok(`"${id}" label key ${key} is defined in both languages`, uses === 2);
  }
}

console.log(`${pass} passed, ${fail} failed  (${ids.length} caches: ${ids.join(', ')})`);
process.exit(fail ? 1 : 0);
