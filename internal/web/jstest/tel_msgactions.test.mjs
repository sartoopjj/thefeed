// The ⋮ on every message, and what it offers: edit our own text, delete where
// Telegram can (for me / for everyone), each one call.
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

const dm = { kind: 'user', id: 8, accessHash: '9' };
const basic = { kind: 'group', id: 20, accessHash: '0' };
const superg = { kind: 'group', id: 30, accessHash: '3' };
const channel = { kind: 'channel', id: 40, accessHash: '4', readOnly: true };

function harness(peer, msgs, answer) {
  const state = { peer, reply: null, draft: '', threads: { [peer.kind + ':' + peer.id]: { msgs } } };
  const seen = { sheet: '', calls: [], box: { value: '', style: {}, focus() {} } };
  const api = new Function('telState', 'telThread', 'telPeerKey', 'telSheet', 'telSheetItem', 'telT', 'telCloseSheet',
    'telRender', 'telWhenIdle', 'telOp', 'telCall', 'telTouchPreview', 'telGrowInput', 'document', 'seen',
    fn('telFindMsg') + fn('telProtected') + fn('telMsgMenu') + fn('telChannelBased') + fn('telCanDelete') + fn('telAskDelete') +
    fn('telDeleteMsg') + fn('telStartEdit') + fn('telSendEdit') +
    '; return { menu: telMsgMenu, ask: telAskDelete, del: telDeleteMsg, edit: telStartEdit, send: telSendEdit };',
  )(state, (k) => state.threads[k], (p) => p ? p.kind + ':' + p.id : '', (h) => { seen.sheet = h; },
    (i, label, on) => '<' + i + ' ' + on + '>' + label + '</>', (k) => '[' + k + ']', () => {}, () => {},
    async () => {}, async (l, f) => f(), async (url, body) => { seen.calls.push([url, body]); if (seen.onCall) seen.onCall(); return answer; },
    () => {}, () => {}, { getElementById: () => seen.box }, seen);
  return { state, seen, api, thread: () => state.threads[peer.kind + ':' + peer.id] };
}

// --- what the menu offers ------------------------------------------------------
{
  const { seen, api } = harness(dm, [{ id: 1, text: 'mine', out: true }, { id: 2, text: 'theirs' }]);
  api.menu(1);
  eq('our own message: reply, copy, edit, delete', ['telStartReply', 'telCopyMsg', 'telStartEdit', 'telAskDelete'].map((f) => seen.sheet.includes(f)), [true, true, true, true]);
  api.menu(2);
  eq('theirs in a private chat: no edit, but delete', [seen.sheet.includes('telStartEdit'), seen.sheet.includes('telAskDelete')], [false, true]);
}
{
  const { seen, api } = harness(superg, [{ id: 3, text: 'theirs' }, { id: 4, text: 'mine', out: true }]);
  api.menu(3);
  eq('theirs in a supergroup: no delete (only admins could)', seen.sheet.includes('telAskDelete'), false);
  api.menu(4);
  eq('ours in a supergroup: delete', seen.sheet.includes('telAskDelete'), true);
}
{
  const { seen, api } = harness(channel, [{ id: 5, text: 'post' }]);
  api.menu(5);
  eq('a channel we only read: copy alone', ['telStartReply', 'telCopyMsg', 'telStartEdit', 'telAskDelete'].map((f) => seen.sheet.includes(f)), [false, true, false, false]);
}

// --- delete ------------------------------------------------------------------------
{
  const { seen, api } = harness(basic, [{ id: 6, text: 'x', out: true }], { ok: true });
  api.ask(6);
  eq('the account\'s own box: for me, or for everyone',
    [seen.sheet.includes('telDeleteMsg(6,false)'), seen.sheet.includes('telDeleteMsg(6,true)')], [true, true]);
}
{
  const { seen, api } = harness(superg, [{ id: 7, text: 'x', out: true }], { ok: true });
  api.ask(7);
  eq('a supergroup: for everyone only', [seen.sheet.includes('telDeleteMsg(7,false)'), seen.sheet.includes('telDeleteMsg(7,true)')], [false, true]);
}
{
  const { seen, api, thread } = harness(dm, [{ id: 8, text: 'a' }, { id: 9, text: 'b' }], { ok: true });
  await api.del(9, true);
  eq('one call, with the choice', seen.calls, [['/api/tel/delete-messages', { peer: dm, ids: [9], revoke: true }]]);
  eq('and the message leaves at once', thread().msgs.map((m) => m.id), [8]);
}
{
  const { api, thread } = harness(dm, [{ id: 8, text: 'a' }], null);
  await api.del(8, false);
  eq('a refused delete leaves it', thread().msgs.map((m) => m.id), [8]);
}

