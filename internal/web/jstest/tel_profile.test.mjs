// Tapping the open chat's name shows its info, as in the messenger and in
// Telegram: picture, name, what it is and how many are in it, every username
// (the main one and the others it holds), a channel's description, and what
// can be done with it. Only the description or bio may need a call
// (tel_about.test).
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
const jsStr = (x) => esc(x).replace(/\\/g, '\\\\').replace(/'/g, "\\'").replace(/"/g, '&quot;');

function profile(peer, dialog, blocked, about) {
  let sheet = null;
  const telState = { peer, members: {}, membersFresh: {}, blocked: blocked ? { [peer.id]: {} } : {},
    about: about || {}, aboutBusy: {}, aboutFailed: {} };
  const asked = [];
  const run = new Function('telState', 'telT', 'telEsc', 'telAttrQ', 'telAvatar', 'telSheet', 'telDialogFor', 'telPeerKey',
    'jsStr', 'icon', 'telIsBlocked', 'telFmtCount', 'telSafeText', 'telUIDir', 'telProfileAbout',
    ['telSheetItem', 'telMembersLine', 'telHasFullInfo', 'telHasAbout', 'telAboutWord', 'telAboutHTML', 'telPeerProfile'].map(grab).join('\n') + '\nreturn telPeerProfile;');
  run(telState, (k) => '[' + k + ']', esc, esc, (n, size, th) => `<avatar ${size}>`, (h) => { sheet = h; },
    () => dialog, (p) => p.kind + ':' + p.id, jsStr, (n) => `<i ${n}>`,
    (p) => !!blocked && (p.kind === 'user' || p.kind === 'bot'), (n) => String(n),
    (x) => String(x).replace(/[\u202A-\u202E\u2066-\u2069]/g, ''), () => 'rtl', (k) => asked.push(k))();
  profile.asked = asked;
  return sheet;
}
const text = (h) => h.replace(/<[^>]*>/g, ' ').replace(/\s+/g, ' ').trim();

