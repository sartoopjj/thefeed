// Telegram login when the code does not come by the app or SMS.
//
// Telegram now often sends the login code by email, or sends nothing and asks
// for a login email first ("setupemail"). The screen says which inbox, offers
// the add-an-email step, and signs in with an email code in the field
// Telegram expects it in.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const src = readFileSync(join(here, '..', 'static', 'js', 'tel.js'), 'utf8');
const i18n = readFileSync(join(here, '..', 'static', 'js', 'i18n.js'), 'utf8');

let pass = 0, fail = 0;
function eq(name, got, want) {
  if (JSON.stringify(got) === JSON.stringify(want)) { pass++; return; }
  fail++;
  console.error(`FAIL ${name}\n  got  ${JSON.stringify(got)}\n  want ${JSON.stringify(want)}`);
}
const grab = (name) => src.match(new RegExp('(?:async )?function ' + name + '\\([\\s\\S]*?\\n}'))[0];
const esc = (x) => String(x).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');

const names = ['telTakeCode', 'telSignedIn', 'telResendCode', 'telSignIn', 'telSetupEmail', 'telVerifyEmail',
  'telChangeEmail', 'telEditPhone', 'telRenderCodeVia', 'telRenderEmailSetup', 'telRenderLogin', 'telLTR'];
const body = 'var window = {};\n' + names.map(grab).join('\n') + '\nreturn {' + names.map((n) => n + ':' + n).join(',') + '};';

// world: the login screen, with the backend answering from `answers` (one per
// call, in order) and the inputs holding `inputs`.
function world() {
  const st = { phone: '+15550001', codeHash: '', codeVia: '', codeEmail: '', emailSent: '', needPassword: false,
    passwordHint: '', busy: false, error: '', info: {} };
  const w = { st, calls: [], answers: [], inputs: {}, log: [] };
  const doc = { getElementById: (id) => (id in w.inputs ? { value: w.inputs[id] } : null) };
  w.api = new Function('telState', 'telT', 'telEsc', 'telRender', 'document', 'telOp', 'telCall', 'telFetch',
    'telPost', 'telReason', 'telResetData', 'telRefreshInfo', 'telReady', 'telLoadDialogs',
    'telDefaultCredsBanner', 'telDialSelectHTML', 'telDialCode', 'telAttrQ', body)(
    st, (k) => '[' + k + ' {email}]', esc, () => w.log.push('render'), doc,
    async (label, fn) => { w.log.push('op ' + label); await fn(); },
    async (url, b) => { w.calls.push(b); const r = w.answers.shift(); return r && r.ok ? r : null; },
    async (url, b) => { w.calls.push(b); return w.answers.shift(); },
    (b) => b, (r, k) => r.reason || k,
    () => w.log.push('reset'), async () => w.log.push('refresh'), () => false, () => w.log.push('dialogs'),
    () => '', () => '', () => '+1', esc);
  w.screen = () => w.api.telRenderLogin();
  return w;
}
const has = (html, id) => html.includes('id="' + id + '"');
const text = (h) => h.replace(/<[^>]*>/g, ' ').replace(/\s+/g, ' ').trim();

// The full add-an-email path, from "no code was sent" to signed in.
{
  const w = world();
  w.st.codeHash = 'h1'; w.st.codeVia = 'app';
  // Resend: Telegram now says it sends no code until an email is added.
  w.answers.push({ ok: true, codeHash: 'h1', via: 'setupemail' });
  await w.api.telResendCode();
  let html = w.screen();
  eq('setupemail: asks for an email', has(html, 'telEmail'), true);
  eq('setupemail: no code field to wait at', has(html, 'telCode'), false);
  eq('setupemail: says why', html.includes('[tel_email_setup_intro'), true);

  // A typo is caught here, costing no query.
  w.inputs.telEmail = 'not-an-email';
  await w.api.telSetupEmail();
  eq('bad address: no call', w.calls.length, 1);
  eq('bad address: says so', w.st.error.startsWith('[tel_err_email_invalid'), true);

  w.inputs.telEmail = ' u@m.org ';
  w.answers.push({ ok: true, emailPattern: 'u***@m***.org' });
  await w.api.telSetupEmail();
  eq('setup request', w.calls[1], { action: 'emailsetup', phone: '+15550001', codeHash: 'h1', email: 'u@m.org' });
  html = w.screen();
  eq('then asks for the code sent there', has(html, 'telEmailCode'), true);
  eq('naming the masked address, isolated LTR', html.includes('⁦u***@m***.org⁩'), true);

  // Changing the address is local.
  w.api.telChangeEmail();
  eq('change email: back to the address', has(w.screen(), 'telEmail'), true);
  eq('change email: no call', w.calls.length, 2);
  w.answers.push({ ok: true, emailPattern: 'u***@m***.org' });
  await w.api.telSetupEmail();

  // The setup code: Telegram then sends the LOGIN code, with a new hash.
  w.inputs.telEmailCode = ' 777 ';
  w.answers.push({ ok: true, codeHash: 'h2', via: 'email', emailPattern: 'u***@m***.org' });
  await w.api.telVerifyEmail();
  eq('verify request', w.calls[3], { action: 'emailverify', phone: '+15550001', codeHash: 'h1', code: '777' });
  eq('new hash replaces the old', w.st.codeHash, 'h2');
  html = w.screen();
  eq('now the usual code field', has(html, 'telCode'), true);
  eq('which says which inbox', html.includes('[tel_code_via_email_to'), true);
  eq('with the masked address, isolated LTR', html.includes('⁦u***@m***.org⁩'), true);
  eq('no "it came in your Telegram" line under an email code', html.includes('[tel_code_hint'), false);

  // Sign-in says the code came by email, so it goes in the email field.
  w.inputs.telCode = '55555';
  w.answers.push({ ok: true });
  await w.api.telSignIn();
  eq('signin request', w.calls[4], { action: 'signin', phone: '+15550001', code: '55555', codeHash: 'h2', via: 'email' });
  eq('signed in: fresh start', w.log.includes('reset') && w.log.includes('refresh'), true);
  eq('signed in: login step cleared', [w.st.codeHash, w.st.codeVia, w.st.codeEmail], ['', '', '']);
}

