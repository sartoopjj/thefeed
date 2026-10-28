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

console.log(`${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
