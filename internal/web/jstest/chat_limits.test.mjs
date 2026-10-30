// Each chat server advertises its own limits and counts its own sends, so the
// numbers shown must be the ones of the server the conversation uses, whatever
// order the servers' answers arrive in.
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

const fn = (name) => {
  const m = src.match(new RegExp('function ' + name + '\\([^)]*\\) \\{[\\s\\S]*?\\n\\}\\n'));
  if (!m) throw new Error('missing ' + name);
  return m[0];
};
const code = ['chatLimitsKey', 'chatKeepRowLimits', 'chatCurLimits', 'chatCurQuota',
  'chatMergeServerResult', 'chatMaxBytes', 'chatQuotaIcon'].map(fn).join('\n');

const load = (state) => new Function('chatState', 'chatUsableServers', 'chatT', 'chatFmtTime', 'chatQuotaSvg',
  code + '; return { chatCurLimits, chatCurQuota, chatMergeServerResult, chatKeepRowLimits, chatMaxBytes, chatQuotaIcon };',
)(state, () => (state.avail.servers || []).filter((s) => s.available && s.enabled),
  (k) => k + ':{n}', () => '', () => '');

const own = { maxMsgBytes: 900, sendPerHour: 300, inboxCap: 200, ttlHours: 72 };
const other = { maxMsgBytes: 500, sendPerHour: 30, inboxCap: 50, ttlHours: 72 };
const fresh = () => ({
  open: false, peer: 'p', curServer: '', pendingServer: {}, quotaByServer: {}, avail: null,
});

// --- the reported bug: answers alternate, the number must not ---------------
{
  const st = fresh();
  const c = load(st);
  st.curServer = 'other.test';
  const seen = [];
  for (const [key, lim] of [['own.test', own], ['other.test', other], ['own.test', own], ['other.test', other], ['own.test', own]]) {
    c.chatMergeServerResult({ key, enabled: true, available: true }, lim);
    seen.push((c.chatCurLimits() || {}).sendPerHour ?? null);
  }
  eq('conversation on the 30 server never shows 300', seen, [null, 30, 30, 30, 30]);
  eq('and keeps showing 30 once the 300 server answered last', c.chatCurLimits().sendPerHour, 30);
  eq('message size is that server\'s too', c.chatMaxBytes(), 500);

  st.curServer = 'own.test';
  eq('switching the conversation to the 300 server shows 300', c.chatCurLimits().sendPerHour, 300);
  eq('with its own message size', c.chatMaxBytes(), 900);
}

// --- limits carried in the row itself (aggregate response) ------------------
{
  const st = fresh();
  const c = load(st);
  st.curServer = 'other.test';
  c.chatMergeServerResult({ key: 'own.test', enabled: true, available: true, limits: own }, null);
  c.chatMergeServerResult({ key: 'other.test', enabled: true, available: true, limits: other }, null);
  eq('row limits are read per server', c.chatCurLimits().sendPerHour, 30);
}

// --- a later answer without limits keeps that server's last known ones ------
{
  const prev = { servers: [{ key: 'own.test', limits: own }, { key: 'other.test', limits: other }] };
  const next = { servers: [{ key: 'own.test' }, { key: 'other.test', limits: { sendPerHour: 40 } }] };
  load(fresh()).chatKeepRowLimits(next, prev);
  eq('missing limits come from the same server', next.servers[0].limits, own);
  eq('never from another server, and new ones win', next.servers[1].limits, { sendPerHour: 40 });
}

// --- no server chosen yet: the strictest one -------------------------------
{
  const st = fresh();
  const c = load(st);
  st.peer = null;
  c.chatMergeServerResult({ key: 'own.test', enabled: true, available: true }, own);
  c.chatMergeServerResult({ key: 'other.test', enabled: true, available: true }, other);
  eq('no conversation: the smallest send cap', c.chatCurLimits().sendPerHour, 30);
  eq('and the smallest message size', c.chatMaxBytes(), 500);
  c.chatMergeServerResult({ key: 'other.test', enabled: true, available: false }, other);
  eq('an unreachable server does not lower it', c.chatCurLimits().sendPerHour, 300);
}

// --- quota is per server ----------------------------------------------------
{
  const st = fresh();
  const c = load(st);
  c.chatMergeServerResult({ key: 'own.test', enabled: true, available: true }, own);
  c.chatMergeServerResult({ key: 'other.test', enabled: true, available: true }, other);
  st.quotaByServer['other.test'] = { remaining: 4, resetUnix: 0 };
  st.quotaByServer['own.test'] = { remaining: 250, resetUnix: 0 };
  st.curServer = 'other.test';
  eq('quota of the conversation\'s server', c.chatCurQuota().remaining, 4);
  eq('the ring is drawn against that server\'s cap', c.chatQuotaIcon().low, true);
  st.curServer = 'own.test';
  eq('the other server keeps its own count', c.chatCurQuota().remaining, 250);
}

// --- every place that stores a quota names its server -----------------------
eq('no shared quota left', /chatState\.quota\b/.test(src), false);
eq('no shared limits left', /avail\.limits/.test(src), false);

console.log(`chat_limits: ${pass} passed, ${fail} failed`);
if (fail) process.exit(1);
