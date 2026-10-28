// telGuessReactions is what the user sees the instant they tap, before the
// DNS round trip answers. Getting it wrong shows a count that is simply
// untrue, so every transition is pinned here.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const src = readFileSync(join(here, '..', 'static', 'js', 'tel.js'), 'utf8');
const fn = src.match(/function telGuessReactions\([\s\S]*?\n}/)[0];
const telGuessReactions = new Function(fn + '; return telGuessReactions;')();

let pass = 0, fail = 0;
function eq(name, got, want) {
  const g = JSON.stringify(got), w = JSON.stringify(want);
  if (g === w) { pass++; return; }
  fail++;
  console.error(`FAIL ${name}\n  got  ${g}\n  want ${w}`);
}

// Adding one nobody has picked yet.
eq('new emoji appears with count 1',
  telGuessReactions([{ emoji: '👍', count: 3, chosen: false }], '❤️', true),
  [{ emoji: '👍', count: 3, chosen: false }, { emoji: '❤️', count: 1, chosen: true }]);

// Joining a reaction others already left.
eq('existing emoji gains one',
  telGuessReactions([{ emoji: '👍', count: 3, chosen: false }], '👍', true),
  [{ emoji: '👍', count: 4, chosen: true }]);

// Taking your own back: the count drops and, at zero, the pill goes.
eq('removing your own drops the count',
  telGuessReactions([{ emoji: '👍', count: 4, chosen: true }], '👍', false),
  [{ emoji: '👍', count: 3, chosen: false }]);
eq('a reaction only you left disappears',
  telGuessReactions([{ emoji: '👍', count: 1, chosen: true }], '👍', false),
  []);

// Switching: Telegram releases the old choice as it takes the new one, so the
// old count must fall in the same step the new one rises.
eq('switching moves the vote, it does not duplicate it',
  telGuessReactions([
    { emoji: '👍', count: 4, chosen: true },
    { emoji: '❤️', count: 2, chosen: false },
  ], '❤️', true),
  [{ emoji: '👍', count: 3, chosen: false }, { emoji: '❤️', count: 3, chosen: true }]);

eq('switching away from one only you left removes it',
  telGuessReactions([
    { emoji: '👍', count: 1, chosen: true },
    { emoji: '❤️', count: 2, chosen: false },
  ], '❤️', true),
  [{ emoji: '❤️', count: 3, chosen: true }]);

eq('no reactions yet', telGuessReactions([], '🔥', true),
  [{ emoji: '🔥', count: 1, chosen: true }]);

console.log(`${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
