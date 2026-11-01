// Replying in a Telegram conversation: tap a message, Reply, and the send goes
// out as an answer to it -- with the quote on the placeholder at once.
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

const group = { kind: 'group', id: 7, title: 'Book Club' };
const dm = { kind: 'user', id: 8, title: 'Alex' };
const channel = { kind: 'channel', id: 9, title: 'News', readOnly: true };

// The reply functions, over a state the test owns.
function harness(peer, msgs) {
  const state = { peer, reply: null, threads: {} };
  state.threads[peer.kind + ':' + peer.id] = { msgs };
  const seen = { sheet: '', renders: 0, focused: false };
  const api = new Function('telState', 'telThread', 'telPeerKey', 'telSheet', 'telSheetItem', 'telT',
    'telCloseSheet', 'telRender', 'telMediaLabel', 'document', 'window', 'seen',
    fn('telFindMsg') + fn('telReplyFor') + fn('telMsgTap') + fn('telMsgMenu') + fn('telStartReply') +
    fn('telCancelReply') +
    '; return { tap: telMsgTap, menu: telMsgMenu, start: telStartReply, cancel: telCancelReply, replyFor: telReplyFor };',
  )(state, (k) => state.threads[k], (p) => p ? p.kind + ':' + p.id : '',
    (h) => { seen.sheet = h; }, (i, label, on) => '<' + i + ' ' + on + '>' + label + '</>', (k) => '[' + k + ']',
    () => {}, () => { seen.renders++; }, () => '[media]',
    { getElementById: () => ({ focus: () => { seen.focused = true; } }) },
    { getSelection: () => seen.selection || '' }, seen);
  return { state, seen, api };
}
const target = (matches) => ({ closest: (sel) => (matches && sel.split(', ').some((s) => matches.includes(s)) ? {} : null) });

// --- the tap ------------------------------------------------------------------
{
  const { seen, api } = harness(group, [{ id: 5, text: 'Chapter 9?', from: 'Mia' }]);
  for (const own of ['a', 'button', '.tel-reacts', '.tel-reply', '.tel-spoiler', '.tel-kbd']) {
    seen.sheet = '';
    api.tap({ target: target([own]) }, 5);
    eq('a tap on ' + own + ' keeps its own meaning', seen.sheet, '');
  }
  seen.selection = 'Chapter';
  api.tap({ target: target(null) }, 5);
  eq('finishing a text selection opens nothing', seen.sheet, '');
  seen.selection = '';
  api.tap({ target: target(null) }, 5);
  eq('a plain tap offers Reply and Copy', /telStartReply\(5\)[\s\S]*telCopyMsg\(5\)/.test(seen.sheet), true);
}
{
  const { seen, api } = harness(channel, [{ id: 3, text: 'post' }, { id: 4, text: '' }]);
  api.menu(3);
  eq('a read-only channel offers no Reply', /telStartReply/.test(seen.sheet), false);
  eq('but still Copy', /telCopyMsg\(3\)/.test(seen.sheet), true);
  seen.sheet = '';
  api.menu(4);
  eq('nothing to offer, no sheet', seen.sheet, '');
}

// --- who is being answered ------------------------------------------------------
{
  const { state, seen, api } = harness(group, [
    { id: 5, text: 'Chapter 9?', from: 'Mia' }, { id: 6, text: 'mine', out: true }, { id: 7, text: '', media: 'photo' },
  ]);
  api.start(5);
  eq('a group message names its sender', [state.reply.who, state.reply.text, state.reply.id], ['Mia', 'Chapter 9?', 5]);
  eq('the composer is focused to type the answer', seen.focused, true);
  api.start(6);
  eq('our own message is "You"', state.reply.who, '[tel_you]');
  api.start(7);
  eq('a message without text is named by its attachment', state.reply.text, '[media]');
  eq('the reply belongs to this conversation', !!api.replyFor(group), true);
  eq('and to no other', api.replyFor(dm), null);
  api.cancel();
  eq('cancel drops it', state.reply, null);
}
{
  const { state, api } = harness(dm, [{ id: 2, text: 'hi' }]);
  api.start(2);
  eq('in a private chat the other side is named by the chat', state.reply.who, 'Alex');
}

