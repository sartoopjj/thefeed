// A conversation must end up with every message Telegram has, whatever order
// they arrived in and whatever we sent meanwhile, and with each message's
// latest text.
//
// Telegram numbers a chat's messages in one sequence. When the other side's
// message landed between two of our polls and we then sent one, ours took the
// next number; asking for "newer than the newest we hold" then skipped theirs
// for good. And a message we already held was never replaced, so an edit was
// never seen.
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
const optional = (name) => { try { return fn(name); } catch (e) { return ''; } };

const PAGE = Number(src.match(/var TEL_HISTORY_PAGE = (\d+);/)[1]);
const MAX_PAGES = Number((src.match(/var TEL_NEW_MAX_PAGES = (\d+);/) || [0, 1])[1]);

// A fake Telegram: getHistory over one chat, newest first, honouring
// limit / offsetId / minId exactly as messages.getHistory does.
function fakeTelegram() {
  const all = [];
  const asked = [];
  return {
    all, asked,
    add(m) { all.push(m); },
    edit(id, text, editDate) { const m = all.find((x) => x.id === id); m.text = text; m.editDate = editDate; },
    // The backend's cache, which its own check for changes keeps current.
    cache() { return { ok: true, cached: true, messages: all.map((m) => ({ ...m })).sort((a, b) => a.id - b.id) }; },
    history(q) {
      if (q.includes('cacheOnly=1')) return this.cache();
      const p = new URLSearchParams(q.split('?')[1]);
      const limit = +p.get('limit'), offsetId = +(p.get('offsetId') || 0), minId = +(p.get('minId') || 0);
      asked.push({ offsetId, minId });
      const page = all.filter((m) => m.id > minId && (!offsetId || m.id < offsetId))
        .sort((a, b) => b.id - a.id).slice(0, limit).map((m) => ({ ...m }));
      return { ok: true, messages: page.reverse() };
    },
  };
}

function load(tg) {
  const peer = { kind: 'user', id: 7 };
  const state = { peer, threads: {} };
  const code = [
    fn('telHistoryQuery'), fn('telNewestRealID'), optional('telFetchCursor'),
    fn('telMergeMessages'), fn('telThreadFetchNew'),
  ].join('\n');
  const api = new Function(
    'telState', 'telThread', 'telPeerKey', 'telCall', 'telCallQuiet', 'telBgOp', 'telOp', 'telRender',
    'telTouchPreview', 'telSepPin', 'telNotifyNew', 'TEL_HISTORY_PAGE', 'TEL_PENDING_BASE', 'TEL_NEW_MAX_PAGES',
    'var telHistCtl = null; var AbortController = function () { this.signal = null; };\n' + code +
    '; return { telThreadFetchNew, telMergeMessages, telNewestRealID };',
  )(state,
    (k) => state.threads[k] || (state.threads[k] = { msgs: [] }),
    (p) => p.kind + ':' + p.id,
    async (q) => tg.history(q),
    async (q) => tg.history(q),
    async (label, run) => run(), async (label, run) => run(), () => {},
    () => {}, () => {}, () => {}, PAGE, 1e12, MAX_PAGES);
  return { api, state, thread: () => state.threads['user:7'] };
}

