// The progress strip must not strobe on a working link.
//
// One user action is several round trips -- messages, then views, then
// reactions -- and each is its own operation. At tens of seconds each the
// strip looks continuous; at a couple of hundred milliseconds each, a strip
// bound to one operation goes on and off per round trip.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const src = readFileSync(join(here, '..', 'static', 'js', 'tel.js'), 'utf8');
const css = readFileSync(join(here, '..', 'static', 'css', 'tel.css'), 'utf8');

let pass = 0, fail = 0;
const eq = (name, got, want) => {
  if (JSON.stringify(got) === JSON.stringify(want)) { pass++; return; }
  fail++;
  console.error(`FAIL ${name}\n  got  ${JSON.stringify(got)}\n  want ${JSON.stringify(want)}`);
};

// Run the real visibility owner out of tel.js against a fake clock and DOM.
const block = src.match(/var TEL_PROG_LINGER_MS[\s\S]*?\nfunction telProgApply\(\) \{[\s\S]*?\n\}/)[0];

function harness() {
  let now = 0;
  const timers = [];
  const shown = [];           // every visibility CHANGE the DOM would see
  const root = { on: false };
  const fakeDoc = {
    documentElement: {
      classList: {
        toggle(_c, v) {
          if (root.on !== v) { root.on = v; shown.push({ at: now, on: v }); }
        },
      },
    },
    querySelectorAll: () => [],
  };
  const api = new Function(
    'document', 'setTimeout', 'clearTimeout', 'Date',
    block + '; return { show: telProgShow };',
  )(fakeDoc, (fn, ms) => { timers.push({ at: now + ms, fn }); return timers.length; },
    (id) => { if (id) timers[id - 1] = null; },
    { now: () => now });
  return {
    show: api.show,
    advance(ms) {
      const end = now + ms;
      for (; ;) {
        const due = timers.filter(t => t && t.at <= end).sort((a, b) => a.at - b.at)[0];
        if (!due) break;
        now = due.at;
        timers[timers.indexOf(due)] = null;
        due.fn();
      }
      now = end;
    },
    get changes() { return shown; },
    get visible() { return root.on; },
  };
}

// --- a burst of fast operations is ONE bar ---------------------------------

// Three round trips of 200ms each with 50ms between them: what a working
// link does. The strip must come up once and go down once.
let h = harness();
for (let i = 0; i < 3; i++) {
  h.show(true); h.advance(200);
  h.show(false); h.advance(50);
}
h.advance(2000);
eq('a burst of three fast operations shows one bar',
  h.changes.map(c => c.on), [true, false]);
eq('and it is down once the burst is over', h.visible, false);

// --- a single quick operation is still readable ----------------------------

// 80ms of work must not be an 80ms flash.
h = harness();
h.show(true); h.advance(80); h.show(false);
h.advance(300);
eq('a very short operation is still on screen 380ms later', h.visible, true);
h.advance(2000);
eq('and it does come down', h.visible, false);

// --- a long operation is unaffected ----------------------------------------

h = harness();
h.show(true); h.advance(30000);
eq('a long fetch keeps the bar up', h.visible, true);
h.show(false); h.advance(1000);
eq('and it goes down after it ends', h.visible, false);

// --- it is never left up ---------------------------------------------------

// A hide scheduled and then overtaken by a new operation must not leave a
// stale timer that hides the strip mid-operation.
h = harness();
h.show(true); h.advance(1000);
h.show(false); h.advance(100);   // hide pending
h.show(true); h.advance(5000);   // a new operation claims it
eq('a new operation cancels a pending hide', h.visible, true);

// --- the strip stays out of the layout ------------------------------------

// The reason it may float over the messages at all: it takes no space there,
// so showing it cannot move the conversation.
eq('the strip is absolutely positioned in both panes',
  /#telSidebar \.tel-prog-wrap,\s*\n#telPane \.tel-prog-wrap \{\s*\n\s*position: absolute;/.test(css), true);

// And the conversation reserves only the topbar, not the strip, so there is no
// dead band above the first message. The sidebar still reserves it, because
// there the strip covers rows the user has to tap.
// Anchored at the start of a line so `#telPane .tel-msgs` cannot stand in for
// `.tel-msgs`, and bounded by the closing brace so the number has to come from
// inside that rule.
const ruleOf = (sel) => {
  const m = css.match(new RegExp('^' + sel.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') + ' \\{([^}]*)\\}', 'm'));
  if (!m) throw new Error('no rule for ' + sel);
  return m[1];
};
const padTop = (body) => {
  const m = body.match(/padding(?:-top)?: (\d+)px/);
  if (!m) throw new Error('no top padding in ' + JSON.stringify(body));
  return Number(m[1]);
};
// The topbar ends at 60px, the strip floats at 66-100px. Reserving only the
// topbar is ~68px; reserving the strip too was 108px and showed as a dead band.
eq('the conversation reserves the topbar only, not the strip',
  padTop(ruleOf('.tel-msgs')) < 100, true);
eq('and it still clears the topbar', padTop(ruleOf('.tel-msgs')) >= 60, true);
// The sidebar DOES reserve it: there the strip covers rows the user must tap.
eq('the sidebar still reserves room for the strip',
  /padding: var\(--tel-prog-room\)/.test(ruleOf('.tel-body')), true);

console.log(`${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
