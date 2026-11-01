// What Telegram deleted -- for everyone, or by a self-destruct timer -- must
// leave the open conversation too. Telegram's API terms forbid keeping it.
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
const fn = (name) => {
  const m = src.match(new RegExp('(async )?function ' + name + '\\([^)]*\\) \\{[\\s\\S]*?\\n\\}\\n'));
  if (!m) throw new Error('missing ' + name);
  return m[0];
};

const drop = new Function(fn('telDropDeleted') + '; return telDropDeleted;')();
{
  const th = { msgs: [{ id: 5 }, { id: 6 }, { id: 7 }, { id: 1e12 + 1, pending: true }, { id: 9, failed: true }] };
  eq('deleted ids leave', drop(th, [6, 7, 1e12 + 1, 9]), 2);
  eq('our own unsent messages are never taken for them', th.msgs.map((m) => m.id), [5, 1e12 + 1, 9]);
  eq('nothing deleted, nothing changes', drop(th, []), 0);
  eq('no list at all is fine', drop(th, undefined), 0);
}

const visible = new Function('telSearchFor', fn('telVisibleMessages') + '; return telVisibleMessages;')(() => '');
{
  const now = Date.now() / 1000;
  const th = { msgs: [{ id: 1, expires: now - 5 }, { id: 2, expires: now + 600 }, { id: 3 }, { id: 4, service: true }] };
  eq('a self-destructed message is gone at its time, before any delete arrives',
    visible(th).map((m) => m.id), [2, 3]);
}

{
  const fetchNew = fn('telThreadFetchNew');
  eq('every refresh applies the deleted ids the cache returns',
    /telDropDeleted\(th, cached\.deleted\)/.test(fetchNew), true);
  eq('and redraws when something went', /if \(added \|\| th\.edited \|\| dropped\)/.test(fetchNew), true);
}


// A self-destructed message leaves the open conversation at its time, with one
// redraw, and only when something actually expired.
{
  const now = Math.floor(Date.now() / 1000);
  const state = { peer: { kind: 'user', id: '8' }, threads: {} };
  let renders = 0;
  const tick = new Function('telState', 'telPeerKey', 'telRender',
    fn('telExpireTick') + '; return telExpireTick;')(state, (p) => p.kind + ':' + p.id, () => { renders++; });
  state.threads['user:8'] = { msgs: [{ id: 1 }, { id: 2, expires: now + 3600 }, { id: 3, expires: now - 1 }] };
  tick();
  eq('the expired one goes', state.threads['user:8'].msgs.map((m) => m.id), [1, 2]);
  eq('with one redraw', renders, 1);
  tick();
  eq('nothing expired: no redraw', renders, 1);
  state.peer = null;
  tick();
  eq('no open conversation is fine', renders, 1);
}
eq('it runs every second', /function telTick\(\) \{[\s\S]{0,200}telExpireTick\(\);/.test(src), true);

console.log(`tel_deletes: ${pass} passed, ${fail} failed`);
if (fail) process.exit(1);
