// Every round trip shows the progress strip, the quiet ones included.
//
// The ad fetch runs outside telOp/telBgOp on purpose (it must never hold up
// what the user does, and anything they do cancels it), and the member count
// ran inside the backend's own refresh. Neither drew the strip, so both spent
// queries with nothing on screen.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const src = readFileSync(join(here, '..', 'static', 'js', 'tel.js'), 'utf8');
const i18n = readFileSync(join(here, '..', 'static', 'js', 'i18n.js'), 'utf8');

let pass = 0, fail = 0;
function eq(name, got, want) {
  if (JSON.stringify(got) === JSON.stringify(want)) { pass++; return; }
  fail++;
  console.error(`FAIL ${name}\n  got  ${JSON.stringify(got)}\n  want ${JSON.stringify(want)}`);
}
const grab = (name) => src.match(new RegExp('(?:async )?function ' + name + '\\([\\s\\S]*?\\n}'))[0];

// --- the strip itself -------------------------------------------------------

// Runs the real telPaintProgress against a stand-in strip.
function paint(state) {
  let shown = null;
  const label = { textContent: '' };
  const wrap = {
    classList: { toggle() {} },
    querySelector: (s) => (s === '.tel-prog-label' ? label : s === '.tel-prog-fill' ? { style: {} } : null),
  };
  const document = { querySelectorAll: (s) => (s === '.tel-prog-wrap' ? [wrap] : []) };
  new Function('telState', 'telT', 'telFmtBytes', 'telSafeText', 'telPhaseLabel', 'telProgShow', 'document',
    grab('telPaintProgress') + '\ntelPaintProgress();')(
    state, (k) => '[' + k + ']', String, String, () => '[phase]', (on) => { shown = on; }, document);
  return { shown, label: label.textContent };
}

{
  const p = paint({ info: { connected: true }, adBusy: true });
  eq('an ad fetch puts the strip up', p.shown, true);
  eq('under its own name', p.label.startsWith('[tel_loading_ad]'), true);
}
{
  // A real operation outranks it: the ad is about to be cancelled anyway.
  const p = paint({ info: { connected: true }, adBusy: true, busy: true, busyLabel: 'tel_sending' });
  eq('an operation names itself, not the ad', p.label.startsWith('[tel_sending]'), true);
}
eq('idle stays down', paint({ info: { connected: true } }).shown, false);

// The live query counter is re-read only while something runs.
eq('the counter keeps moving during an ad fetch', /telState\.adBusy/.test(grab('telTick')), true);

// --- ads: visible, yet still never in the way --------------------------------

