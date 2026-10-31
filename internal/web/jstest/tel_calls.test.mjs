// telCall serialises the body itself. Handing it one already wrapped by
// telPost sends {method, headers, body} as the payload, so the backend sees no
// peer and refuses at once -- which is how muting a channel always failed.
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

eq('no call hands telCall a body telPost already wrapped',
  src.match(/telCall(Quiet)?\('[^']*',\s*telPost\(/g), null);

// The real toggle, the real telCall/telPost/telFetch, and a fetch that records
// what actually goes over the wire.
const sent = [];
const peer = { kind: 'channel', id: 42, accessHash: 7 };
const dialog = { peer, muted: false };
const api = new Function('telDialogFor', 'telRender', 'telReason', 'telT', 'telState', 'fetch',
  fn('telPost') + fn('telFetch') + fn('telCall') + fn('telToggleMute') + '; return telToggleMute;',
)(() => dialog, () => {}, () => 'err', (k) => k, { error: '' },
  async (url, opts) => {
    sent.push({ url, body: opts && JSON.parse(opts.body) });
    return { ok: true, status: 200, json: async () => ({ ok: true, muted: true }) };
  });

await api('channel:42');
eq('the mute request carries the conversation and the setting',
  sent.map((s) => s.body), [{ peer, muted: true }]);
eq('and the switch stays where it was flipped', dialog.muted, true);

// --- the shared-key warning ----------------------------------------------------
{
  const banner = new Function('telEsc', 'telT', fn('telDefaultCredsBanner') + '; return telDefaultCredsBanner;')(
    (x) => x, (k) => '[' + k + ']');
  eq('the shared key in use: warn and offer the user\'s own',
    /\[tel_default_creds_warn\][\s\S]*telEditCredentials\(\)[\s\S]*\[tel_use_own_creds\]/.test(banner({ defaultCredentials: true })), true);
  eq('the user\'s own key in use: no warning', banner({ defaultCredentials: false }), '');
  eq('no info yet: no warning', banner(null), '');
  const creds = fn('telRenderCredentials');
  eq('back-to-default is offered only when the build has one and it is not in use',
    /info\.defaultAvailable && !info\.defaultCredentials/.test(creds), true);
  eq('switching back sends useDefault, never a pair',
    /telCall\('\/api\/tel\/credentials', \{ useDefault: true \}\)/.test(fn('telUseDefaultCredentials')), true);
}

console.log(`tel_calls: ${pass} passed, ${fail} failed`);
if (fail) process.exit(1);
