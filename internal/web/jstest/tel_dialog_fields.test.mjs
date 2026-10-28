// Every field the backend sends on a conversation row must be taken when that
// row is refreshed.
//
// telApplyPage keeps the existing row object and copies the fresh values onto
// it one by one. A field left off that list is not merely stale — it is frozen
// at whatever the row was FIRST fetched with, for the life of the app. That is
// what happened to `muted`: Telegram sent the right value, the backend stored
// the right value, and a channel silenced on the phone went on ringing here
// however many times the list was refreshed.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const src = readFileSync(join(here, '..', 'static', 'js', 'tel.js'), 'utf8');
const types = readFileSync(join(here, '..', '..', 'tgbridge', 'types.go'), 'utf8');

let pass = 0, fail = 0;
const ok = (name, cond) => { if (cond) pass++; else { fail++; console.error('FAIL ' + name); } };

// The JSON names on tgbridge.Dialog.
const dialogStruct = types.match(/type Dialog struct \{[\s\S]*?\n\}/)[0];
const fields = [...dialogStruct.matchAll(/`json:"([a-zA-Z]+)[,"]/g)].map(m => m[1]);
ok('found the Dialog fields', fields.length >= 7);

// The else-branch that refreshes a row already held.
const merge = src.match(/var cur = telState\.dialogs\[byKey\[k\]\];[\s\S]*?if \(d\.preview\) cur\.preview = d\.preview;/)[0];

for (const f of fields) {
  // peer is handled by name; the rest must each be assigned.
  const assigned = new RegExp('cur\\.' + f + '\\s*=').test(merge);
  ok(`a refresh takes "${f}"`, assigned);
}

// And the one that was missing, called out by name so the reason survives.
ok('muted specifically', /cur\.muted\s*=\s*d\.muted/.test(merge));

// Run the real merge to prove it, rather than only reading it.
// Drop the first line, which fetches the row out of telState; the test
// supplies `cur` directly.
const body = merge.slice(merge.indexOf('\n') + 1);
const applyMerge = new Function('cur', 'd', body + '\n return cur;');
const row = { peer: { kind: 'channel', id: '1' }, muted: false, unread: 3, pinned: false };
const fresh = { peer: { kind: 'channel', id: '1' }, muted: true, unread: 0, pinned: true,
  topMsg: 9, when: 5, readOutbox: 2, readInbox: 8, preview: 'hi' };
const after = applyMerge(row, fresh);
ok('a channel muted on another device goes quiet here', after.muted === true);
// And unmuting travels too, or a channel un-silenced elsewhere stays silent.
const row2 = Object.assign({}, after);
ok('and unmuting travels back', applyMerge(row2, Object.assign({}, fresh, { muted: false })).muted === false);

console.log(`${pass} passed, ${fail} failed  (${fields.length} fields: ${fields.join(', ')})`);
process.exit(fail ? 1 : 0);
