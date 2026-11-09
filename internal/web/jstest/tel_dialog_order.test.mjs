// The conversation list in Telegram's order: pinned first, then by the time
// of the newest message.
//
// It was sorted by the newest message's ID. Private chats (and bots and basic
// groups) share one id counter, and every channel has its own, so ids from
// the two cannot be compared: a channel at post 90 000 sat above a private
// chat written to a minute ago at message 4 000.
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

function harness() {
  const telState = { dialogs: [], more: false, total: 0, cursor: null };
  const env = {
    telState, telRender() {}, TEL_DIALOG_MAX: 400,
    telNewestRealID: (th) => Math.max(...th.msgs.filter((m) => !m.pending).map((m) => m.id)),
  };
  const names = Object.keys(env);
  const body = ['telPeerKey', 'telSortDialogs', 'telApplyPage', 'telDialogFor', 'telTouchPreview'].map(grab).join('\n') +
    '\nreturn { telApplyPage, telTouchPreview };';
  return { telState, ...new Function(...names, body)(...names.map((n) => env[n])) };
}
const row = (kind, id, topMsg, when, extra) => ({ peer: { kind, id: String(id) }, topMsg, when, unread: 0, ...extra });
const order = (st) => st.dialogs.map((d) => d.peer.kind + ':' + d.peer.id);

{
  const h = harness();
  h.telApplyPage({ dialogs: [
    row('channel', 1, 90000, 1000),   // busy channel, high id, posted earlier
    row('user', 2, 4000, 2000),       // private chat, low id, written to later
    row('bot', 3, 4100, 1500),
    row('channel', 4, 50, 3000),      // young channel, tiny id, newest of all
  ] }, true);
  eq('newest message first, whatever its id', order(h.telState), ['channel:4', 'user:2', 'bot:3', 'channel:1']);
}
{
  const h = harness();
  h.telApplyPage({ dialogs: [
    row('user', 1, 10, 5000),
    row('channel', 2, 10, 100, { pinned: true }),
  ] }, true);
  eq('pinned still first', order(h.telState), ['channel:2', 'user:1']);
}
{
  const h = harness();
  h.telApplyPage({ dialogs: [row('user', 1, 10, 0), row('user', 2, 20, 900), row('user', 3, 30, 900)] }, true);
  eq('same second: the later message first; no time: last', order(h.telState), ['user:3', 'user:2', 'user:1']);
}
{
  // A message written here moves its conversation to the top at once, as in
  // Telegram, without waiting for the next list fetch.
  const h = harness();
  h.telApplyPage({ dialogs: [row('channel', 1, 90000, 2000), row('user', 2, 4000, 1000)] }, true);
  const th = { msgs: [{ id: 4000, date: 1000, text: 'old' }, { id: 4001, date: 3000, text: 'just sent' }] };
  h.telTouchPreview('user:2', th);
  eq('a message sent here moves the chat up', order(h.telState), ['user:2', 'channel:1']);
}

eq('one sort, used by both', (src.match(/telSortDialogs\(\)/g) || []).length >= 2, true);
eq('time is the key, the id only breaks a tie',
  /return \(\(b\.when \|\| 0\) - \(a\.when \|\| 0\)\) \|\| /.test(grab('telSortDialogs')), true);
eq('no other sort of the list', (src.match(/telState\.dialogs\.sort\(/g) || []).length, 1);

console.log(`${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
