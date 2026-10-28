// Where the "new messages" line goes, and what it takes to move it.
//
// Opening a conversation with unread posts used to land on the newest one --
// the one place from which the run of new posts is invisible. The line is the
// answer, and it has exactly two ways to be wrong: in the wrong place, or
// moving while someone is reading it.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const src = readFileSync(join(here, '..', 'static', 'js', 'tel.js'), 'utf8');

const STICKY = Number(src.match(/var TEL_SEP_STICKY_MS = (\d+);/)[1]);
const fns = ['telSepFor', 'telSepPin', 'telSepArmCommit', 'telSepFromUnread']
  .map(n => src.match(new RegExp(`function ${n}\\([\\s\\S]*?\\n}`))[0])
  .join('\n');

let pass = 0, fail = 0;
function eq(name, got, want) {
  const g = JSON.stringify(got), w = JSON.stringify(want);
  if (g === w) { pass++; return; }
  fail++;
  console.error(`FAIL ${name}\n  got  ${g}\n  want ${w}`);
}

// One fresh module per case: the pin is deliberately sticky, so state must not
// leak between them.
function load() {
  const timers = [];
  const api = new Function(
    'TEL_SEP_STICKY_MS', 'telState', 'telRender', 'setTimeout',
    fns + '; return {telSepFor, telSepPin, telSepFromUnread};',
  )(
    STICKY,
    { sep: {}, sepScroll: '' },
    () => { },
    (fn, ms) => { timers.push({ fn, ms }); return timers.length; },
  );
  return { api, timers };
}

// --- placing the line from Telegram's read mark -----------------------------

{
  const { api, timers } = load();
  api.telSepPin('channel:7', 90);
  eq('pins at the read mark', api.telSepFor('channel:7'), 90);
  // The window is armed by the pin, not by the first render: a conversation
  // left open with the line showing must still dismiss it.
  eq('arms the sticky window once', timers.map(t => t.ms), [STICKY]);
}

// Nothing to place it from is not a reason to invent a position: a line in the
// wrong place is worse than no line.
{
  const { api, timers } = load();
  api.telSepPin('channel:7', 0);
  eq('no mark means no line', api.telSepFor('channel:7'), 0);
  eq('and no timer', timers.length, 0);
}

// --- the line must not walk ------------------------------------------------

// A busy channel keeps arriving while it is open. Letting each fetch re-pin
// would drag the line down message by message, re-scrolling the reader and
// quietly redefining "new" as "the last one".
{
  const { api } = load();
  api.telSepPin('channel:7', 90);
  api.telSepPin('channel:7', 95);
  api.telSepPin('channel:7', 99);
  eq('first pin wins for the whole window', api.telSepFor('channel:7'), 90);
}

// Two conversations keep their own lines: switching between them must not
// carry one chat's boundary into another.
{
  const { api } = load();
  api.telSepPin('channel:7', 90);
  api.telSepPin('user:3', 40);
  eq('one line per conversation',
    [api.telSepFor('channel:7'), api.telSepFor('user:3')], [90, 40]);
  eq('an unopened conversation has none', api.telSepFor('channel:9'), 0);
}

// --- one scroll, not one per render ---------------------------------------

{
  const state = { sep: {}, sepScroll: '' };
  const api = new Function(
    'TEL_SEP_STICKY_MS', 'telState', 'telRender', 'setTimeout',
    fns + '; return {telSepFor, telSepPin, telSepFromUnread};',
  )(STICKY, state, () => { }, () => 1);
  api.telSepPin('channel:7', 90);
  eq('a new line asks to be scrolled to', state.sepScroll, 'channel:7');
  // The render consumes the request. A re-pin inside the window must not
  // resurrect it, or every refresh tick yanks the view back to the line.
  state.sepScroll = '';
  api.telSepPin('channel:7', 95);
  eq('a refresh inside the window does not re-scroll', state.sepScroll, '');
}

// --- the fallback: counting back from the newest ---------------------------

// A row cached before the read mark was carried has only the count. Telegram's
// unread count is exactly how many incoming messages sit after the mark, so
// counting back that many lands on the first unread one.
const th = (...msgs) => ({ msgs });
const inMsg = id => ({ id });
const outMsg = id => ({ id, out: true });

{
  const { api } = load();
  eq('counts back to the first unread',
    api.telSepFromUnread(th(inMsg(10), inMsg(11), inMsg(12)), 2), 10);
}

// Our own messages were never unread, so they cannot be counted: Telegram did
// not count them either, and counting them here would stop the walk short and
// put the line far too late -- below posts the user has not seen.
{
  const { api } = load();
  eq('skips our own messages',
    api.telSepFromUnread(th(inMsg(10), inMsg(11), outMsg(99), inMsg(100)), 2), 10);
}

