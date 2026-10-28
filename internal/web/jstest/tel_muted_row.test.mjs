// A muted conversation has to look muted.
//
// The only thing mute changed on a row was the unread badge's colour, and a
// silenced channel usually has no badge at all -- so the conversations the
// user had deliberately silenced were indistinguishable from the ones still
// allowed to interrupt.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const src = readFileSync(join(here, '..', 'static', 'js', 'tel.js'), 'utf8');
const i18n = readFileSync(join(here, '..', 'static', 'js', 'i18n.js'), 'utf8');
const icons = readFileSync(join(here, '..', 'static', 'js', 'icons.js'), 'utf8');

let pass = 0, fail = 0;
const eq = (name, got, want) => {
  if (JSON.stringify(got) === JSON.stringify(want)) { pass++; return; }
  fail++;
  console.error(`FAIL ${name}\n  got  ${JSON.stringify(got)}\n  want ${JSON.stringify(want)}`);
};

// Run the real row markup out of telRenderSidebar.
const block = src.match(/html \+= '<div class="tel-row'[\s\S]*?'<\/div>';/)[0];
const row = (d) => new Function(
  'd', 'key', 'active', 'telAvatar', 'telFlag', 'telEsc', 'telAttrQ', 'telT',
  'telFmtWhen', 'icon', 'telPeerKey', 'telState',
  'var html = ""; ' + block + '; return html;',
)(d, 'h1', false, () => '<avatar>', s => s, s => s, s => s, k => k,
  () => '10:00', n => `<i:${n}>`, () => 'h1', { peer: null });

const muted = { peer: { kind: 'channel', id: 1, title: 'Quiet' }, muted: true, unread: 0, preview: 'p' };
const audible = { peer: { kind: 'channel', id: 2, title: 'Loud' }, muted: false, unread: 0, preview: 'p' };

// --- the case that was invisible ------------------------------------------

// Muted with NOTHING unread: no badge exists to tint, so the row itself must
// say it.
eq('a muted conversation with no unread still shows it',
  row(muted).includes('<i:bellOff>'), true);
eq('and an audible one does not', row(audible).includes('<i:bellOff>'), false);

// --- and with unread, both signals ----------------------------------------

const mutedUnread = Object.assign({}, muted, { unread: 4 });
eq('muted with unread keeps the dimmed badge',
  /class="tel-unread muted"/.test(row(mutedUnread)), true);
eq('and still shows the marker', row(mutedUnread).includes('<i:bellOff>'), true);

const loudUnread = Object.assign({}, audible, { unread: 4 });
eq('an audible one gets the accent badge',
  /class="tel-unread"/.test(row(loudUnread)), true);
eq('and no marker', row(loudUnread).includes('<i:bellOff>'), false);

// --- it is labelled, not just drawn ---------------------------------------

eq('the marker carries a label for screen readers',
  /aria-label="tel_muted"/.test(row(muted)), true);
eq('tel_muted exists in English', /tel_muted: 'Muted'/.test(i18n), true);
eq('and in Persian', /tel_muted: 'بی‌صدا'/.test(i18n), true);

// --- the icons exist and are themeable ------------------------------------

for (const n of ['bell', 'bellOff']) {
  const m = icons.match(new RegExp(`\\b${n}:\\s*\\{[^}]*svg: '(<svg[^']*)'`));
  eq(`${n} is in the registry`, !!m, true);
  if (m) eq(`${n} follows the current colour`, /currentColor/.test(m[1]), true);
}

// The action sheet used a CLOCK for "mute", which says nothing about sound.
eq('the mute action uses a bell, not a clock',
  /telSheetItem\(d\.muted \? 'bell' : 'bellOff'/.test(src), true);
eq('no clock left on a mute action', /\? 'music' : 'clock'/.test(src), false);

console.log(`${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