// --- edit --------------------------------------------------------------------------
{
  const { state, seen, api, thread } = harness(dm, [{ id: 10, text: 'teh', out: true, entities: [{ type: 'bold' }] }], { ok: true, editDate: 500 });
  api.edit(10);
  eq('edit puts the text back in the composer', [state.draft, seen.box.value, state.reply.edit, state.reply.id], ['teh', 'teh', true, 10]);
  await api.send(dm, state.reply, 'the');
  eq('one call with the new text', seen.calls, [['/api/tel/edit', { peer: dm, msgId: 10, text: 'the' }]]);
  const m = thread().msgs[0];
  eq('the message changes in place, marked edited', [m.text, m.editDate, m.entities.length], ['the', 500, 0]);
}
{
  const { state, seen, api } = harness(dm, [{ id: 11, text: 'teh', out: true }], null);
  const ed = { key: 'user:8', id: 11, edit: true };
  await api.send(dm, ed, 'the');
  eq('a refused edit puts the text back, nothing lost', [state.draft, seen.box.value, state.reply === ed], ['the', 'the', true]);
}
{
  const { state, seen, api } = harness(dm, [{ id: 11, text: 'teh', out: true }], null);
  const ed = { key: 'user:8', id: 11, edit: true };
  seen.onCall = () => { state.peer = basic; };
  await api.send(dm, ed, 'the');
  eq('refused after moving to another chat: its composer is left alone', [state.draft, seen.box.value, state.reply], ['', '', null]);
}
{
  const { seen, api } = harness(dm, [{ id: 12, text: 'same', out: true }], { ok: true });
  await api.send(dm, { id: 12, edit: true }, 'same');
  eq('unchanged text costs nothing', seen.calls.length, 0);
}

// --- deleting a whole conversation -------------------------------------------------
{
  const sheetFor = (peer) => {
    let sheet = '';
    const del = new Function('telCloseSheet', 'telDialogFor', 'telAttrQ', 'telEsc', 'telT', 'telSheetItem', 'telSheet', 'jsStr',
      fn('telChannelBased') + fn('telDeleteChat') + '; return telDeleteChat;')(() => {}, () => ({ peer }), (x) => x, (x) => x,
      (k) => '[' + k + ']{name}', (i, label, on) => '<' + on + '>' + label + '</>', (h) => { sheet = h; }, (x) => x);
    del(peer.kind + ':' + peer.id);
    return sheet;
  };
  const priv = sheetFor({ kind: 'user', id: 8, title: 'Alex' });
  eq('a private chat: for me, or for both sides', [priv.includes("telDoDeleteChat('user:8',false)"), priv.includes("telDoDeleteChat('user:8',true)"), priv.includes('Alex')], [true, true, true]);
  const bas = sheetFor({ kind: 'group', id: 20, accessHash: '0' });
  eq('a basic group: for me only', [bas.includes(',false)'), bas.includes(',true)')], [true, false]);
  eq('a supergroup is left', sheetFor({ kind: 'group', id: 30, accessHash: '3' }).includes('[tel_leave_group]'), true);
  eq('a channel is left', sheetFor({ kind: 'channel', id: 40, accessHash: '4' }).includes('[tel_leave]'), true);
  const dir = sheetFor({ kind: 'group', id: 200, accessHash: '2', direct: true });
  eq('direct messages: removed from our list, and it says so', [dir.includes('[tel_direct_remove]'), dir.includes('[tel_direct_remove_note]'), dir.includes(',true)')], [true, true, false]);
}

// --- plumbing --------------------------------------------------------------------------
{
  const send = fn('telSend');
  eq('an edit is sent as an edit, not a new message', /if \(rp && rp\.edit\)[\s\S]*telSendEdit\(peer, rp, text\)/.test(send), true);
  eq('cancelling an edit empties the box', /wasEdit[\s\S]*box\.value = ''/.test(fn('telCancelReply')), true);
  eq('the ⋮ is on every real message with something to offer', /!m\.pending && !m\.failed && !m\.service && telHasMsgMenu\(peer, m\) \? '<button class="tel-msg-more"/.test(fn('telRenderPane')), true);
}
for (const k of ['tel_leave_group', 'tel_delete_for_both', 'tel_direct_remove', 'tel_direct_remove_note', 'tel_msg_menu', 'tel_edit', 'tel_editing', 'tel_delete_msg', 'tel_delete_for_me', 'tel_delete_for_all',
  'tel_err_edit', 'tel_err_edit_expired', 'tel_err_edit_not_yours', 'tel_err_delete_msg', 'tel_err_delete_forbidden']) {
  eq(k + ' in fa and en', (i18n.match(new RegExp('\\b' + k + ':', 'g')) || []).length, 2);
}

console.log(`tel_msgactions: ${pass} passed, ${fail} failed`);
if (fail) process.exit(1);