const load = grab('telLoadAds');
eq('the ad fetch still never takes the link through telOp/telBgOp', /telOp\(|telBgOp\(/.test(load), false);
{
  const telState = { peer: { kind: 'channel', id: '9' } };
  let seen = null;
  let release;
  const gate = new Promise((r) => { release = r; });
  const run = new Function('telState', 'telThread', 'telPeerKey', 'telRender', 'telAdCheckView',
    'telFetch', 'telPost', 'telPaintProgress',
    'var telAdCtl = null;' + grab('telAdAbort') + load + '\nreturn { telLoadAds, telAdAbort };');
  const fns = run(telState, () => ({}), (p) => p.kind + ':' + p.id, () => {}, () => {},
    async () => { seen = telState.adBusy; await gate; return { ok: true, ads: [] }; },
    (b) => b, () => {});
  const done = fns.telLoadAds('channel:9', telState.peer);
  await Promise.resolve();
  eq('marked as loading while in flight', seen, true);
  // Anything the user does cancels it, and the strip has to go with it.
  fns.telAdAbort();
  eq('cancelling clears the mark', !!telState.adBusy, false);
  release();
  await done;
  eq('and it stays clear once the answer lands', !!telState.adBusy, false);
}

// --- the member count: a step of opening the chat ----------------------------

const open = grab('telOpenPeer');
eq('opening asks for the count after the counters, before the ads',
  open.indexOf('telFetchMembers(') > open.indexOf('telRefreshStats(') &&
  open.indexOf('telFetchMembers(') < open.indexOf('telLoadAds('), true);
eq('and inside the planned steps', open.indexOf('telFetchMembers(') < open.indexOf('telStepsDone()'), true);

const fetchFn = grab('telFetchMembers');
function members(state, peer) {
  const calls = [];
  const run = new Function('telState', 'telPeerKey', 'telBgOp', 'telCallQuiet', 'telStepsMore', 'telRender',
    'telLoadMembers', fetchFn + '\nreturn telFetchMembers;');
  const f = run(state, (p) => p.kind + ':' + p.id,
    async (label, fn) => { calls.push(label); return fn(); },
    async (url, body) => { calls.push(url); return { ok: true, members: 321, known: true }; },
    () => {}, () => {}, async () => {});
  return { calls, go: () => f('channel:9', peer) };
}
{
  const st = { peer: { kind: 'channel', id: '9' }, members: {}, membersFresh: {} };
  const m = members(st, st.peer);
  await m.go();
  eq('an unknown count is a visible step', m.calls, ['tel_loading_members', '/api/tel/members']);
  eq('and lands in the header', st.members['channel:9'], 321);
}
{
  const st = { peer: { kind: 'channel', id: '9' }, members: { 'channel:9': 10 }, membersFresh: { 'channel:9': true } };
  const m = members(st, st.peer);
  await m.go();
  eq('a fresh count costs nothing', m.calls, []);
}
{
  // The page stayed open past the day: the backend says the count it holds
  // is no longer current. It is shown until the new one lands, and read again
  // -- held in memory as "known", it was never read again while the page
  // lived, which on a phone is days.
  const st = { peer: { kind: 'channel', id: '9' }, members: { 'channel:9': 10 }, membersFresh: { 'channel:9': false } };
  const m = members(st, st.peer);
  await m.go();
  eq('a stale count is read again', m.calls, ['tel_loading_members', '/api/tel/members']);
  eq('and replaced', [st.members['channel:9'], st.membersFresh['channel:9']], [321, true]);
}
{
  const peer = { kind: 'channel', id: '9', members: 50 };
  const st = { peer, members: {}, membersFresh: {} };
  const m = members(st, peer);
  await m.go();
  eq('nor does one that came with the list', m.calls, []);
}
for (const [what, peer] of [['person', { kind: 'user', id: '9' }], ['bot', { kind: 'bot', id: '9' }],
  ['direct chat', { kind: 'group', id: '9', accessHash: '4', direct: true }],
  ['basic group', { kind: 'group', id: '9', accessHash: '0' }]]) {
  const st = { peer, members: {}, membersFresh: {} };
  const m = members(st, peer);
  await m.go();
  eq(`no count is asked for a ${what}`, m.calls, []);
}
{
  const peer = { kind: 'group', id: '9', accessHash: '4' };
  const st = { peer, members: {}, membersFresh: {} };
  const m = members(st, peer);
  await m.go();
  eq('a supergroup is asked for', m.calls, ['tel_loading_members', '/api/tel/members']);
}

// A channel opened by @username or a t.me link (or the new-chat box) opens
// inline, not through telOpenPeer, and skipped the step: it showed no count at
// all (seen live: Telegram's username lookup carries none). After their own
// telOp, since the step is a telBgOp and would be refused while it runs.
for (const fn of ['telOpenByUsername', 'telStartNewChat']) {
  const body = grab(fn);
  const at = body.indexOf('telFetchMembers(');
  eq(`${fn} reads the member count too`, at > 0, true);
  eq(`${fn}: after its own operation, not inside it`, at > body.lastIndexOf('\n  });'), true);
  eq(`${fn}: only for the chat still open`, /telPeerKey\(telState\.peer\) === [\w.]+\) await telFetchMembers\(/.test(body), true);
}

for (const k of ['tel_loading_members', 'tel_loading_ad']) {
  eq(`${k} in both languages`, (i18n.match(new RegExp('\\b' + k + ':', 'g')) || []).length, 2);
}

console.log(`${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