// --- a channel holding several usernames ------------------------------------
{
  const peer = { kind: 'channel', id: '9', title: 'Durov', username: 'durov', usernames: ['rove', 'paul'], members: 10063297 };
  const h = profile(peer, { peer, muted: false });
  eq('picture and name', /<avatar 64>/.test(h) && /Durov/.test(h), true);
  eq('what it is and how many', /\[tel_subscribers_n\]/.test(h), true);
  eq('the main username, tap to copy', /onclick="telCopy\('@durov'\)"[^>]*>@durov</.test(h), true);
  eq('and every other one', /\[tel_info_also\]/.test(h) && /onclick="telCopy\('@rove'\)"/.test(h) && /onclick="telCopy\('@paul'\)"/.test(h), true);
  eq('usernames are left-to-right inside a Persian sheet', /dir="ltr"[^>]*>@durov/.test(h), true);
  eq('separated by the Persian comma in Persian', /@rove<\/button><span class="tel-profile-sep">، <\/span>/.test(h), true);
  eq('mute from here', /telToggleMute\('channel:9'\)/.test(h), true);
  eq('leave from here', /telDeleteChat\('channel:9'\)/.test(h), true);
  eq('no block for a channel', /telToggleBlock/.test(h), false);
  eq('no join for one already in the list', /telJoinChannel/.test(h), false);
}
// --- a person --------------------------------------------------------------
{
  const peer = { kind: 'user', id: '7', title: 'Someone', username: 'someone' };
  const h = profile(peer, { peer });
  eq('a person: no member count', /tel_subscribers|tel_members/.test(h), false);
  eq('says it is a person', /\[tel_kind_user\]/.test(h), true);
  eq('block from here', /telToggleBlock\(\)[\s\S]*\[tel_block\]/.test(h), true);
  eq('no "also" line with one username', /tel_info_also/.test(h), false);
  eq('blocked: offers unblock', /\[tel_unblock\]/.test(profile(peer, { peer }, true)), true);
}
// --- a channel only being viewed -------------------------------------------
{
  const peer = { kind: 'channel', id: '5', title: 'Public' };
  const h = profile(peer, { peer, visiting: true });
  eq('viewing, not joined: join from here', /telJoinChannel\('channel:5'\)/.test(h), true);
  // Not a member: nothing to mute or leave (as in Telegram, Join is all).
  eq('viewing: no mute', /telToggleMute/.test(h), false);
  eq('viewing: no leave', /telDeleteChat/.test(h), false);
  eq('no username field without one', /tel_info_username/.test(h), false);
}
// --- nothing from Telegram reaches the markup raw ---------------------------
{
  const peer = { kind: 'channel', id: '6', title: '<img src=x onerror=alert(1)>', username: "a'b", usernames: ['x"y<z'] };
  const h = profile(peer, { peer });
  eq('title escaped', /<img/.test(h), false);
  eq('username cannot break out of its onclick', /telCopy\('@a\\'b'\)/.test(h), true);
  eq('nor an extra one', /<z/.test(h) || /x"y/.test(h), false);
}

// --- a channel's description ------------------------------------------------
{
  const peer = { kind: 'channel', id: '9', title: 'News' };
  const h = profile(peer, { peer }, false, { 'channel:9': { text: 'Hourly news.\nhttps://example.org', fresh: false } });
  eq('the description, as last read (an old one too)', /tel-profile-about[^>]*>Hourly news\.\nhttps:\/\/example\.org</.test(h), true);
  eq('its own labelled field', /\[tel_info_about\]/.test(h), true);
  eq('in a slot the read can repaint', /id="telProfileAbout" data-key="channel:9"/.test(h), true);
  eq('and the sheet asks for it to be brought up to date', profile.asked, ['channel:9']);
  eq('nothing read yet: no empty field', /tel-profile-about/.test(profile(peer, { peer })), false);
  const evil = profile(peer, { peer }, false, { 'channel:9': { text: '<img src=x onerror=alert(1)>\u202Egnp.exe', fresh: true } });
  eq('a description is text, never markup', /<img/.test(evil), false);
  eq('nor a direction override', /\u202E/.test(evil), false);
  const sg = profile({ kind: 'group', id: '8', accessHash: '5', title: 'SG' }, { peer: {} });
  eq('a supergroup has one too', /telProfileAbout/.test(sg), true);
  // A person's is a bio, under its own word; a bot's is a description.
  const bio = profile({ kind: 'user', id: '7', title: 'P' }, { peer: {} }, false, { 'user:7': { text: 'Photographer', fresh: true } });
  eq('a person: their bio, under "bio"', /\[tel_info_bio\][\s\S]*Photographer/.test(bio) && !/tel_info_about/.test(bio), true);
  eq('and it is brought up to date', profile.asked, ['user:7']);
  const bot = profile({ kind: 'bot', id: '8', title: 'B' }, { peer: {} }, false, { 'bot:8': { text: 'Weather bot', fresh: true } });
  eq('a bot: its description', /\[tel_info_about\][\s\S]*Weather bot/.test(bot), true);
  for (const [name, p] of [['a basic group', { kind: 'group', id: '5', accessHash: '0' }], ['direct messages', { kind: 'channel', id: '4', direct: true }]]) {
    const x = profile(p, { peer: p });
    eq(`${name}: no description slot, nothing asked`, [/telProfileAbout/.test(x), profile.asked.length], [false, 0]);
  }
}

// --- reached by tapping the header -------------------------------------------
const pane = grab('telRenderPane');
eq('the header title opens it', /class="tel-topbar-title[^"]*" role="button"[^>]*onclick="telPeerProfile\(\)"/.test(pane), true);
eq('and says so to a screen reader', /onclick="telPeerProfile\(\)"[^>]*aria-label=|aria-label=[^>]*onclick="telPeerProfile\(\)"/.test(pane), true);
// The ⋮ menu keeps everything it had (the App Store notes walk through it).
const menu = grab('telThreadMenu');
for (const f of ['telToggleBlock', 'telToggleMute', 'telCopyUsername', 'telDeleteChat', 'telJoinChannel'])
  eq(`⋮ still has ${f}`, menu.includes(f), true);

// A username has no spaces to break at: it must wrap anywhere, or a long one
// runs off a 320px screen (seen in the browser).
const css = readFileSync(join(here, '..', 'static', 'css', 'tel.css'), 'utf8');
eq('a long username wraps inside the sheet', /\.tel-profile-un \{[^}]*overflow-wrap: anywhere/.test(css), true);
// Counts use one kind of digit everywhere: "1 مشترک" beside "2 مشترک", not "۱".
eq('the singular uses the same digits as every other count',
  /tel_subscribers_one: '1 /.test(i18n) && /tel_members_one: '1 /.test(i18n) && !/tel_(subscribers|members)_one: '۱/.test(i18n), true);

eq('a long description wraps and scrolls inside the sheet',
  /\.tel-profile-about \{[^}]*white-space: pre-wrap[^}]*overflow-wrap: anywhere[^}]*overflow-y: auto/.test(css), true);
for (const k of ['tel_info', 'tel_info_username', 'tel_info_also', 'tel_info_about', 'tel_loading_about', 'tel_about_failed',
  'tel_info_bio', 'tel_loading_bio', 'tel_bio_failed']) {
  eq(`${k} in both languages`, (i18n.match(new RegExp('\\b' + k + ':', 'g')) || []).length, 2);
}

console.log(`${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
