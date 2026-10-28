// When ✓ becomes ✓✓.
//
// Delivery is a DNS round trip the frontend makes on its own, and the peer
// picks our message up on one of ITS poll cycles — long after the post-send
// probes (0/3/8s) have stopped. Two things kept the second tick from ever
// arriving: nothing asked for the status again, and the one place that did ask
// threw the answer away while the caret was still in the message box.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const src = readFileSync(join(here, '..', 'static', 'js', 'chat.js'), 'utf8');

let pass = 0, fail = 0;
const eq = (name, got, want) => {
  if (JSON.stringify(got) === JSON.stringify(want)) { pass++; return; }
  fail++;
  console.error(`FAIL ${name}\n  got  ${JSON.stringify(got)}\n  want ${JSON.stringify(want)}`);
};

// --- which tick a message gets --------------------------------------------

// The real branch out of chatRenderThread, so loosening it fails these cases.
const tickBlock = src.match(/var sk = m\.server \|\| threadServer[\s\S]*?if \(sk && del < m\.seq\) pending\[sk\] = true;/)[0];
const tick = (m, st, threadServer) => new Function(
  'm', 'st', 'threadServer', 'icon', 'pending',
  'var ticks = ""; ' + tickBlock + '; return { ticks: ticks, pending: Object.keys(pending) };',
)(m, st, threadServer, n => n, {});

const sent = { accepted: { 'a.test': 2 }, delivered: {} };
const got = { accepted: { 'a.test': 2 }, delivered: { 'a.test': 2 } };

eq('accepted but not fetched yet is one tick',
  tick({ seq: 2, server: 'a.test' }, sent).ticks, ' tickSingle');
eq('and stays on the probe list',
  tick({ seq: 2, server: 'a.test' }, sent).pending, ['a.test']);
eq('delivered is two ticks',
  tick({ seq: 2, server: 'a.test' }, got).ticks, ' tickDouble');
eq('and comes off the probe list',
  tick({ seq: 2, server: 'a.test' }, got).pending, []);
eq('not even accepted yet is the clock',
  tick({ seq: 3, server: 'a.test' }, sent).ticks, ' clock');
// Counters are per server (seq numbering is per server): another server's
// delivered count must not tick this message.
eq('another server\'s count does not tick this message',
  tick({ seq: 2, server: 'b.test' }, got).ticks, ' clock');

// --- who asks again, and when ---------------------------------------------

// The backend is the only poller; it announces each cycle with 'nextpoll'.
// That announcement is the status refresh's cadence — no frontend timer.
const sseSrc = src.match(/function chatOnSSE\(data\) \{[\s\S]*?\n\}/)[0];
const onSSE = (state) => {
  const calls = [];
  const fn = new Function(
    'chatState', 'chatRefreshStatus', 'chatUpdateCountdown', 'chatBumpActivity',
    'chatNotifyNew', 'chatLoadThreads', 'chatRenderThread', 'chatMergeServerResult',
    'chatShowHandshakeProgress', 'chatShowProgress', 'chatShowPollProgress',
    'clearTimeout', 'document',
    sseSrc + '; return chatOnSSE;',
  )(state, a => calls.push(a), () => { }, () => { }, () => { },
    () => Promise.resolve(), () => { }, () => { }, () => { }, () => { }, () => { },
    () => { }, { querySelector: () => null });
  return { fn, calls };
};

const base = (over) => Object.assign({
  open: true, view: 'thread', peer: 'p1', pendingServers: { p1: ['a.test'] },
}, over);

let t = onSSE(base());
t.fn({ type: 'nextpoll', ms: 20000 });
eq('a poll cycle re-asks for delivery while a message is outstanding', t.calls, ['p1']);

// An idle conversation must not spend a query: every message already has ✓✓.
t = onSSE(base({ pendingServers: { p1: [] } }));
t.fn({ type: 'nextpoll', ms: 20000 });
eq('nothing outstanding costs no query', t.calls, []);

t = onSSE(base({ pendingServers: {} }));
t.fn({ type: 'nextpoll', ms: 20000 });
eq('a conversation never sent to costs no query', t.calls, []);

// Not looking at the conversation → nothing to repaint.
t = onSSE(base({ view: 'list' }));
t.fn({ type: 'nextpoll', ms: 20000 });
eq('the list view does not probe', t.calls, []);

t = onSSE(base({ open: false }));
t.fn({ type: 'nextpoll', ms: 20000 });
eq('a closed messenger does not probe', t.calls, []);

// The countdown still has to be driven by the same event.
const st = base();
t = onSSE(st);
t.fn({ type: 'nextpoll', ms: 20000 });
eq('and the countdown still follows the backend schedule', st.pollIntervalMs, 20000);

// --- the answer must reach the screen -------------------------------------

// chatRenderThread reuses the live footer while the input has focus and keeps
// the scroll position, so the status path must not defer: a deferred render is
// flushed only by a scroll or a popup close, which pins ✓ for anyone who sent
// a message and left the caret in the box.
const statusBody = src.match(/async function chatFetchPeerStatus\([\s\S]*?\n\}/)[0];
eq('the status path does not defer on input focus',
  /renderPending\s*=\s*true/.test(statusBody), false);
eq('it renders instead', /chatRenderThread\(\)/.test(statusBody), true);

// And the render it calls is the one that protects a focused box.
eq('and that render keeps the live footer when the box has focus',
  /var liveFooter = hadFocus && !firstRender \? modal\.querySelector\('\.chat-footer'\) : null;/.test(src), true);

console.log(`${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
