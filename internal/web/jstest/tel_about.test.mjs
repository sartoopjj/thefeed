// A channel's description in its info sheet. It is read when the sheet opens,
// unless a read within the day (the chat's own member step brings it too)
// already has it: one call, at most once a day per channel. A read that fails
// leaves the old text on screen; with nothing to show it says so.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const src = readFileSync(join(here, '..', 'static', 'js', 'tel.js'), 'utf8');

let pass = 0, fail = 0;
function eq(name, got, want) {
  if (JSON.stringify(got) === JSON.stringify(want)) { pass++; return; }
  fail++;
  console.error(`FAIL ${name}\n  got  ${JSON.stringify(got)}\n  want ${JSON.stringify(want)}`);
}
const grab = (name) => src.match(new RegExp('(?:async )?function ' + name + '\\([\\s\\S]*?\\n}'))[0];
const esc = (x) => String(x).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
const tick = () => new Promise((r) => setTimeout(r, 0));

const body = 'var telInfoFlight = {};\nvar telAboutCtl = null;\n' + ['telAboutAbort', 'telHasFullInfo', 'telHasAbout', 'telAboutWord', 'telNoteInfo', 'telInfoURL', 'telAskInfo', 'telAboutHTML',
  'telAboutShown', 'telPaintAbout', 'telProfileAbout'].map(grab).join('\n') +
  '\nreturn { telProfileAbout: telProfileAbout, telAskInfo: telAskInfo, telAboutHTML: telAboutHTML, telAboutAbort: telAboutAbort };';

// world builds the page around one open info sheet: the backend's cache
// (cached), what Telegram answers (live; null is a failed read), the tunnel.
function world({ cached, live, busy = false, about = {}, peer = { kind: 'channel', id: '9', accessHash: '3' } }) {
  const key = peer.kind + ':' + peer.id;
  const st = { peer, members: {}, membersFresh: {}, about, aboutBusy: {}, aboutFailed: {}, busy, bg: false };
  const log = [];
  // The sheet's description slot; every paint of it is kept.
  const slot = { attrs: { 'data-key': key }, html: '', shown: [], getAttribute(k) { return this.attrs[k]; },
    get innerHTML() { return this.html; }, set innerHTML(v) { this.html = v; this.shown.push(v); } };
  const doc = { getElementById: (id) => (id === 'telProfileAbout' && w.sheetOpen ? slot : null) };
  const waiters = [];
  const w = {
    st, log, slot, key, peer, sheetOpen: true, cached, live,
    // The tunnel frees up; whatever waited goes on.
    idle() { st.busy = false; st.bg = false; waiters.splice(0).forEach((r) => r()); },
    paints: [],
  };
  const api = new Function('telState', 'telT', 'telEsc', 'telSafeText', 'telPeerKey', 'telRender', 'document',
    'telFetch', 'telCallQuiet', 'telBgOp', 'telWhenIdle', body)(
    st, (k) => '[' + k + ']', esc, (x) => String(x), (p) => p.kind + ':' + p.id, () => log.push('render'), doc,
    async (url) => { log.push('GET'); return w.cached; },
    async (url, b, signal) => { log.push('POST ' + JSON.stringify(b)); w.signal = signal;
      for (let i = 0; i < (w.slowTicks || 1); i++) { await tick(); if (signal && signal.aborted) return null; }
      return w.live; },
    async (label, fn) => { if (st.busy || st.bg) return null; st.bg = true; log.push('step ' + label);
      w.paints.push(slot.innerHTML); try { return await fn(); } finally { st.bg = false; } },
    () => (st.busy || st.bg) ? new Promise((r) => waiters.push(r)) : Promise.resolve());
  w.api = api;
  // As telPeerProfile does: the sheet is drawn from what the page holds, then
  // brought up to date.
  w.open = () => { slot.innerHTML = api.telAboutHTML(key); return api.telProfileAbout(key, peer); };
  return w;
}
const posts = (w) => w.log.filter((l) => l.startsWith('POST'));
const text = (h) => h.replace(/<[^>]*>/g, ' ').replace(/\s+/g, ' ').trim();

