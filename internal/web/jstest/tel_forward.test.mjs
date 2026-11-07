// Forward: offered in ⋮ unless Telegram's content protection forbids it, a
// picker with Telegram's options, one call whatever they are, and the
// "Forwarded from" line on forwards.
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

const chan = { kind: 'channel', id: '40', accessHash: '4', title: 'Space Watch', readOnly: true };
const alex = { kind: 'user', id: '8', accessHash: '9', title: 'Alex' };
const book = { kind: 'group', id: '20', accessHash: '0', title: 'Book Club' };
const paid = { kind: 'group', id: '50', accessHash: '5', title: 'Paid', direct: true, paidStars: 10 };
const key = (p) => p ? p.kind + ':' + p.id : '';

function harness(peer, msgs, answer, fetchedTarget) {
  const threads = { [key(peer)]: { msgs, fetched: true }, 'user:8': { msgs: [{ id: 1, text: 'old' }], fetched: !!fetchedTarget } };
  const state = { peer, dialogs: [chan, alex, book, paid].map((p) => ({ peer: p })) };
  const seen = { sheet: '', calls: [], toasts: [], merged: null, touched: null, renders: 0, list: { innerHTML: '' }, opts: [] };
  const sheetEl = { querySelectorAll: () => seen.opts };
  const api = new Function('telState', 'telThread', 'telPeerKey', 'telSheet', 'telSheetItem', 'telT', 'telCloseSheet',
    'telRender', 'telWhenIdle', 'telOp', 'telCall', 'telTouchPreview', 'telMergeMessages', 'telDialogFor', 'telEsc',
    'telAttrQ', 'telAvatar', 'telSafeText', 'icon', 'hydrateIcons', 'showToast', 'document', 'seen', 'jsStr',
    fn('telFindMsg') + fn('telProtected') + fn('telChannelBased') + fn('telCanDelete') + fn('telMsgMenu') + fn('telHasMsgMenu') +
    fn('telForwardTargets') + fn('telForwardListHTML') + fn('telFwdOption') + fn('telStartForward') +
    fn('telForwardOption') + fn('telForwardFilter') + fn('telDoForward') + fn('telFwdHTML') +
    '; return { has: telHasMsgMenu, menu: telMsgMenu, start: telStartForward, opt: telForwardOption, filter: telForwardFilter, go: telDoForward, fwd: telFwdHTML, targets: telForwardTargets };',
  )(state, (k) => threads[k] || (threads[k] = { msgs: [] }), key, (h) => { seen.sheet = h; },
    (i, label, on) => '<' + i + ' ' + on + '>' + label + '</>', (k) => '[' + k + ']', () => {}, () => { seen.renders++; },
    async () => {}, async (l, f) => f(), async (url, body) => { seen.calls.push([url, body]); return answer; },
    (k, th) => { seen.touched = k; }, (th, m) => { seen.merged = m; th.msgs.push(...m); },
    (k) => state.dialogs.find((d) => key(d.peer) === k) || null, (s) => String(s), (s) => String(s),
    (n) => '<av ' + n + '>', (s) => s, (n) => '<i ' + n + '>', () => {}, (m) => seen.toasts.push(m),
    { getElementById: (id) => id === 'telFwdList' ? seen.list : (id === 'telSheet' ? sheetEl : null) }, seen, (s) => String(s));
  return { state, seen, api, threads };
}

// --- offered unless protected ----------------------------------------------------
{
  const { seen, api } = harness(chan, [{ id: 5, text: 'post' }, { id: 6, text: 'locked', protected: true }]);
  api.menu(5);
  eq('a channel post can be forwarded', /telStartForward\(5\)/.test(seen.sheet), true);
  seen.sheet = '';
  api.menu(6);
  eq('a protected message: only Report here', seen.sheet.replace(/<fail telReportMsg\(6\)>\[tel_report\]<\/>/, ''), '');
  eq('a protected message: no Forward', /telStartForward/.test(seen.sheet), false);
  eq('and no Copy either', /telCopyMsg/.test(seen.sheet), false);
}
{
  const { seen, api } = harness({ ...chan, protected: true }, [{ id: 5, text: 'post' }]);
  api.menu(5);
  eq('a protected channel: neither Forward nor Copy', [/telStartForward/.test(seen.sheet), /telCopyMsg/.test(seen.sheet)], [false, false]);
  seen.sheet = '';
  api.start(5);
  eq('nor can the picker be opened around the menu', seen.sheet, '');
}