// An app or SMS code still signs in as before, its via passed along.
{
  const w = world();
  w.st.codeHash = 'h'; w.st.codeVia = 'app';
  eq('app code: keeps the "in your Telegram" line', w.screen().includes('[tel_code_hint'), true);
  w.inputs.telCode = '1';
  w.answers.push({ ok: true });
  await w.api.telSignIn();
  eq('app code: via app', w.calls[0].via, 'app');
}

// Telegram may sign in without any further code.
{
  const w = world();
  w.st.codeHash = 'h1'; w.st.codeVia = 'setupemail'; w.st.emailSent = 'u***';
  w.inputs.telEmailCode = '1';
  w.answers.push({ ok: true, done: true });
  await w.api.telVerifyEmail();
  eq('done: signed in', w.log.includes('reset') && w.log.includes('refresh'), true);
  eq('done: no code step left', w.st.codeHash, '');
}

// A refused verify leaves the user on the email-code step to try again.
{
  const w = world();
  w.st.codeHash = 'h1'; w.st.codeVia = 'setupemail'; w.st.emailSent = 'u***';
  w.inputs.telEmailCode = '1';
  w.answers.push({ ok: false, reason: 'tel_err_code_invalid' });
  await w.api.telVerifyEmail();
  eq('refused: same hash', w.st.codeHash, 'h1');
  eq('refused: still on the email code', has(w.screen(), 'telEmailCode'), true);
}

// Hints: the other deliveries, and nothing from Telegram is ever markup.
{
  const w = world();
  w.st.codeHash = 'h';
  w.st.codeVia = 'firebase';
  eq('firebase: says only the official app gets it', w.api.telRenderCodeVia().includes('[tel_code_via_firebase'), true);
  w.st.codeVia = 'email'; w.st.codeEmail = '';
  eq('email, no address: the plain line', w.api.telRenderCodeVia().includes('[tel_code_via_email '), true);
  w.st.codeVia = 'email'; w.st.codeEmail = '<img src=x onerror=alert(1)>$&';
  const h = w.api.telRenderCodeVia();
  eq('address is escaped', h.includes('<img'), false);
  w.st.codeVia = 'setupemail'; w.st.emailSent = '<b>x</b>';
  eq('setup address is escaped', w.screen().includes('<b>x'), false);
  // Editing the number drops the whole half-finished step.
  w.api.telEditPhone();
  eq('edit phone clears the email step', [w.st.codeHash, w.st.codeVia, w.st.codeEmail, w.st.emailSent], ['', '', '', '']);
}

// Every text the login screen uses exists in both languages.
{
  const keys = ['tel_code_via_email', 'tel_code_via_email_to', 'tel_code_via_firebase', 'tel_email_setup_intro',
    'tel_email_setup_hint', 'tel_email', 'tel_email_code_sent', 'tel_change_email', 'tel_err_email_invalid',
    'tel_err_no_account'];
  for (const k of keys) {
    const n = (i18n.match(new RegExp('\\n\\s+' + k + ':', 'g')) || []).length;
    eq('i18n ' + k + ' in fa and en', n, 2);
  }
  eq('{email} placeholder in both', (i18n.match(/tel_code_via_email_to: '[^'\n]*\{email\}/g) || []).length, 2);
}

console.log(`${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