// Read today (by the chat's member step, or an earlier open): no call.
{
  const w = world({ cached: { ok: true, known: true, members: 5, about: 'Hourly news', aboutRead: true, aboutFresh: true } });
  await w.open();
  eq('fresh in the cache: no call to Telegram', posts(w), []);
  eq('and the text is on the sheet', text(w.slot.innerHTML), '[tel_info_about] Hourly news');
  eq('without a "loading" flashing by', w.slot.shown.some((h) => /tel_loading_about/.test(h)), false);
}
// Never read: one call, drawn as a step, with "loading" on the sheet meanwhile.
{
  const w = world({ cached: { ok: true, known: true, members: 5, aboutRead: false },
    live: { ok: true, known: true, members: 6, about: 'Hourly news', aboutRead: true, aboutFresh: true } });
  await w.open();
  eq('never read: one call, asking for the description', posts(w), ['POST {"peer":{"kind":"channel","id":"9","accessHash":"3"},"about":true}']);
  eq('a visible step', w.log.includes('step tel_loading_about'), true);
  eq('the sheet says it is loading while it does', /\[tel_loading_about\]/.test(w.paints[0]), true);
  eq('then shows it', text(w.slot.innerHTML), '[tel_info_about] Hourly news');
  eq('the new count comes with it', w.st.members['channel:9'], 6);
}
// A day old, and Telegram cannot be reached: the old text stays, no error.
{
  const w = world({ cached: { ok: true, known: true, members: 5, about: 'Old text', aboutRead: true, aboutFresh: false }, live: null });
  await w.open();
  eq('stale: read again', posts(w).length, 1);
  eq('failed: the old text is still shown', text(w.slot.innerHTML), '[tel_info_about] Old text');
  eq('with no failure note over it', /tel_about_failed/.test(w.slot.innerHTML), false);
  eq('and not marked fresh', w.st.about['channel:9'].fresh, false);
}
// Nothing ever read and the read fails: say so (and try again next open).
{
  const w = world({ cached: { ok: true, known: false, aboutRead: false }, live: null });
  await w.open();
  eq('nothing to show: says it could not load', text(w.slot.innerHTML), '[tel_about_failed]');
  w.live = { ok: true, known: true, members: 2, about: 'Now it works', aboutRead: true, aboutFresh: true };
  await w.open();
  eq('the next open tries again', [posts(w).length, text(w.slot.innerHTML)], [2, '[tel_info_about] Now it works']);
}
// Read, and the channel has no description: nothing shown, nothing asked again.
{
  const w = world({ cached: { ok: true, known: true, members: 5, about: '', aboutRead: true, aboutFresh: true } });
  await w.open();
  eq('an empty description: no field, no note, no call', [w.slot.innerHTML, posts(w)], ['', []]);
}
// Closed before the call starts (the tunnel was busy): nothing is asked.
{
  const w = world({ cached: { ok: true, known: true, aboutRead: false }, live: { ok: true, aboutRead: true, about: 'x', aboutFresh: true }, busy: true });
  const p = w.open();
  await tick(); await tick();
  eq('waits while the tunnel is busy', posts(w), []);
  w.sheetOpen = false;
  w.idle();
  await p;
  eq('closed meanwhile: Telegram is not asked', posts(w), []);
  eq('and no failure is recorded', !!w.st.aboutFailed['channel:9'], false);
}
// The chat's own member step is running when the sheet opens, and brings the
// description: the sheet does not ask again.
{
  const w = world({ cached: { ok: true, known: false, aboutRead: false }, live: null, busy: true });
  const p = w.open();
  await tick(); await tick();
  w.st.about['channel:9'] = { text: 'From the step', fresh: true };
  w.idle();
  await p;
  eq('the step read it: no second call', posts(w), []);
  eq('and its text is shown', text(w.slot.innerHTML), '[tel_info_about] From the step');
}
// The cache was cleared (Storage → Telegram): what the page held goes too.
{
  const w = world({ cached: { ok: true, known: false, aboutRead: false }, live: null,
    about: { 'channel:9': { text: 'Before the clear', fresh: true } } });
  await w.open();
  eq('cleared in the backend: the old text is not shown from memory', /Before the clear/.test(w.slot.innerHTML), false);
  eq('nor kept for the next open', w.st.about['channel:9'], undefined);
}
// One call between the chat's step and the sheet, if they ever overlap.
{
  const w = world({ cached: {}, live: { ok: true } });
  const a = w.api.telAskInfo('channel:9', w.peer, false), b = w.api.telAskInfo('channel:9', w.peer, true);
  await Promise.all([a, b]);
  eq('two asks in flight share one call', posts(w).length, 1);
  await w.api.telAskInfo('channel:9', w.peer, true);
  eq('and the next one after it is its own', posts(w).length, 2);
}

