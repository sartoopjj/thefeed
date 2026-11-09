// Each ⟳ refreshes the thing it sits on.
//
// On desktop the sidebar and the open conversation are on screen together, so
// a refresh that picks its target by asking "is a conversation open?" gives
// the sidebar's button the conversation -- and a message arriving in any other
// chat then shows no unread count until the background pass comes round. On
// mobile only one of the two is ever visible, so that reads correct.
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

// Run the real telRefreshNow, recording which refresh it reaches for.
const body = src.match(/async function telRefreshNow\(what\) \{[\s\S]*?\n\}/)[0];
const run = async (what, peer) => {
  const calls = [];
  const state = { peer };
  const fn = new Function(
    'telState', 'telScheduleNext', 'telReady', 'telRefreshInfo', 'telPeerKey',
    'telSteps', 'telStepsDone', 'telThreadFetchNew', 'telRefreshStats',
    'telRefreshDialogs',
    body + '; return telRefreshNow;',
  )(state, () => { }, () => true, async () => { }, p => p && p.id,
    () => { }, () => { }, async () => calls.push('thread'), async () => calls.push('stats'),
    async () => calls.push('list'));
  await fn(what);
  return calls;
};

const openChat = { id: 'c1', title: 'Open one' };

// --- the case from desktop -------------------------------------------------

eq('the sidebar button refreshes the conversation LIST, with a chat open',
  await run('list', openChat), ['list']);
eq('and with no chat open', await run('list', null), ['list']);

// --- the conversation's own button ----------------------------------------

eq('the thread button refreshes the conversation',
  await run('thread', openChat), ['thread', 'stats']);

// A thread refresh with nothing open has no thread to fetch; the list is the
// only honest thing left to do.
eq('and falls back to the list when nothing is open',
  await run('thread', null), ['list']);

// --- the ⋮ menu, which exists in every state ------------------------------

// No target named: refresh whatever is in front, as before.
eq('the menu refreshes the open conversation when there is one',
  await run(undefined, openChat), ['thread', 'stats']);
eq('and the list when there is not', await run(undefined, null), ['list']);

// --- and the buttons actually name their target ---------------------------

// Both headers draw the one refresh button (the countdown) through
// telRefreshBtnHTML, which writes the target into its onclick.
const btnFn = src.match(/function telRefreshBtnHTML\([\s\S]*?\n}/)[0];
eq('the button names the target it was given',
  /onclick="telRefreshNow\(\\'' \+ what \+ '\\'\)"/.test(btnFn), true);
const topFn = src.match(/function telRenderTopbar\([\s\S]*?\n}/)[0];
eq('the sidebar button asks for the list', /telRefreshBtnHTML\('list'\)/.test(topFn), true);
const paneFn = src.match(/function telRenderPane\([\s\S]*?\n}/)[0];
eq('the thread button asks for the thread', /telRefreshBtnHTML\('thread'\)/.test(paneFn), true);

console.log(`${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