// --- the send --------------------------------------------------------------------
{
  // telReplyFor as the page has it, bound to this test's state.
  const mk = (state) => new Function('telState', 'telPeerKey', fn('telReplyFor') + '; return telReplyFor;')(
    state, (p) => p ? p.kind + ':' + p.id : '');
  for (const [name, reply, want] of [
    ['an answer carries replyTo', { key: 'group:7', id: 5, who: 'Mia', text: 'Chapter 9?' }, 5],
    ['a plain send carries none', null, undefined],
    ['a reply started in another chat is not sent here', { key: 'user:8', id: 2 }, undefined],
  ]) {
    const peer = group;
    const state = { peer, reply, draft: '', threads: { 'group:7': { msgs: [] } } };
    const calls = [];
    let first = null;
    const telSend = new Function('document', 'telState', 'telPeerKey', 'telThread', 'telReplyFor', 'TEL_PENDING_BASE',
      'telRender', 'telWhenIdle', 'telSteps', 'telOp', 'telCall', 'telTouchPreview', 'telStepsMore', 'telStepAdvance',
      'telHistoryQuery', 'telFetchCursor', 'telMergeMessages', 'telDropPending', 'telT', 'telRefreshTicks', 'telStepsDone',
      'var telPendingSeq = 0;' + fn('telSend') + '; return telSend;',
    )({ getElementById: () => ({ value: 'see you at 7' }) }, state, (p) => p.kind + ':' + p.id,
      (k) => state.threads[k], mk(state), 1e12,
      () => { first = first || JSON.parse(JSON.stringify(state.threads['group:7'].msgs[0] || null)); },
      async () => {}, () => {}, async (l, f) => f(),
      async (url, body) => { calls.push(body); return { ok: true, msgId: 99 }; },
      () => {}, () => {}, () => {}, () => '', () => 0, () => 0, () => {}, (k) => k, async () => {}, () => {});
    await telSend();
    eq(name, calls[0].replyTo, want);
    eq(name + ': the placeholder shows it at once', first && first.replyTo, want);
    eq(name + ': the reply is used up', state.reply, null);
  }
}

// --- the strip and the plumbing ----------------------------------------------------
{
  const bar = new Function('telEsc', 'telSafeText', 'telT', 'telAttrQ', 'icon', 'TEL_KEEP_FOCUS',
    fn('telReplyBarHTML') + '; return telReplyBarHTML;')((x) => String(x).replace(/</g, '&lt;'), (x) => x,
    (k) => '[' + k + ']', (x) => x, () => '', '');
  const html = bar({ id: 5, who: '<b>Mia</b>', text: 'Chapter 9?' });
  eq('the strip names who and quotes what', /&lt;b>Mia[\s\S]*Chapter 9\?/.test(html), true);
  eq('it can be cancelled', /onclick="telCancelReply\(\)"/.test(html), true);
  eq('tapping it jumps to the message', /telJumpToMsg\(5\)/.test(html), true);
}
{
  const pane = fn('telRenderPane');
  eq('the strip is drawn OUTSIDE the composer, which telKeepComposer never redraws',
    pane.indexOf('telReplyBarHTML(rp)') > -1 && pane.indexOf('telReplyBarHTML(rp)') < pane.indexOf('<div class="tel-compose">'), true);
  eq('only a real message is tappable', /!m\.pending && !m\.service \? ' onclick="telMsgTap\(event,/.test(pane), true);
  eq('switching conversations drops an unsent reply', /telState\.reply = null;/.test(fn('telEnterThread')), true);
  eq('Escape cancels it', /Escape[\s\S]*telCancelReply\(\)/.test(fn('telInputKey')), true);
  eq('a retried reply is still a reply', /telState\.reply = msg\.replyTo/.test(fn('telRetrySend')), true);
}
for (const k of ['tel_reply', 'tel_reply_cancel', 'tel_you']) {
  eq(k + ' in fa and en', (i18n.match(new RegExp('\\b' + k + ':', 'g')) || []).length, 2);
}

console.log(`tel_reply: ${pass} passed, ${fail} failed`);
if (fail) process.exit(1);
