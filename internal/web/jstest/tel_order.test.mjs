// Message order after a send that settles from the send's own answer rather
// than from a read-back. A placeholder sorts last on a synthetic id, so
// anything that arrived while it was in flight has to be put back in front of
// it once the real id lands.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const src = readFileSync(join(here, '..', 'static', 'js', 'tel.js'), 'utf8');
const merge = src.match(/function telMergeMessages\([\s\S]*?\n}/)[0];
const telMergeMessages = new Function(merge + '; return telMergeMessages;')();

// What telSend does once Telegram reports the id it assigned.
const settle = (() => {
  const body = src.match(/pending\.id = r\.msgId;[\s\S]*?telTouchPreview/)[0];
  if (!/sort\(/.test(body)) {
    throw new Error('telSend no longer reorders after settling a sent message');
  }
  return (th, pending, id) => {
    pending.id = id;
    pending.pending = false;
    th.msgs.sort((a, b) => a.id - b.id);
  };
})();

const PENDING_BASE = 1e12;
let pass = 0, fail = 0;
function eq(name, got, want) {
  const g = JSON.stringify(got), w = JSON.stringify(want);
  if (g === w) { pass++; return; }
  fail++;
  console.error(`FAIL ${name}\n  got  ${g}\n  want ${w}`);
}
const ids = th => th.msgs.map(m => m.id);

// The case: we send, and THEIR message lands before our id comes back.
{
  const th = { msgs: [{ id: 100, text: 'hi' }] };
  const pending = { id: PENDING_BASE + 1, text: 'mine', out: true, pending: true };
  th.msgs.push(pending);
  // Their message arrives while ours is still in flight.
  telMergeMessages(th, [{ id: 102, text: 'theirs' }]);
  eq('placeholder still sorts last while in flight', ids(th), [100, 102, PENDING_BASE + 1]);
  settle(th, pending, 103);
  eq('ours settles after theirs', ids(th), [100, 102, 103]);
}

// Same, but Telegram assigned ours the LOWER id: it must move in front.
{
  const th = { msgs: [{ id: 100, text: 'hi' }] };
  const pending = { id: PENDING_BASE + 1, text: 'mine', out: true, pending: true };
  th.msgs.push(pending);
  telMergeMessages(th, [{ id: 102, text: 'theirs' }]);
  settle(th, pending, 101);
  eq('ours settles in front of a later message', ids(th), [100, 101, 102]);
}

// Two of ours in flight at once stay in the order they were sent.
{
  const th = { msgs: [{ id: 100 }] };
  const p1 = { id: PENDING_BASE + 1, pending: true };
  const p2 = { id: PENDING_BASE + 2, pending: true };
  th.msgs.push(p1, p2);
  settle(th, p1, 101);
  settle(th, p2, 102);
  eq('two sends keep their order', ids(th), [100, 101, 102]);
}

// A later fetch that adds nothing must not disturb what we settled.
{
  const th = { msgs: [{ id: 100 }] };
  const pending = { id: PENDING_BASE + 1, pending: true };
  th.msgs.push(pending);
  settle(th, pending, 101);
  const added = telMergeMessages(th, [{ id: 101 }]);
  eq('the echo is not duplicated', [added, ids(th)], [0, [100, 101]]);
}

console.log(`${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