// What telSend does with Telegram's answer, taken from the real code so that
// changing it changes this test.
const ack = src.match(/if \(r\.msgId\) \{[\s\S]*?pending\.pending = false;(?:\n\s*\/\/[^\n]*)*(?:\n[^\n]*pending\.sent = true;)?/)[0];
const sendAck = (th, msgId, text) => {
  const pending = { id: 1e12 + 1, text, out: true, pending: true };
  th.msgs.push(pending);
  new Function('pending', 'r', ack.replace(/^if \(r\.msgId\) \{/, '') )(pending, { msgId });
  th.msgs.sort((a, b) => a.id - b.id);
};

// --- the reported case ------------------------------------------------------
{
  const tg = fakeTelegram();
  const { api, thread } = load(tg);
  tg.add({ id: 9, text: 'hers 1' });
  tg.add({ id: 10, text: 'hers 2' });
  await api.telThreadFetchNew(true);
  eq('first two arrive', thread().msgs.map((m) => m.id), [9, 10]);

  tg.add({ id: 11, text: 'hers 3, not fetched yet' }); // lands on Telegram
  tg.add({ id: 12, text: 'mine', out: true });          // we send; Telegram numbers it after hers
  sendAck(thread(), 12, 'mine');
  await api.telThreadFetchNew(true);
  eq('her third message is not skipped', thread().msgs.map((m) => m.id), [9, 10, 11, 12]);
  eq('and ours is there once', thread().msgs.filter((m) => m.id === 12).length, 1);

  await api.telThreadFetchNew(true);
  eq('a later poll asks from the newest fetched id', tg.asked[tg.asked.length - 1].minId, 12);
}

// --- more arrived than one page holds -----------------------------------------
{
  const tg = fakeTelegram();
  const { api, thread } = load(tg);
  tg.add({ id: 1, text: 'x' });
  await api.telThreadFetchNew(true);
  for (let i = 2; i <= 2 + PAGE * 2; i++) tg.add({ id: i, text: 'm' + i });
  await api.telThreadFetchNew(true);
  const ids = thread().msgs.map((m) => m.id);
  const missing = [];
  for (let i = 1; i <= 2 + PAGE * 2; i++) if (!ids.includes(i)) missing.push(i);
  eq('nothing between the old newest and the new newest is skipped', missing, []);
}

// --- edits ------------------------------------------------------------------
{
  const tg = fakeTelegram();
  const { api, thread } = load(tg);
  tg.add({ id: 5, text: 'typo' });
  await api.telThreadFetchNew(true);
  api.telMergeMessages(thread(), [{ id: 5, text: 'fixed', editDate: 100 }], true);
  eq('an edited message replaces the old text', thread().msgs[0].text, 'fixed');
  api.telMergeMessages(thread(), [{ id: 5, text: 'older copy', editDate: 50 }], false);
  eq('an older copy (from the disk cache) does not undo it', thread().msgs[0].text, 'fixed');
  thread().msgs[0].views = 40;
  api.telMergeMessages(thread(), [{ id: 5, text: 'again', editDate: 200 }], true);
  eq('an edit keeps counts it does not carry', [thread().msgs[0].text, thread().msgs[0].views], ['again', 40]);
}

// --- an edit made while the conversation is open reaches it -----------------
{
  const tg = fakeTelegram();
  const { api, thread } = load(tg);
  tg.add({ id: 20, text: 'hello' });
  tg.add({ id: 21, text: 'how ar you' });
  await api.telThreadFetchNew(true);
  tg.edit(21, 'how are you', 500);   // she fixes it; nothing new is sent
  await api.telThreadFetchNew(true);
  eq('the edit shows up on the next poll', thread().msgs.find((m) => m.id === 21).text, 'how are you');
  eq('without dragging in messages that were never on screen', thread().msgs.map((m) => m.id), [20, 21]);
}

// --- the "edited" mark -----------------------------------------------------
{
  const edited = new Function('telT', 'telEsc', 'telAttrQ', 'telFmtTime',
    fn('telEditedHTML') + '; return telEditedHTML;')((k) => '[' + k + ']', (x) => x, (x) => x, (d) => 't' + d);
  eq('an edited message says so, with the edit time on hover',
    edited({ id: 1, text: 'x', editDate: 90 }), '<span class="tel-msg-edited" title="t90">[tel_edited]</span>');
  eq('an unedited one does not', edited({ id: 1, text: 'x' }), '');
  eq('nor a service line', edited({ id: 1, service: true, editDate: 90 }), '');
  eq('the mark is drawn in every message\'s meta line', /telStatsHTML\(m\) \+ telEditedHTML\(m\)/.test(src), true);
}

console.log(`tel_gaps: ${pass} passed, ${fail} failed`);
if (fail) process.exit(1);
