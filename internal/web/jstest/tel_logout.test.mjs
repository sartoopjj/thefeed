// Signing out of Telegram.
//
// The account lives in a file on this machine, so removing it is local and
// must always be possible — most of all during a shutdown, which is exactly
// when someone needs their account off their device and exactly when nothing
// is connected.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const src = readFileSync(join(here, '..', 'static', 'js', 'tel.js'), 'utf8');

let pass = 0, fail = 0;
const ok = (name, cond) => { if (cond) pass++; else { fail++; console.error('FAIL ' + name); } };

const fn = src.match(/async function telLogout\([\s\S]*?\n}/)[0];

// telOp returns at once and does NOTHING when another operation holds the
// link, so routing sign-out through it meant pressing "log out" during a
// fetch did nothing at all — no error, no screen change.
ok('does not take the operation lock', !/telOp\(/.test(fn));

// Whatever is in flight is abandoned rather than waited for.
ok('abandons an in-flight fetch', /telHistAbort\(\)/.test(fn));

// The screen reflects it before any request goes out.
const clearsAt = fn.indexOf('telState.authorized = false');
const fetchesAt = fn.indexOf("telFetch('/api/tel/auth'");
ok('clears the signed-in state', clearsAt > 0);
ok('and does so BEFORE asking the backend', clearsAt < fetchesAt);
ok('drops the cached conversations too', /telResetData\(\)/.test(fn));
ok('repaints immediately', fn.slice(clearsAt, fetchesAt).includes('telRender()'));

// A failed request must not leave the user looking signed in.
ok('a failed request is swallowed, not surfaced', /catch \(e\) \{ \}/.test(fn));

// The question before signing out goes through the app's own dialog. The iOS
// WebView has no handler for window.confirm, so there it answers "no" without
// showing anything and the button did nothing at all.
const grab = (name) => src.match(new RegExp('async function ' + name + '\\([\\s\\S]*?\\n}'))[0];
const names = ['telLogout', 'telUseDefaultCredentials', 'telSaveCredentials'];
const body = names.map(grab).join('\n') + '\nreturn {' + names.join(',') + '};';

// world runs the three with confirm() behaving as on iOS, and the app dialog
// answering `answer`.
function world(answer) {
  const w = { asked: [], native: 0, calls: [], st: null };
  w.st = { authorized: true, user: { id: 1 }, info: { hasSession: true, apiId: 1, apiHash: 'a' } };
  const doc = { getElementById: (id) => ({ telApiId: { value: '2' }, telApiHash: { value: 'b' } })[id] || null };
  w.api = new Function('telState', 'telT', 'confirm', 'showConfirmDialog', 'telCloseSheet', 'telHistAbort',
    'telResetData', 'telRender', 'telFetch', 'telPost', 'telRefreshInfo', 'telOp', 'telCall', 'document', body)(
    w.st, (k) => k, () => { w.native++; return false; },
    async (msg) => { w.asked.push(msg); return answer; },
    () => {}, () => {}, () => {}, () => {},
    async (url, b) => { w.calls.push(b); return {}; }, (b) => b, async () => {},
    async (label, f) => { await f(); },
    async (url, b) => { w.calls.push(b); return { ok: true, signedOut: true }; }, doc);
  return w;
}

{
  const w = world(true);
  await w.api.telLogout();
  ok('logout: asks in the app dialog', w.asked.length === 1 && w.asked[0] === 'tel_logout_confirm');
  ok('logout: yes signs out', w.st.authorized === false && w.st.user === null);
  ok('logout: yes tells the backend', w.calls.length === 1 && w.calls[0].action === 'logout');
  ok('logout: never uses window.confirm', w.native === 0);
}
{
  const w = world(false);
  await w.api.telLogout();
  ok('logout: no keeps the account', w.st.authorized === true && w.calls.length === 0);
}
for (const name of ['telUseDefaultCredentials', 'telSaveCredentials']) {
  const yes = world(true);
  await yes.api[name]();
  ok(name + ': asks in the app dialog', yes.asked.length === 1 && yes.asked[0] === 'tel_creds_signout_confirm');
  ok(name + ': yes goes ahead', yes.calls.length === 1 && yes.st.authorized === false);
  ok(name + ': never uses window.confirm', yes.native === 0);
  const no = world(false);
  await no.api[name]();
  ok(name + ': no changes nothing', no.calls.length === 0 && no.st.authorized === true);
}

console.log(`${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
