// Picking another config in the shield sheet switches to it, even while the
// current link is busy; picking the live one does nothing.
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

function harness(info, flags) {
  const state = Object.assign({ info, busy: false, bg: false, codeHash: 'h', codeVia: 'sms', needPassword: true }, flags);
  const seen = { calls: [], connects: [], aborts: 0, follows: 0 };
  const api = new Function('telState', 'telCall', 'telConnect', 'telRefreshInfo', 'telFollowConnect', 'telRender',
    'telHistAbort', 'lang', 'seen',
    fn('telPickServer') + fn('telSwitchServer') + '; return { pick: telPickServer };',
  )(state, async (url, body) => { seen.calls.push([url, body.server]); return { ok: true }; },
    async (k) => { seen.connects.push(k); }, async () => {}, () => { seen.follows++; }, () => {},
    () => { seen.aborts++; }, 'fa', seen);
  return { state, seen, api };
}

{
  const { seen, api, state } = harness({ connected: true, server: 'a' }, { bg: true, busy: true });
  await api.pick('b');
  eq('busy with the live link: the switch still goes out', seen.calls, [['/api/tel/connect', 'b']]);
  eq('the open fetch is let go', seen.aborts, 1);
  eq('and the new connect is followed', seen.follows, 1);
  eq('the half-finished login is dropped', [state.codeHash, state.needPassword], ['', false]);
}
{
  const { seen, api } = harness({ connecting: true, server: '' , resumeServer: 'a' }, {});
  await api.pick('b');
  eq('while another config is connecting, too', seen.calls, [['/api/tel/connect', 'b']]);
}
{
  const { seen, api, state } = harness({ connected: true, server: 'a' }, {});
  await api.pick('a');
  eq('the live config: nothing', [seen.calls.length, seen.connects.length], [0, 0]);
  eq('and its login is kept', state.codeHash, 'h');
}
{
  const { seen, api } = harness({ connected: false, resumeServer: 'a' }, {});
  await api.pick('b');
  eq('no live link: the ordinary connect', seen.connects, ['b']);
}
{
  const { seen, api } = harness({ connected: false }, { busy: true });
  await api.pick('b');
  eq('no live link and busy: waits, as before', seen.connects.length + seen.calls.length, 0);
}

console.log(`tel_switch: ${pass} passed, ${fail} failed`);
if (fail) process.exit(1);