{
  const { seen, api } = harness({ kind: 'user', id: '7', accessHash: '1', title: 'Mia' }, [{ id: 9, text: 'secret', protected: true }]);
  api.menu(9);
  eq('protected in a chat we write to: Reply stays, Copy and Forward go',
    [/telStartReply/.test(seen.sheet), /telCopyMsg/.test(seen.sheet), /telStartForward/.test(seen.sheet)], [true, false, false]);
}

// --- a ⋮ only where the menu has something --------------------------------------
{
  const mia = { kind: 'user', id: '7', accessHash: '1', title: 'Mia' };
  const peers = [chan, { ...chan, protected: true }, mia, { ...mia, protected: true }, book,
    { kind: 'group', id: '30', accessHash: '3', title: 'Super', readOnly: true, protected: true }];
  const msgs = [{ text: 'x' }, { text: '' , media: 'photo' }, { text: 'x', out: true }, { text: 'x', protected: true }, { text: 'x', out: true, protected: true }];
  let agree = 0, total = 0;
  for (const p of peers) {
    for (const m0 of msgs) {
      const m = { id: 5, ...m0 };
      const { seen, api } = harness(p, [m]);
      api.menu(5);
      total++;
      if (api.has(p, m) === (seen.sheet !== '')) agree++;
      else console.error('disagree', JSON.stringify(p), JSON.stringify(m));
    }
  }
  eq('the ⋮ is drawn exactly when the menu opens', agree, total);
  const { api } = harness(chan, []);
  // Still drawn: someone else's post can be reported even where it cannot be
  // copied or forwarded.
  eq('a protected channel post keeps its ⋮, for Report', api.has({ ...chan, protected: true }, { id: 1, text: 'x' }), true);
}
eq('the bubble asks before drawing it', /!m\.service && telHasMsgMenu\(peer, m\) \? '<button class="tel-msg-more"/.test(src), true);

// --- self-destructing media ---------------------------------------------------
{
  const media = new Function('telT', 'telSafeText', 'telFmtBytes', 'icon', 'telEsc',
    'var TEL_MEDIA_ICON = { photo: "views", photo_expired: "clock", video_expired: "clock" };' +
    fn('telMediaHTML') + '; return telMediaHTML;',
  )((k) => '[' + k + ']', (s) => s, (n) => n + 'B', (n) => '<i ' + n + '>', (s) => String(s));
  eq('a timed photo says it self-destructs', /\[tel_media_photo\] · \[tel_media_timed\]/.test(media({ media: 'photo', timed: true })), true);
  eq('an expired one says it expired', /<i clock>.*\[tel_media_photo_expired\]/.test(media({ media: 'photo_expired', timed: true })), true);
  eq('and not that it self-destructs as well', /tel_media_timed/.test(media({ media: 'photo_expired', timed: true })), false);
  eq('an ordinary photo is just a photo', /tel_media_timed/.test(media({ media: 'photo' })), false);
  // The bridge marks timed media protected, so ⋮ offers no Forward.
  const { seen, api } = harness({ kind: 'user', id: '7', accessHash: '1', title: 'Mia' },
    [{ id: 9, text: '', media: 'photo', timed: true, protected: true }]);
  api.menu(9);
  eq('no Forward on a self-destructing photo', /telStartForward/.test(seen.sheet), false);
}

// --- the picker -------------------------------------------------------------------
{
  const { seen, api, state } = harness(chan, [{ id: 5, text: 'post' }, { id: 7, text: 'caption', media: 'photo' }]);
  eq('targets: writable and free only', api.targets('').map((d) => d.peer.title), ['Alex', 'Book Club']);
  eq('search narrows them, case aside', api.targets('alex').map((d) => d.peer.title), ['Alex']);
  api.start(5);
  eq('options: sender name and sound', [/hideSender/.test(seen.sheet), /silent/.test(seen.sheet)], [true, true]);
  eq('no captions option for a text message', /hideCaptions/.test(seen.sheet), false);
  eq('both chats listed', [/telDoForward\('user:8'\)/.test(seen.sheet), /telDoForward\('group:20'\)/.test(seen.sheet)], [true, true]);
  eq('a channel we only read is not a target', /telDoForward\('channel:40'\)/.test(seen.sheet), false);
  eq('a paid conversation is not a target', /telDoForward\('group:50'\)/.test(seen.sheet), false);
  api.filter('book');
  eq('typing redraws only the list', [/Book Club/.test(seen.list.innerHTML), /Alex/.test(seen.list.innerHTML)], [true, false]);
  api.filter('zzz');
  eq('nothing found says so', /tel_forward_none/.test(seen.list.innerHTML), true);
  api.start(7);
  eq('media with a caption offers hiding it', /hideCaptions/.test(seen.sheet), true);
  api.opt('hideCaptions', true);
  eq('hiding captions hides the sender too', state.fwd.options, { hideSender: true, hideCaptions: true, silent: false });
  api.opt('hideSender', false);
  eq('showing the sender brings the captions back', state.fwd.options, { hideSender: false, hideCaptions: false, silent: false });
  api.opt('silent', true);
  eq('silent', state.fwd.options.silent, true);
}

// --- the call ----------------------------------------------------------------------
{
  const copy = { id: 90, text: 'post', out: true, forwarded: true, fwd: 'Space Watch' };
  const { seen, api, threads } = harness(chan, [{ id: 5, text: 'post' }], { ok: true, messages: [copy] }, true);
  api.start(5);
  api.opt('silent', true);
  await api.go('user:8');
  eq('one call, with source, target and options', seen.calls, [['/api/tel/forward',
    { from: chan, ids: [5], to: alex, options: { hideSender: false, hideCaptions: false, silent: true } }]]);
  eq('the copy lands in the open target at once', threads['user:8'].msgs.map((m) => m.id), [1, 90]);
  eq('and its row is touched', seen.touched, 'user:8');
  eq('a toast names the target', seen.toasts, ['[tel_forwarded]'.replace('{name}', 'Alex')]);
}
{
  const { seen, api, threads } = harness(chan, [{ id: 5, text: 'post' }], { ok: true, messages: [{ id: 91 }] }, false);
  api.start(5);
  await api.go('user:8');
  eq('a target not loaded yet is left to load itself', threads['user:8'].msgs.map((m) => m.id), [1]);
}
{
  const { seen, api } = harness(chan, [{ id: 5, text: 'post' }], null, true);
  api.start(5);
  await api.go('user:8');
  eq('a refusal: nothing merged, no toast', [seen.merged, seen.toasts.length], [null, 0]);
}

// --- "Forwarded from" ------------------------------------------------------------------
{
  const { api } = harness(chan, []);
  eq('named source', /\[tel_forwarded_from\]/.test(api.fwd({ forwarded: true, fwd: 'Alex' })), true);
  eq('unnamed source still marked', /\[tel_forwarded_msg\]/.test(api.fwd({ forwarded: true })), true);
  eq('an ordinary message has none', api.fwd({ text: 'x' }), '');
}
eq('every bubble draws it', /html \+= telFwdHTML\(m\) \+ telReplyHTML\(m, th\);/.test(src), true);

for (const k of ['tel_forward', 'tel_forward_to', 'tel_forward_hide_sender', 'tel_forward_hide_captions', 'tel_forward_silent',
  'tel_forward_search', 'tel_forward_none', 'tel_forwarded', 'tel_forwarded_from', 'tel_forwarded_msg', 'tel_err_forward',
  'tel_err_forward_protected', 'tel_err_cannot_write']) {
  eq(k + ' in fa and en', (i18n.match(new RegExp('\\b' + k + ':', 'g')) || []).length, 2);
}

console.log(`tel_forward: ${pass} passed, ${fail} failed`);
if (fail) process.exit(1);
