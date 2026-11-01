// Telegram's sponsored messages: required by the API terms, so they are shown
// -- clearly as an ad -- but fetched last, never holding the link, and cancelled
// by anything the user does.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const src = readFileSync(join(here, '..', 'static', 'js', 'tel.js'), 'utf8');
const i18n = readFileSync(join(here, '..', 'static', 'js', 'i18n.js'), 'utf8');

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
const esc = (x) => String(x).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');

// --- priority ---------------------------------------------------------------
eq('every foreground operation cancels the ad fetch', /telAdAbort\(\)/.test(fn('telOp')), true);
eq('every background operation cancels it too', /telAdAbort\(\)/.test(fn('telBgOp')), true);
eq('the ad fetch never takes the link through telOp/telBgOp',
  /telOp\(|telBgOp\(/.test(fn('telLoadAds')), false);
{
  const open = fn('telOpenPeer');
  eq('opening a channel asks for ads after everything else',
    open.indexOf('telLoadAds(') > open.indexOf('telMarkRead(') &&
    open.indexOf('telLoadAds(') > open.indexOf('telRefreshStats('), true);
}

// --- fetching -----------------------------------------------------------------
const mkLoad = (state, calls, answer) => new Function(
  'telState', 'telThread', 'telPeerKey', 'telRender', 'telAdCheckView', 'telFetch', 'telPost',
  'var telAdCtl = null;' + fn('telAdAbort') + fn('telLoadAds') +
  '; return { load: telLoadAds, abort: telAdAbort };',
)(state, (k) => (state.threads[k] = state.threads[k] || {}), (p) => p ? p.kind + ':' + p.id : '',
  () => { state.renders++; }, () => {},
  (url, opts) => { calls.push({ url, body: JSON.parse(opts.body), signal: opts.signal }); return answer(opts.signal); },
  (b) => ({ method: 'POST', body: JSON.stringify(b) }));

{
  const chan = { kind: 'channel', id: 5 };
  const state = { peer: chan, busy: false, bg: false, threads: {}, renders: 0 };
  const calls = [];
  const ads = [{ randomId: 'AQID', title: 'Shop' }];
  const api = mkLoad(state, calls, async () => ({ ok: true, ads }));
  await api.load('channel:5', chan);
  eq('a channel asks once, with its peer', calls.map((c) => [c.url, c.body]), [['/api/tel/sponsored', { peer: chan }]]);
  eq('the answer lands on the thread', state.threads['channel:5'].ads, ads);
  eq('and is drawn', state.renders, 1);

  calls.length = 0;
  await api.load('group:6', { kind: 'group', id: 6 });
  eq('groups are never asked', calls.length, 0);

  state.busy = true;
  await api.load('channel:5', chan);
  eq('nothing is asked while an operation holds the link', calls.length, 0);
  state.busy = false;

  // The user moved on before the answer came back.
  const late = { kind: 'channel', id: 7 };
  const api2 = mkLoad(state, calls, async () => { state.peer = chan; return { ok: true, ads }; });
  state.peer = late;
  await api2.load('channel:7', late);
  eq('an answer for a channel no longer open is dropped', state.threads['channel:7'], undefined);

  // Cancelled mid-flight by an operation starting.
  let signal = null;
  let release;
  const api3 = mkLoad(state, calls, (s) => { signal = s; return new Promise((res) => { release = res; }); });
  state.peer = chan;
  state.threads = {};
  const pending = api3.load('channel:5', chan);
  api3.abort();
  eq('telAdAbort cancels the request itself', signal && signal.aborted, true);
  release({ ok: true, ads });
  await pending;
}

// --- drawing ------------------------------------------------------------------
{
  const draw = new Function('telEsc', 'telT', 'telAttrQ', 'telSafeText', 'telSafeURL', 'icon',
    fn('telAdHTML') + '; return telAdHTML;')(esc, (k) => '[' + k + ']', esc, (x) => x,
    new Function(fn('telSafeURL') + '; return telSafeURL;')(), () => '');
  const html = draw({ randomId: 'x', title: '<b>Shop</b>', text: 'Buy <img src=x onerror=alert(1)>',
    url: 'https://t.me/shop', button: 'Open shop' });
  eq('labelled as an ad', html.includes('[tel_ad_label]'), true);
  eq('says Telegram placed it, not the channel', html.includes('[tel_ad_note]'), true);
  eq('title and text are escaped', /<b>|<img/.test(html), false);
  eq('the button opens through telAdOpen, not a raw link', /onclick="telAdOpen\(0\)"[^>]*>Open shop</.test(html) && !/href=/.test(html), true);
  eq('a recommendation is labelled as one',
    draw({ randomId: 'x', recommended: true }).includes('[tel_ad_recommended]'), true);
  eq('an unsafe link gets no button', draw({ randomId: 'x', url: 'javascript:alert(1)' }).includes('telAdOpen'), false);
  eq('no button text falls back to Open', draw({ randomId: 'x', url: 'https://example.com' }).includes('[tel_ad_open]'), true);
}

// --- view: once, and only when the whole text is on screen -------------------
{
  const sent = [];
  let textRect = { top: 100, bottom: 160, height: 60 };
  const box = { getBoundingClientRect: () => ({ top: 0, bottom: 500 }),
    querySelector: () => ({ querySelector: () => ({ getBoundingClientRect: () => textRect }) }) };
  const ad = { randomId: 'AQID' };
  const check = new Function('document', 'telAdAt', 'telAdAct', 'telAdViewed',
    fn('telAdCheckView') + '; return telAdCheckView;')(
    { getElementById: () => box }, () => ad, (a, x) => sent.push([a, x.randomId]), {});
  textRect = { top: 450, bottom: 520, height: 70 };
  check();
  eq('half on screen is not a view', sent.length, 0);
  textRect = { top: 100, bottom: 160, height: 60 };
  check(); check();
  eq('fully on screen counts once', sent, [['view', 'AQID']]);
}

// --- report: reasons, then done --------------------------------------------------
{
  const ad = { randomId: 'AQID', canReport: true };
  const state = { peer: { kind: 'channel', id: 5 } };
  const th = { ads: [ad] };
  const calls = [];
  const answers = [
    { ok: true, report: { title: 'Why?', options: [{ text: 'Spam', option: 'UQ==' }] } },
    { ok: true, report: { done: 'reported' } },
  ];
  let sheet = '', toast = '';
  const report = new Function('telCloseSheet', 'telAdAt', 'telWhenIdle', 'telOp', 'telCall', 'telSheet',
    'telSheetItem', 'telEsc', 'telSafeText', 'telState', 'telThread', 'telPeerKey', 'telRender', 'showToast', 'telT',
    'var telAdReportOpts = null;' + fn('telAdReport') + '; return telAdReport;')(
    () => {}, () => ad, async () => {}, async (l, f) => f(),
    async (url, body) => { calls.push(body); return answers.shift(); },
    (h) => { sheet = h; }, (i, label, on) => '<item ' + on + '>' + label + '</item>', (x) => x, (x) => x,
    state, () => th, () => 'channel:5', () => {}, (m) => { toast = m; }, (k) => '[' + k + ']');
  await report(0);
  eq('the first report goes with no option', calls[0], { action: 'report', randomId: 'AQID', option: '' });
  eq('Telegram\'s reasons are offered', /telAdReport\(0,0\)[^>]*>Spam/.test(sheet), true);
  await report(0, 0);
  eq('the picked reason is sent back as Telegram gave it', calls[1].option, 'UQ==');
  eq('a reported ad leaves the screen', th.ads, []);
  eq('and the user is told', toast, '[tel_ad_reported]');
}

eq('connecting declares the UI language to Telegram',
  /\/api\/tel\/connect', \{ server: serverKey, lang:/.test(src), true);

// --- every string exists in both languages ------------------------------------------
for (const k of ['tel_ad_label', 'tel_ad_recommended', 'tel_ad_note', 'tel_ad_open', 'tel_ad_menu',
  'tel_ad_about', 'tel_ad_report', 'tel_ad_reported', 'tel_ad_hidden', 'tel_err_ad_premium', 'tel_err_ad_expired']) {
  eq(k + ' in fa and en', (i18n.match(new RegExp('\\b' + k + ':', 'g')) || []).length, 2);
}
eq('the Persian label says plainly it is an ad', /tel_ad_label: 'تبلیغ'/.test(i18n), true);

console.log(`tel_ads: ${pass} passed, ${fail} failed`);
if (fail) process.exit(1);
