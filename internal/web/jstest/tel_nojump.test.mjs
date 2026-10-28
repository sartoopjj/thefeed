// The progress strip must not move anything when it comes and goes.
//
// It appears on every background check and leaves when the check ends, so any
// shift it causes happens once a minute under the reader's eyes. It used to
// shift both lists, by two different mechanisms: in the sidebar it was an
// ordinary child and every row dropped by its height; in the conversation the
// list's top padding appeared with it.
//
// Compensating the scroll afterwards was worse -- the paint runs constantly,
// so every rounding error accumulated and the list crept downwards on its own.
// The room is simply reserved at all times instead, so there is nothing to
// compensate and nothing to go wrong.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const css = readFileSync(join(here, '..', 'static', 'css', 'tel.css'), 'utf8');
const js = readFileSync(join(here, '..', 'static', 'js', 'tel.js'), 'utf8');

let pass = 0, fail = 0;
const ok = (name, cond) => { if (cond) pass++; else { fail++; console.error('FAIL ' + name); } };

// The strip floats in BOTH panes, so it takes no layout space in either.
const floatRule = css.match(/#telSidebar \.tel-prog-wrap,\n#telPane \.tel-prog-wrap \{[\s\S]*?\n\}/);
ok('the strip floats in both panes', !!floatRule);
ok('and is positioned out of flow', /position: absolute/.test(floatRule ? floatRule[0] : ''));

// No padding that appears and disappears with it: the conversation's top
// padding is one constant, whatever the strip is doing.
ok('no padding toggled by the strip', !/html\.tel-prog-on[^{]*\.tel-msgs \{/.test(css));
const msgsPad = Number(css.match(/^\.tel-msgs \{[^}]*padding: (\d+)px/m)[1]);
ok('the conversation padding is a fixed amount', msgsPad > 0);

// The separator's scroll anchor has to match that padding, or scrolling to the
// "new messages" line puts it somewhere else than the top of the list. Checked
// as a RELATIONSHIP, so changing one and forgetting the other fails here
// rather than on screen.
const sepAnchor = Number(css.match(/^\.tel-new-sep \{[^}]*scroll-margin-top: (\d+)px/m)[1]);
ok('the separator anchor tracks that padding', Math.abs(sepAnchor - msgsPad) <= 10);
ok('and no state class changes it', !/tel-prog-on[^{]*\.tel-new-sep/.test(css));

// The scroll-compensation experiment must not come back: it drifted.
const paint = js.match(/function telPaintProgress\([\s\S]*?\n}/)[0];
ok('the paint does not move any scroller', !/scrollTop\s*=/.test(paint));
ok('and does not measure positions to do it', !/getBoundingClientRect/.test(paint));

// --- and it must not cover the controls under it --------------------------

// The strip floats, so room is reserved for it in the list. Reserved room is a
// fixed amount, which only works if the strip is a fixed height: left to wrap,
// the label ran to four lines in the narrow sidebar, the strip grew to 135px,
// and it covered the tab filters and the first rows again.
const labelRule = css.match(/\.tel-prog-label \{[\s\S]*?\n\}/)[0];
ok('the status is one line', /white-space: nowrap/.test(labelRule));
ok('and trims rather than wraps', /text-overflow: ellipsis/.test(labelRule));

// The room reserved must cover where the strip ends.
const room = Number(css.match(/--tel-prog-room:\s*(\d+)px/)[1]);
const top = Number(css.match(/--tel-prog-top:\s*(\d+)px/)[1]);
ok('room is reserved for the strip', room > 0);
// Measured: a one-line strip is 34px tall. The reservation has to clear that
// plus the gap between it and the list's own top.
ok('and the reservation clears a one-line strip', room >= 34);
ok('the list uses that reservation', /padding: var\(--tel-prog-room\)/.test(css));
ok('the strip is positioned from the same pair', /top: var\(--tel-prog-top\)/.test(css));
void top;

console.log(`${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
