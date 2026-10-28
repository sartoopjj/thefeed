// Which messages a counter refresh asks about. Every id costs bytes in the
// answer and every byte is a DNS query, so this is a cost decision as much as
// a correctness one.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const src = readFileSync(join(here, '..', 'static', 'js', 'tel.js'), 'utf8');

const MAX = Number(src.match(/var TEL_STATS_MAX = (\d+);/)[1]);
const MIN_MS = Number(src.match(/var TEL_STATS_MIN_MS = (\d+);/)[1]);
const fn = src.match(/function telStatsWanted\([\s\S]*?\n}/)[0];

let visible = [];
const telStatsWanted = new Function(
  'TEL_STATS_MAX', 'TEL_STATS_MIN_MS', 'telVisibleMsgIDs',
  fn + '; return telStatsWanted;',
)(MAX, MIN_MS, () => visible);

let pass = 0, fail = 0;
function eq(name, got, want) {
  const g = JSON.stringify(got), w = JSON.stringify(want);
  if (g === w) { pass++; return; }
  fail++;
  console.error(`FAIL ${name}\n  got  ${g}\n  want ${w}`);
}

const thread = (ids, statsAt) => ({ msgs: ids.map(id => ({ id })), statsAt });

// Only what the reader can see.
visible = [5, 6, 7];
eq('asks about the visible messages', telStatsWanted(thread([1, 2, 3, 4, 5, 6, 7])), [5, 6, 7]);

// Scrolling back brings older ones into view, and those are what get asked.
visible = [1, 2, 3];
eq('scrolling back asks about the older ones', telStatsWanted(thread([1, 2, 3, 4, 5, 6, 7])), [1, 2, 3]);

// A post asked about moments ago cannot have moved enough to pay for again.
visible = [5, 6, 7];
const now = Date.now();
eq('skips what was just refreshed',
  telStatsWanted(thread([5, 6, 7], { 5: now, 6: now })), [7]);

// Old enough, so it is worth asking again.
eq('asks again once the floor has passed',
  telStatsWanted(thread([5, 6, 7], { 5: now - MIN_MS - 1, 6: now })), [5, 7]);

// Everything fresh: no request at all, rather than an empty-handed round trip.
eq('nothing to ask means no request',
  telStatsWanted(thread([5, 6, 7], { 5: now, 6: now, 7: now })), []);

// A screenful at most, taking the newest when more are visible.
visible = Array.from({ length: MAX + 5 }, (_, i) => i + 1);
const capped = telStatsWanted(thread(visible));
eq('capped at a screenful', [capped.length, capped[0], capped[capped.length - 1]],
  [MAX, 6, MAX + 5]);

// Before the first render nothing is on screen yet; the thread itself is the
// fallback, or opening a conversation would never refresh its counters.
visible = [];
eq('falls back to the thread when nothing is measured yet',
  telStatsWanted(thread([1, 2, 3])), [1, 2, 3]);

// Placeholders for messages still being sent have synthetic ids and no
// counters to read.
visible = [];
eq('ignores ids that are not real', telStatsWanted(thread([0, -1, 4])), [4]);

// Messages that have just arrived from Telegram carry the counts Telegram has
// for them right now. Asking again immediately is two more round trips per
// conversation opened, over DNS, for numbers that cannot have moved -- so the
// merge stamps them fresh and this returns nothing.
const mergeFn = src.match(/function telMergeMessages\([\s\S]*?\n}/)[0];
eq('a merge can mark the counts fresh', /statsAt\[m\.id\] = now/.test(mergeFn), true);

// ...but ONLY for messages that came from Telegram just now. The same
// messages read back from the cache carry counts as old as the cache is, and
// treating those as current is how a post read in its first minute keeps its
// empty reaction row for ever — the very thing the refresh exists to stop.
eq('only a live merge marks them', /if \(live\) th\.statsAt/.test(mergeFn), true);

// Run the real merge both ways.
const merge = new Function('th', 'msgs', 'live',
  mergeFn + '; return telMergeMessages(th, msgs, live);');
const msgs = [{ id: 10 }, { id: 11 }];

const fromLink = { msgs: [] };
merge(fromLink, msgs, true);
eq('live messages are stamped', Object.keys(fromLink.statsAt).sort(), ['10', '11']);

const fromDisk = { msgs: [] };
merge(fromDisk, msgs, false);
eq('cached messages are NOT stamped', Object.keys(fromDisk.statsAt), []);
eq('so the refresh still asks about them',
  telStatsWanted(Object.assign(fromDisk, { statsAt: fromDisk.statsAt })).sort(), [10, 11]);

// The cache prime is the caller that must pass false.
const primeFn = src.match(/async function telPrimeThreadFromCache\([\s\S]*?\n}/)[0];
eq('priming from cache does not claim freshness',
  /telMergeMessages\(th, r\.messages, false\)/.test(primeFn), true);

// With everything just merged, there is nothing to ask about at all.
visible = [5, 6, 7];
const justArrived = thread([5, 6, 7]);
justArrived.statsAt = { 5: Date.now(), 6: Date.now(), 7: Date.now() };
eq('nothing to refresh right after a fetch', telStatsWanted(justArrived), []);

// And once the floor has passed, the counters are worth asking for again:
// views and reactions really do move after a post is written.
const later = Date.now() - MIN_MS - 1;
eq('worth asking again once they could have moved',
  telStatsWanted(thread([5, 6, 7], { 5: later, 6: later, 7: later })), [5, 6, 7]);

console.log(`${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
