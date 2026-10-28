// How much of the conversation list is fetched, and when.
//
// The rule: ONE page automatically, the rest only when the user asks. An
// account with hundreds of conversations is six or seven pages of DNS queries
// past the first, and on a link where every query costs seconds that is not
// something to spend without being asked — least of all on every start.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const src = readFileSync(join(here, '..', 'static', 'js', 'tel.js'), 'utf8');

let pass = 0, fail = 0;
function eq(name, got, want) {
  if (JSON.stringify(got) === JSON.stringify(want)) { pass++; return; }
  fail++;
  console.error(`FAIL ${name}\n  got  ${JSON.stringify(got)}\n  want ${JSON.stringify(want)}`);
}

// --- nothing walks the list on its own --------------------------------------

// The background pager is gone. Its parts must not come back quietly: each of
// these is enough to start fetching pages nobody asked for.
for (const ghost of ['telScheduleAutoPage', 'telArmAutoPage', 'telState.paging', 'pageTimer']) {
  eq(`no background walk: ${ghost}`, src.includes(ghost), false);
}

// The first load fetches exactly one page and stops.
const loadFn = src.match(/async function telLoadDialogs\([\s\S]*?\n}/)[0];
eq('the first load fetches one page', (loadFn.match(/telCall\(/g) || []).length, 1);
eq('and does not chain into another', /telLoadMoreDialogs|ScheduleAutoPage/.test(loadFn), false);

// Priming from the cache fetches nothing at all.
const primeFn = src.match(/async function telPrimeFromCache\([\s\S]*?\n}/)[0];
eq('priming from cache touches no live page',
  /telLoadMoreDialogs|ArmAutoPage|refresh=1/.test(primeFn), false);
eq('priming from cache asks the cache only', /cacheOnly=1/.test(primeFn), true);

// --- the user's own request --------------------------------------------------

const moreFn = src.match(/async function telLoadMoreDialogs\([\s\S]*?\n}/)[0];
// One press, one page: it must not loop.
eq('load-more fetches a single page', (moreFn.match(/telCall\(/g) || []).length, 1);
eq('load-more does not recurse', /telLoadMoreDialogs\(/.test(moreFn.slice(40)), false);
// It pages from the cursor, or it would refetch page one for ever.
eq('load-more continues from the cursor', /telState\.cursor/.test(moreFn), true);
eq('load-more stops when there is no more', /!telState\.more/.test(moreFn), true);

// The button is what offers it, and only when there is more to get.
eq('the button hangs off the more flag',
  /if \(telState\.more\) \{[\s\S]{0,260}telLoadMoreNow\(\)/.test(src), true);

// --- the button must not be dead while the link is busy ---------------------

// The list refreshes itself every minute and a fetch takes tens of seconds
// over DNS, so a press almost always lands while something else holds the
// tunnel. It used to be disabled outright during a foreground call, and
// during a BACKGROUND poll it looked enabled while telOp swallowed the press.
const btn = src.match(/if \(telState\.more\) \{[\s\S]*?tel-hint[\s\S]*?<\/div>'/)[0];
eq('not disabled merely because the link is busy', /telState\.busy \? ' disabled'/.test(btn), false);
eq('disabled only while its own press is in flight',
  /telState\.morePending \? ' disabled'/.test(btn), true);

const nowFn = src.match(/async function telLoadMoreNow\([\s\S]*?\n}/)[0];
eq('waits for the link instead of giving up', /telWhenIdle\(\)/.test(nowFn), true);
eq('retries a bounded number of times', /TEL_MORE_TRIES/.test(nowFn), true);
eq('and releases the button when it gives up', /finally/.test(nowFn), true);

// A dropped call has to be distinguishable from a page that brought nothing,
// or the retry loop cannot tell whether it ever ran.
const moreFn2 = src.match(/async function telLoadMoreDialogs\([\s\S]*?\n}/)[0];
eq('reports when it never ran', /return ran \? added : null/.test(moreFn2), true);

// Run the real loop: busy for the first two goes, then free.
let idleCalls = 0, fetches = 0;
let busy = true;
const telWhenIdle = async () => { idleCalls++; if (idleCalls >= 3) busy = false; };
const telLoadMoreDialogs = async () => (busy ? null : (fetches++, 1));
await new Function('telState', 'telWhenIdle', 'telLoadMoreDialogs', 'telRender', 'TEL_MORE_TRIES',
  nowFn + '; return telLoadMoreNow();',
)({ morePending: false }, telWhenIdle, telLoadMoreDialogs, () => {}, 6);
eq('keeps trying until the link frees up', [idleCalls, fetches], [3, 1]);

// --- the count line must not promise what cannot be fetched ---------------

// Telegram's count includes conversations this client cannot show, so it can
// sit one or two above what is actually loadable. Saying "197 of 198" when 197
// is everything reads as a failure and sends the user pressing a dead button.
const lineFn = src.match(/function telLoadedLine\([\s\S]*?\n}/)[0];
const line = new Function('telState', 'telT',
  lineFn + '; return telLoadedLine();');
const t = k => k;
eq('says "of N" while there is more to fetch',
  line({ dialogs: new Array(40), total: 300, more: true }, t), 'tel_loaded_of');
eq('and just the count once there is not',
  line({ dialogs: new Array(197), total: 198, more: false }, t), 'tel_loaded_n');
eq('and when the count agrees anyway',
  line({ dialogs: new Array(198), total: 198, more: true }, t), 'tel_loaded_n');

console.log(`${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
