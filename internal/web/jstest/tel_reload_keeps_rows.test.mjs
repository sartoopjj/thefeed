// "Reload chats" must not shrink the list to one page.
//
// The live answer to a forced reload is Telegram's newest page and nothing
// more. Replacing the list with it dropped every older row from the screen,
// though the backend still held them. Channels and bots post all the time and
// stay in that page; private chats go quiet and fell out, so the People tab
// emptied while Channels and Bots looked fine.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const src = readFileSync(join(here, '..', 'static', 'js', 'tel.js'), 'utf8');

let pass = 0, fail = 0;
function eq(name, got, want) {
  if (JSON.stringify(got) === JSON.stringify(want)) { pass++; return; }
  fail++;
  console.error(`FAIL ${name}\n  got  ${JSON.stringify(got)}\n  want ${JSON.stringify(want)}`);
}

const grab = (name) => src.match(new RegExp('(?:async )?function ' + name + '\\([\\s\\S]*?\\n}'))[0];

function harness(liveAnswer) {
  const telState = { dialogs: [], loadedOnce: false, more: false, total: 0, cursor: null };
  const env = {
    telState,
    telRender() {},
    telOp: async (_k, fn) => fn(),
    telCall: async () => liveAnswer,
    TEL_DIALOG_MAX: 400,
    TEL_DIALOG_PAGE: 40,
  };
  const names = Object.keys(env);
  const body = [grab('telPeerKey'), grab('telApplyPage'), grab('telLoadDialogs')].join('\n') +
    '\nreturn { telApplyPage, telLoadDialogs };';
  const fns = new Function(...names, body)(...names.map((n) => env[n]));
  return { telState, ...fns };
}

// Box ids (private chats, bots) and channel ids are separate counters.
const row = (kind, id, topMsg, when) => ({ peer: { kind, id: String(id) }, topMsg, when, unread: 0 });
const active = [];
for (let i = 0; i < 30; i++) active.push(row('channel', 1000 + i, 90000 - i, 2000 - i));
for (let i = 0; i < 10; i++) active.push(row('bot', 2000 + i, 5000 - i, 1970 - i));
const quiet = [];
for (let i = 0; i < 15; i++) quiet.push(row('user', 3000 + i, 4000 - i, 1500 - i));
const people = (st) => st.dialogs.filter((d) => d.peer.kind === 'user').length;

{
  // The section opened from the cache: everything the backend holds.
  const live = { ok: true, page: { dialogs: active.map((d) => ({ ...d })), more: true, totalKnown: 55,
    nextId: 4991, nextDate: 1961, nextPeer: active[39].peer } };
  const h = harness(live);
  h.telApplyPage({ dialogs: active.concat(quiet).map((d) => ({ ...d })), more: false, totalKnown: 55 }, true);
  eq('from the cache, People lists every private chat', people(h.telState), 15);

  // Menu -> Reload chats: a forced live first page, the 40 newest rows.
  await h.telLoadDialogs(true);
  eq('after Reload chats, People still lists them', people(h.telState), 15);
  eq('and nothing else went missing', h.telState.dialogs.length, 55);
}

{
  // A first load with nothing on screen still shows what the answer brought.
  const h = harness({ ok: true, page: { dialogs: active.map((d) => ({ ...d })), more: true } });
  await h.telLoadDialogs(false);
  eq('a first load fills an empty list', h.telState.dialogs.length, 40);
  eq('and marks the list loaded', h.telState.loadedOnce, true);
}

console.log(`${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
