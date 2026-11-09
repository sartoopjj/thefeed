// The member count under a group's or channel's name in the open chat.
//
// The backend answers it from its cache (never the link); opening the chat is
// what asks Telegram, once a day. The page reads it on open and again when
// the backend says a count arrived.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const src = readFileSync(join(here, '..', 'static', 'js', 'tel.js'), 'utf8');
const sse = readFileSync(join(here, '..', 'static', 'js', 'sse.js'), 'utf8');

let pass = 0, fail = 0;
function eq(name, got, want) {
  if (JSON.stringify(got) === JSON.stringify(want)) { pass++; return; }
  fail++;
  console.error(`FAIL ${name}\n  got  ${JSON.stringify(got)}\n  want ${JSON.stringify(want)}`);
}
const grab = (name) => src.match(new RegExp('(?:async )?function ' + name + '\\([\\s\\S]*?\\n}'))[0];

const words = {
  tel_kind_channel: 'Channel', tel_kind_group: 'Group', tel_kind_forum: 'Forum', tel_kind_user: 'Person',
  tel_subscribers_n: '{n} subscribers', tel_members_n: '{n} members',
  tel_subscribers_one: '1 subscriber', tel_members_one: '1 member',
};
const telState = { members: {} };
const mk = new Function('telState', 'telT', 'telFmtCount', 'telPeerKey', 'telEsc',
  grab('telMembersLine') + '\n' + grab('telThreadSub') + '\nreturn { telMembersLine, telThreadSub };');
const { telThreadSub } = mk(telState, (k) => words[k] || k,
  new Function(grab('telFmtCount') + '\nreturn telFmtCount;')(),
  (p) => p.kind + ':' + p.id,
  (x) => String(x).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;'));

// The line is one line with an ellipsis, so its END is what a phone cuts.
// The member count goes first (most users are on phones, and it is what they
// open the header for); the @id after it. A known count stands in for the
// kind word ("subscribers" already says it is a channel).
const text = (html) => html.replace(/<[^>]*>/g, '');
const chan = { kind: 'channel', id: '9', username: 'news' };
eq('a channel with no count yet', text(telThreadSub(chan)), 'Channel · @news');
telState.members['channel:9'] = 12345;
eq('the count comes first, the id after', text(telThreadSub(chan)), '12.3K subscribers · @news');
eq('the id is isolated left-to-right inside a Persian line',
  /<bdi dir="ltr">@news<\/bdi>$/.test(telThreadSub(chan)), true);
const grp = { kind: 'group', id: '5', members: 42 };
eq('a group counts members, from the list row', text(telThreadSub(grp)), '42 members');
eq('one member', text(telThreadSub({ kind: 'group', id: '6', members: 1 })), '1 member');
telState.members['channel:10'] = 0;
eq('a hidden count shows the kind', text(telThreadSub({ kind: 'channel', id: '10' })), 'Channel');
eq('a person has no count', text(telThreadSub({ kind: 'user', id: '1', members: 3, username: 'bob' })), 'Person · @bob');
eq('a username cannot inject markup', /<b>/.test(telThreadSub({ kind: 'user', id: '1', username: '<b>x' })), false);
// The line takes the UI's direction, not the first letter's: with the Latin
// @id first, dir="auto" turned a Persian line left-to-right.
eq('the header line follows the UI direction', /class="tel-thread-sub" dir="' \+ telUIDir\(\)/.test(src), true);

// Read from the cache on open, and when the backend announces a new count.
const open = grab('telOpenPeer');
eq('opening a chat reads its count', /telLoadMembers\(/.test(open), true);
const load = grab('telLoadMembers');
eq('the count is read from the cache endpoint', /\/api\/tel\/members\?/.test(load), true);
eq('without taking the link', /telOp\(|telBgOp\(|telCall\(/.test(load), false);
eq('the backend does not fetch it behind the page\'s back', /tel-members/.test(sse), false);
eq('a new account starts with no counts', /telState\.members = \{\}/.test(grab('telResetData')) &&
  /telState\.membersFresh = \{\}/.test(grab('telResetData')), true);
{
  // The cache read keeps what is on screen but says whether it is current.
  const st = { peer: { kind: 'channel', id: '9' }, members: { 'channel:9': 10 }, membersFresh: { 'channel:9': true } };
  let answer = { ok: true, known: false, members: 0 };
  const load = new Function('telState', 'telPeerKey', 'telFetch', 'telRender', grab('telLoadMembers') + '\nreturn telLoadMembers;')(
    st, (p) => p.kind + ':' + p.id, async () => answer, () => {});
  await load();
  eq('expired in the backend: still shown, no longer fresh', [st.members['channel:9'], st.membersFresh['channel:9']], [10, false]);
  answer = { ok: true, known: true, members: 12 };
  await load();
  eq('current again: replaced and fresh', [st.members['channel:9'], st.membersFresh['channel:9']], [12, true]);
}

console.log(`${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
