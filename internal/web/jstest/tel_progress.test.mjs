// What the progress strip says while a round trip is in flight.
//
// The counter counts cells going OUT, and its total is not cleared when the
// last one goes. So once everything had been sent the strip sat on a
// finished-looking "10 of 10" for the whole time the answer was coming back —
// which, over DNS, is most of the wait. It read as done while nothing moved.
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

// Run the real branch out of tel.js, so loosening it fails these cases.
// The real branch out of tel.js, so loosening it fails these cases.
const block = src.match(/var sent = info\.sentCells[\s\S]*?\n  \} else if \(running\) \{[\s\S]*?\n  \}/)[0];
const run = (info, running) => new Function(
  'info', 'running', 'telT', 'telFmtBytes',
  'var label = ""; ' + block + '; return { label: label, pct: pct };',
)(info, running, k => k, n => n + 'B');

// --- going out: a count with a known end -----------------------------------

// The counter is cells the relay has ACKNOWLEDGED, so it is real progress and
// it has a total: the user can see how far along the upload is.
eq('counts acknowledged cells while uploading',
  run({ sentCells: 3, totalCells: 10 }, true), { label: ' · tel_of_queries', pct: 30 });
eq('and fills the bar with it',
  run({ sentCells: 9, totalCells: 10 }, true).pct, 90);

// --- coming back: what is left, because there is no total ------------------

// The upload's total is NOT cleared when its last cell lands, so showing the
// counter past that point left a finished "10 of 10" on screen for the whole
// time the answer was still coming — over DNS, most of the wait.
eq('switches to what is left once the upload is done',
  run({ sentCells: 10, totalCells: 10, downLeft: 1200 }, true),
  { label: ' · tel_receiving', pct: 0 });
eq('never leaves a finished counter on screen',
  run({ sentCells: 7, totalCells: 7, downLeft: 0 }, true).label.includes('tel_of_queries'),
  false);
eq('and the bar does not sit full',
  run({ sentCells: 7, totalCells: 7, downLeft: 0 }, true).pct, 0);

// Nothing known yet: waiting is the only true statement.
eq('says it is waiting before the relay has said anything',
  run({ sentCells: 10, totalCells: 10, downLeft: 0 }, true),
  { label: ' · tel_awaiting_reply', pct: 0 });
eq('and with no upload at all',
  run({ downLeft: 0 }, true), { label: ' · tel_awaiting_reply', pct: 0 });

// Nothing running: no label, no bar.
eq('idle says nothing', run({ sentCells: 3, totalCells: 10 }, false), { label: '', pct: 0 });

console.log(`${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
