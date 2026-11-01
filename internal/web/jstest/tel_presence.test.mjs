// Online status and "typing…" -- Telegram's terms forbid hiding either -- are
// reported, but cheaply: online renewed every few minutes, typing at most
// every few seconds, and nothing while the link is busy.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const src = readFileSync(join(here, '..', 'static', 'js', 'tel.js'), 'utf8');

let pass = 0, fail = 0;
const eq = (name, got, want) => {
  if (JSON.stringify(got) === JSON.stringify(want)) { pass++; return; }
  fail++;
  console.error(`FAIL ${name}\n  got  ${JSON.stringify(got)}\n  want ${JSON.stringify(want)}`);
};
const fn = (name) => {
  const m = src.match(new RegExp('(async )?function ' + name + '\\([^)]*\\) \\{[\\s\\S]*?\\n\\}\\n'));
  if (!m) throw new Error('missing ' + name);
  return m[0];
};

let clock = 1_000_000;
const RealDate = Date;
const FakeDate = { now: () => clock };

function harness() {
  const posts = [];
  const state = { open: true, busy: false, bg: false, peer: null, draft: '' };
  const doc = { hidden: false };
  let ready = true;
  const api = new Function('telState', 'document', 'telReady', 'telPeerKey', 'telPresencePost', 'Date',
    'var TEL_ONLINE_RENEW_MS = 4 * 60 * 1000; var TEL_TYPING_EVERY_MS = 5000;' +
    'var telPresence = { online: false, at: 0 }; var telTypingAt = {};' +
    fn('telPresenceTick') + fn('telTypingTick') + '; return { tick: telPresenceTick, typing: telTypingTick };',
  )(state, doc, () => ready, (p) => p.kind + ':' + p.id, (url, body) => posts.push([url, body]), FakeDate);
  return { posts, state, doc, api, setReady: (v) => { ready = v; } };
}

// --- online ------------------------------------------------------------------
{
  const { posts, state, doc, api, setReady } = harness();
  api.tick();
  eq('in the section, visible and connected: online', posts, [['/api/tel/status', { online: true }]]);
  clock += 60_000; api.tick();
  eq('not repeated every tick', posts.length, 1);
  clock += 4 * 60_000; api.tick();
  eq('renewed before it lapses', posts.length, 2);
  doc.hidden = true; api.tick();
  eq('the page goes to the background: offline at once', posts[2], ['/api/tel/status', { online: false }]);
  api.tick();
  eq('and only once', posts.length, 3);
  doc.hidden = false; state.busy = true; api.tick();
  eq('back, but the link is busy: online waits', posts.length, 3);
  state.busy = false; api.tick();
  eq('then goes out', posts[3], ['/api/tel/status', { online: true }]);
  state.open = false; api.tick();
  eq('leaving the section: offline', posts[4], ['/api/tel/status', { online: false }]);
  state.open = true; setReady(false); api.tick();
  eq('not connected: nothing claimed', posts.length, 5);
}
eq('the ticker drives it', /telPaintCountdown\(\);\s*telPresenceTick\(\);/.test(fn('telTick')), true);
eq('leaving the section reports offline', /telState\.open = false;\s*telPresenceTick\(\);/.test(fn('telTeardown')), true);
eq('so does the page going to the background', /addEventListener\('visibilitychange', telPresenceTick\)/.test(src), true);

// --- typing ---------------------------------------------------------------------
{
  const { posts, state, api } = harness();
  const dm = { kind: 'user', id: 8 };
  state.peer = dm; state.draft = 'h';
  api.typing();
  eq('typing is shown', posts, [['/api/tel/typing', { peer: dm }]]);
  clock += 2000; api.typing();
  eq('not on every keystroke', posts.length, 1);
  clock += 4000; api.typing();
  eq('again after a few seconds', posts.length, 2);
  clock += 6000; state.draft = '   '; api.typing();
  eq('an empty box is not typing', posts.length, 2);
  state.draft = 'x'; state.busy = true; api.typing();
  eq('nothing while the link is busy', posts.length, 2);
  state.busy = false; state.peer = { kind: 'channel', id: 9, readOnly: true }; api.typing();
  eq('nothing where we cannot write', posts.length, 2);
  state.peer = dm; state.reply = { id: 4, edit: true }; api.typing();
  eq('nothing while editing an old message', posts.length, 2);
  state.reply = { id: 4 }; api.typing();
  eq('a reply is typing', posts.length, 3);
}
eq('the composer reports typing', /oninput="telState\.draft=this\.value;telGrowInput\(this\);telTypingTick\(\)"/.test(src), true);

console.log(`tel_presence: ${pass} passed, ${fail} failed`);
if (fail) process.exit(1);
