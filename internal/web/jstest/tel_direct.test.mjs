// Writing to a channel's admins (Telegram's direct messages): a button on
// channels that take them, one call to find the conversation, nothing paid.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const src = readFileSync(join(here, '..', 'static', 'js', 'tel.js'), 'utf8');
const i18n = readFileSync(join(here, '..', 'static', 'js', 'i18n.js'), 'utf8');

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

const channel = { kind: 'channel', id: 100, accessHash: 1, title: 'News', directId: '200' };
const direct = { kind: 'group', id: 200, accessHash: 2, title: 'News', direct: true };

function harness(answer) {
  const state = { peer: channel, dialogs: [] };
  const seen = { calls: [], opened: [], info: '', renders: 0, signal: null };
  const api = new Function('telState', 'telWhenIdle', 'telOp', 'telCall', 'telPeerKey', 'telDialogFor', 'telOpenPeer',
    'showInfoDialog', 'telT', 'telRender', 'seen',
    'var telDirectCtl = null;' + fn('telCancelDirect') + fn('telOpenDirect') +
    '; return { open: telOpenDirect, cancel: telCancelDirect, busy: () => !!telDirectCtl };',
  )(state, async () => {}, async (l, f) => f(),
    async (url, body, fb, signal) => { seen.calls.push([url, body]); seen.signal = signal; return typeof answer === 'function' ? answer(signal) : answer; },
    (p) => p.kind + ':' + p.id, (k) => state.dialogs.find((d) => d.peer.kind + ':' + d.peer.id === k) || null,
    async (k) => { seen.opened.push(k); }, (m) => { seen.info = m; }, (k) => '[' + k + ']{n}',
    () => { seen.renders++; }, seen);
  return { state, seen, open: api.open, api };
}

{
  const { state, seen, open } = harness({ ok: true, peer: direct });
  await open();
  eq('asks for the channel\'s conversation', seen.calls, [['/api/tel/direct', { peer: channel }]]);
  eq('opens it', seen.opened, ['group:200']);
  eq('with a row in the list', state.dialogs.map((d) => d.peer.id), [200]);
  eq('that does not offer joining', state.dialogs[0].visiting, undefined);
  await open();
  eq('opening again adds no second row', state.dialogs.length, 1);
}
{
  const { seen, open } = harness({ ok: true, peer: { ...direct, paidStars: 50 } });
  await open();
  eq('a paid channel is explained, not opened', [seen.opened.length, seen.info.includes('50')], [0, true]);
}
{
  const { seen, open, state } = harness(null);
  await open();
  eq('a refusal opens nothing', seen.opened, []);
  state.peer = { kind: 'channel', id: 9 };
  seen.calls.length = 0;
  await open();
  eq('a channel without direct messages asks nothing', seen.calls, []);
}

// --- opening shows at once and can be cancelled ------------------------------
{
  const slow = (signal) => new Promise((res) => { signal.addEventListener('abort', () => res(null)); });
  const { seen, api } = harness(slow);
  const pending = api.open();
  eq('the button changes the moment it is pressed', [api.busy(), seen.renders >= 1], [true, true]);
  api.cancel();
  await pending;
  eq('cancelled while waiting for the link: nothing is sent', seen.calls.length, 0);
  eq('and nothing opens', seen.opened, []);
  eq('the button is back', api.busy(), false);
}
{
  const slow = (signal) => new Promise((res) => { signal.addEventListener('abort', () => res(null)); });
  const { seen, api } = harness(slow);
  const pending = api.open();
  await new Promise((r) => setTimeout(r, 0));
  eq('the request is on its way', seen.calls.length, 1);
  api.cancel();
  await pending;
  eq('cancel stops the request itself', seen.signal.aborted, true);
  eq('and nothing opens', seen.opened, []);
}

// --- the bottom bar where we cannot write -------------------------------------
{
  const mk = (dialog, busy) => new Function('telPeerKey', 'telDialogFor', 'telAttrQ', 'telEsc', 'telT', 'icon', 'telDirectCtl', 'jsStr',
    fn('telReadOnlyBarHTML') + '; return telReadOnlyBarHTML;')((p) => p.kind + ':' + p.id, () => dialog,
    (x) => x, (x) => x, (k) => '[' + k + ']', (n) => '<' + n + '>', busy ? {} : null, (x) => x);
  const bar = mk({ muted: false }, false)(channel);
  eq('mute is at the bottom, as in Telegram', /telToggleMute\('channel:100'\)[\s\S]*\[tel_mute\]/.test(bar), true);
  eq('and the direct-message button beside it', /onclick="telOpenDirect\(\)"[\s\S]*\[tel_direct\]/.test(bar), true);
  eq('a muted channel offers unmute', mk({ muted: true }, false)(channel).includes('[tel_unmute]'), true);
  eq('a channel without direct messages has no such button', mk({ muted: false }, false)({ kind: 'channel', id: 9 }).includes('telOpenDirect'), false);
  const busy = mk({ muted: false }, true)(channel);
  eq('while opening: progress and cancel', /onclick="telCancelDirect\(\)"[\s\S]*\[tel_direct_opening\][\s\S]*\[tel_cancel\]/.test(busy), true);
}

const pane = fn('telRenderPane');
eq('the bar replaces the composer where we cannot write', /if \(peer\.readOnly\) \{\s*return html \+ telReadOnlyBarHTML\(peer\);/.test(pane), true);
eq('no button up in the header any more', /onclick="telOpenDirect\(\)"/.test(pane), false);
eq('the conversation says what it is', /peer\.direct \? 'tel_direct_sub'/.test(pane), true);
eq('and so does its row', /d\.peer\.direct \? ' · ' \+ telEsc\(telT\('tel_direct_short'\)\)/.test(src), true);
for (const k of ['tel_direct', 'tel_direct_opening', 'tel_direct_sub', 'tel_direct_short', 'tel_direct_paid', 'tel_err_paid', 'tel_err_no_direct']) {
  eq(k + ' in fa and en', (i18n.match(new RegExp('\\b' + k + ':', 'g')) || []).length, 2);
}

console.log(`tel_direct: ${pass} passed, ${fail} failed`);
if (fail) process.exit(1);