// Service messages ("X joined") are not posts anybody reads, and Telegram does
// not count them as unread either.
{
  const { api } = load();
  eq('skips service messages',
    api.telSepFromUnread(th(inMsg(10), inMsg(11), { id: 99, service: true }, inMsg(100)), 2), 10);
}

// Fewer loaded than unread: the boundary is further back than this page, so
// there is nothing honest to return. Guessing would draw the line at the top
// of the page and claim posts the user has read are new.
{
  const { api } = load();
  eq('gives up rather than guessing',
    api.telSepFromUnread(th(inMsg(10), inMsg(11)), 5), 0);
  eq('nothing loaded, nothing to place', api.telSepFromUnread(th(), 3), 0);
  eq('no unread, no line', api.telSepFromUnread(th(inMsg(10)), 0), 0);
}

// --- what the renderer does with it ----------------------------------------

// The line is drawn before the first INCOMING message past the boundary. A
// message of ours is not new to us: sending a reply and coming back would
// otherwise put the line under our own message.
// The condition comes out of tel.js itself rather than being retyped here, so
// loosening it in the renderer fails these cases instead of passing them.
const cond = src.match(/if \((sepAfter && !sepDrawn && [^)]*)\) \{\n\s*html \+= '<div class="tel-new-sep"/)[1];
const shouldDraw = new Function('sepAfter', 'sepDrawn', 'm', 'return !!(' + cond + ');');

// Which message the line lands above, running the real condition over a page.
const drawAt = (sepAfter, msgs) => {
  let sepDrawn = false, at = 0;
  for (const m of msgs) {
    if (shouldDraw(sepAfter, sepDrawn, m)) { sepDrawn = true; at = m.id; }
  }
  return at;
};

eq('drawn before the first message past the mark',
  drawAt(90, [inMsg(88), inMsg(90), inMsg(91), inMsg(92)]), 91);
eq('our own newer message does not open the run',
  drawAt(90, [inMsg(90), outMsg(91), inMsg(92)]), 92);
eq('nothing newer than the mark means no line',
  drawAt(90, [inMsg(88), inMsg(90)]), 0);
eq('no mark means no line at all',
  drawAt(0, [inMsg(88), inMsg(90)]), 0);
// Drawn once. A second line further down would read as a second run of new
// messages that does not exist.
eq('drawn once per render',
  [shouldDraw(90, false, inMsg(91)), shouldDraw(90, true, inMsg(92))], [true, false]);

// Suppressed while a search is on: the line belongs to the conversation's
// order, and filtered results are not it.
eq('search suppresses the line',
  /telSearchFor\('thread'\) \? 0 : telSepFor/.test(src), true);

// The scroll must be instant: a smooth scroll janks the Android WebView
// compositor and the next render interrupts the animation anyway.
const scroll = src.match(/function telScrollToSep\([\s\S]*?\n}/)[0];
eq('scrolls to the top of the line', /block: 'start'/.test(scroll), true);
eq('and instantly', /behavior/.test(scroll), false);
// Returning false when the line is not on screen is what lets a cold open keep
// the intent: the messages arrive after the line is decided.
eq('reports when the line is not on screen yet', /return false/.test(scroll), true);

// --- the layout the line has to live in ------------------------------------

// The message list is a COLUMN flex container, so flex-basis is the HEIGHT.
// `flex: 0 0 100%` made the line as tall as the whole pane, which showed up as
// a long empty gap between "New messages" and the first new post.
const css = readFileSync(join(here, '..', 'static', 'css', 'tel.css'), 'utf8');
// Declarations only: the comments explain the trap by naming the bad value.
const rule = css.match(/\.tel-new-sep \{[\s\S]*?\n}/)[0].replace(/\/\*[\s\S]*?\*\//g, '');
eq('the line is not stretched down the pane', /flex:\s*0 0 100%/.test(rule), false);
eq('it takes its own height', /flex:\s*0 0 auto/.test(rule), true);

// scrollIntoView aligns to the top of the scroll port, which runs behind the
// floating topbar -- the list clears that with padding, which the scroll port
// does not see. Without a margin at least as big, the line lands under the bar.
// The bare `.tel-msgs` rule, anchored: matching any selector that ENDS in
// .tel-msgs picked up a padding from a rule in between and compared against 2.
const msgsRule = css.match(/\n\.tel-msgs \{[\s\S]*?\n}/)[0];
const padTop = Number(msgsRule.match(/\n\s*padding: (\d+)px/)[1]);
const margin = Number(rule.match(/scroll-margin-top: (\d+)px/)[1]);
eq('the scroll anchor clears the floating topbar', margin >= padTop, true);

console.log(`${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