// A person's bio: the same rule, under its own words.
{
  const peer = { kind: 'user', id: '7', accessHash: '13' };
  const w = world({ peer, cached: { ok: true, known: false, aboutRead: false },
    live: { ok: true, known: false, about: 'Photographer', aboutRead: true, aboutFresh: true } });
  await w.open();
  eq('a person: their bio is read once', posts(w), ['POST {"peer":{"kind":"user","id":"7","accessHash":"13"},"about":true}']);
  eq('as a "bio" step', w.log.includes('step tel_loading_bio'), true);
  eq('and shown as their bio', text(w.slot.innerHTML), '[tel_info_bio] Photographer');
  eq('no member count is made up for them', w.st.members['user:7'], undefined);
  const f = world({ peer, cached: { ok: true, known: false, aboutRead: false }, live: null });
  await f.open();
  eq('a person, failed with nothing kept: says the bio could not load', text(f.slot.innerHTML), '[tel_bio_failed]');
  const old = world({ peer, cached: { ok: true, known: false, about: 'Old bio', aboutRead: true, aboutFresh: false }, live: null });
  await old.open();
  eq('a person, failed: the old bio stays', text(old.slot.innerHTML), '[tel_info_bio] Old bio');
}

// Every word the sheet can ask for exists, in both languages (a misnamed key
// once showed its own name instead of "could not load").
{
  const i18n = readFileSync(join(here, '..', 'static', 'js', 'i18n.js'), 'utf8');
  const word = new Function(grab('telAboutWord') + '\nreturn telAboutWord;')();
  for (const k of ['user:1', 'bot:1', 'channel:1', 'group:1', 'forum:1']) for (const w of ['info', 'loading', 'failed']) {
    const key = word(k, w);
    eq(`${k} ${w}: ${key} in both languages`, (i18n.match(new RegExp('\\b' + key + ':', 'g')) || []).length, 2);
  }
}

// The user does something while a bio is being read (opens another chat,
// sends, joins): the read gives way at once. Not a failure -- the old text
// stays, nothing says "could not load", and the next open reads it again.
{
  const w = world({ cached: { ok: true, known: true, about: 'Old text', aboutRead: true, aboutFresh: false },
    live: { ok: true, about: 'New text', aboutRead: true, aboutFresh: true } });
  w.slowTicks = 50;
  const p = w.open();
  for (let i = 0; i < 5; i++) await tick();
  eq('the read is in flight, cancellable', !!w.signal && !w.signal.aborted, true);
  w.api.telAboutAbort();   // what telWhenIdle does for every user action
  await p;
  eq('a user action cancels it', w.signal.aborted, true);
  eq('the old text stays', text(w.slot.innerHTML), '[tel_info_about] Old text');
  eq('not counted as a failure', !!w.st.aboutFailed['channel:9'], false);
  eq('and the tunnel is free again', [w.st.bg, !!w.st.aboutBusy['channel:9']], [false, false]);
  w.slowTicks = 1;
  await w.open();
  eq('the next open reads it', text(w.slot.innerHTML), '[tel_info_about] New text');
}
// Every user action waits its turn rather than being dropped, and that wait
// is what cancels a description read.
eq('waiting for the tunnel cancels a description read first', /^function telWhenIdle\(\) \{[\s\S]*?telAboutAbort\(\);[\s\S]*?if \(!telState\.busy/.test(grab('telWhenIdle')), true);
for (const f of ['telJoinChannel', 'telOpenByUsername']) {
  const b = grab(f);
  eq(`${f} waits its turn instead of being dropped`, b.indexOf('await telWhenIdle()') > -1 && b.indexOf('await telWhenIdle()') < b.indexOf('telOp('), true);
}

eq('a new account starts with no descriptions', /telState\.about = \{\}/.test(grab('telResetData')), true);
// Opening a chat still costs only a channel's member step: a description or
// bio is never read for a chat nobody opened the info of.
eq('opening a chat does not read the description', /about: true|telProfileAbout/.test(grab('telOpenPeer') + grab('telFetchMembers')), false);

console.log(`${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
