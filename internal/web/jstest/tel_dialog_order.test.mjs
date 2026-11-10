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
    telState, telRender() {}, TEL_DIALOG_MAX: 400, TEL_PENDING_BASE: 1e12,
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
  // Pins in the user's order (Telegram's list order), not by time; one with
  // no known order after those that have one.
  const h = harness();
  h.telApplyPage({ dialogs: [
    row('user', 1, 10, 5000, { pinned: true, pinOrder: 2 }),
    row('channel', 2, 10, 100, { pinned: true, pinOrder: 1 }),
    row('bot', 3, 10, 9000, { pinned: true }),
    row('user', 4, 10, 9999),
  ] }, true);
  eq('pins in pin order, then by time', order(h.telState), ['channel:2', 'user:1', 'bot:3', 'user:4']);
  // A refreshed page changes the order.
  h.telApplyPage({ dialogs: [row('user', 1, 10, 5000, { pinned: true, pinOrder: 1 }),
    row('channel', 2, 10, 100, { pinned: true, pinOrder: 2 })] }, false);
  eq('a re-pin on the phone shows after the next list read', order(h.telState).slice(0, 2), ['user:1', 'channel:2']);
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

{
  // The phone's clock is a day ahead. The message is drawn at once with that
  // time, but when Telegram answers the send it says its own time, and that
  // is the one the list is ordered by -- otherwise the chat sat on top of the
  // list, over newer ones, until the next list read.
  const h = harness();
  const now = 1_800_000_000;
  h.telApplyPage({ dialogs: [row('channel', 1, 90000, now + 60), row('user', 2, 4000, now - 600)] }, true);
  const pending = { id: 1e12 + 1, date: now + 86400, text: 'hi', out: true, pending: true };
  const th = { msgs: [{ id: 4000, date: now - 600, text: 'old' }, pending] };
  const settle = src.match(/if \(r\.msgId\) \{[\s\S]*?return true;\n    \}/)[0];
  new Function('r', 'pending', 'th', 'key', 'telTouchPreview', settle + '\nreturn false;')(
    { ok: true, msgId: 4001, date: now + 30 }, pending, th, 'user:2', h.telTouchPreview);
  eq('the sent message takes Telegram\'s time', pending.date, now + 30);
  eq('and the row too, not the phone\'s', h.telState.dialogs.find((d) => d.peer.id === '2').when, now + 30);
  eq('so a newer chat stays above it', order(h.telState), ['channel:1', 'user:2']);
}

{
  // A send that failed stays on screen with the phone's time. A later fetch
  // touching the row must order it by the newest REAL message, not by that.
  const h = harness();
  const now = 1_800_000_000;
  h.telApplyPage({ dialogs: [row('channel', 1, 90000, now + 60), row('user', 2, 4000, now - 600)] }, true);
  const th = { msgs: [{ id: 4000, date: now - 600, text: 'old' }, { id: 4002, date: now - 10, text: 'new from them' },
    { id: 1e12 + 2, date: now + 86400, text: 'failed', out: true, failed: true }] };
  h.telTouchPreview('user:2', th);
  eq('a failed message\'s phone time does not order the list', h.telState.dialogs.find((d) => d.peer.id === '2').when, now - 10);
  eq('the newer chat stays above', order(h.telState), ['channel:1', 'user:2']);
}

{
  // A chat unpinned on the phone that has aged off the first page: the page
  // does not mention it, yet every pin is on the first page -- so it is not
  // pinned any more. A later page says nothing about pins.
  const h = harness();
  h.telApplyPage({ dialogs: [row('user', 1, 10, 100, { pinned: true, pinOrder: 1 }), row('user', 2, 10, 5000)] }, true, true);
  h.telApplyPage({ dialogs: [row('user', 2, 10, 5000)] }, false, true);
  const d1 = h.telState.dialogs.find((d) => d.peer.id === '1');
  eq('a first page without it unpins it', [d1.pinned, d1.pinOrder || 0], [false, 0]);
  eq('so it drops to its place by time', order(h.telState), ['user:2', 'user:1']);
  h.telApplyPage({ dialogs: [row('user', 1, 10, 100, { pinned: true, pinOrder: 1 })] }, false, true);
  h.telApplyPage({ dialogs: [row('user', 9, 10, 50)] }, false, false);
  eq('a later page leaves pins alone', !!h.telState.dialogs.find((d) => d.peer.id === '1').pinned, true);
}
for (const fn of ['telPrimeFromCache', 'telLoadDialogs', 'telRefreshDialogs'])
  eq(`${fn} applies a FIRST page`, /telApplyPage\(r\.page, (true|false), true\)/.test(grab(fn)), true);
eq('load-more applies a later page', /telApplyPage\(r\.page, false\)/.test(grab('telLoadMoreDialogs')), true);
{
  // A channel opened by @username or a link, not joined: the row is there to
  // come back to, so it sits right under the pins -- newest visit first --
  // even with no messages at all. By time, an empty one went to the very
  // bottom of the list, where nobody would find it.
  const h = harness();
  h.telApplyPage({ dialogs: [row('user', 1, 10, 5000), row('channel', 2, 10, 100, { pinned: true, pinOrder: 1 })] }, true);
  h.telState.dialogs.unshift(row('channel', 8, 0, 0, { visiting: true, visitedAt: 100 }));
  h.telState.dialogs.unshift(row('channel', 9, 0, 0, { visiting: true, visitedAt: 200 }));
  h.telApplyPage({ dialogs: [] }, false);
  eq('viewed, not joined: under the pins, newest visit first', order(h.telState), ['channel:2', 'channel:9', 'channel:8', 'user:1']);
  // In the account's own list now: joined (here or elsewhere), so an ordinary row.
  h.telApplyPage({ dialogs: [row('channel', 9, 50, 50)] }, false);
  const joined = h.telState.dialogs.find((d) => d.peer.id === '9');
  eq('a row the account lists is no longer "viewing"', !!joined.visiting, false);
  eq('and takes its place by time', order(h.telState), ['channel:2', 'channel:8', 'user:1', 'channel:9']);
}
for (const fn of ['telOpenByUsername', 'telStartNewChat']) {
  eq(`${fn} stamps the visit`, /visiting: true, visitedAt: Date\.now\(\)/.test(grab(fn)), true);
  eq(`${fn}: opening it again moves it back up`, /\.visiting\) \w+\.visitedAt = Date\.now\(\)/.test(grab(fn)), true);
}

eq('one sort, used by both', (src.match(/telSortDialogs\(\)/g) || []).length >= 2, true);
eq('time is the key, the id only breaks a tie',
  /return \(\(b\.when \|\| 0\) - \(a\.when \|\| 0\)\) \|\| /.test(grab('telSortDialogs')), true);
eq('no other sort of the list', (src.match(/telState\.dialogs\.sort\(/g) || []).length, 1);

console.log(`${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
