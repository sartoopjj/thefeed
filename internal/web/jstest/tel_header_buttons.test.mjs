// The chat header on a phone: room for the name and the line under it.
//
// The countdown and the refresh button were two buttons doing one job (the
// next check, and doing it now), and search sat beside them. With the back
// button, the avatar and ⋮ that left the line under the name so little room
// that a channel's member count was cut off. Now the countdown IS the refresh
// button, and the chat's search is in its ⋮ menu.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const src = readFileSync(join(here, '..', 'static', 'js', 'tel.js'), 'utf8');
const css = readFileSync(join(here, '..', 'static', 'css', 'tel.css'), 'utf8');

let pass = 0, fail = 0;
function eq(name, got, want) {
  if (JSON.stringify(got) === JSON.stringify(want)) { pass++; return; }
  fail++;
  console.error(`FAIL ${name}\n  got  ${JSON.stringify(got)}\n  want ${JSON.stringify(want)}`);
}
const grab = (name) => src.match(new RegExp('(?:async )?function ' + name + '\\([\\s\\S]*?\\n}'))[0];

// --- one button: the countdown you can press ---------------------------------
const mkBtn = (state) => new Function('telState', 'telT', 'telAttrQ', 'icon', 'TEL_KEEP_FOCUS',
  grab('telRefreshBtnHTML') + '\nreturn telRefreshBtnHTML;')(
  state, (k) => k, String, (n) => '<i class="icon" data-i="' + n + '"></i>', ' data-keep');
{
  const html = mkBtn({})('thread');
  eq('one button', (html.match(/<button/g) || []).length, 1);
  eq('it refreshes what it sits over', /onclick="telRefreshNow\('thread'\)"/.test(html), true);
  eq('it carries the countdown ring', /tel-timer-ring/.test(html) && /tel-timer-num/.test(html), true);
  eq('and the refresh icon, for while a fetch runs', /data-i="refresh"/.test(html), true);
  eq('the countdown paints it', /class="[^"]*\btel-timer-btn\b/.test(html), true);
  eq('progress repaints its spin', /class="[^"]*\btel-refresh-btn\b/.test(html), true);
  eq('idle: not spinning', /\bspinning\b/.test(html), false);
  eq('says what it does', /aria-label="tel_refresh"/.test(html), true);
  eq('keeps the keyboard up', / data-keep/.test(html), true);
  eq('the list gets its own', /onclick="telRefreshNow\('list'\)"/.test(mkBtn({})('list')), true);
}
{
  const html = mkBtn({ busy: true })('thread');
  eq('busy: spins', /\bspinning\b/.test(html), true);
  eq('and refuses a second press', / disabled/.test(html), true);
  eq('a background pass spins without disabling', / disabled/.test(mkBtn({ bg: true })('thread')), false);
}
// While it spins the ring makes way for the turning arrow, and the other way
// round.
eq('idle shows the ring, not the arrow', /\.tel-timer-btn \.icon\s*\{[^}]*display:\s*none/.test(css), true);
eq('spinning shows the arrow, not the ring',
  /\.tel-timer-btn\.spinning \.tel-timer-ring\s*\{[^}]*display:\s*none/.test(css), true);
eq('the separate countdown button is gone', /function telCountdownHTML/.test(src), false);

// --- the headers -------------------------------------------------------------
const pane = grab('telRenderPane');
const header = pane.slice(0, pane.indexOf('telThreadMenu()'));
eq('the chat header has the merged button', /telRefreshBtnHTML\('thread'\)/.test(header), true);
eq('and no search button', /telSearchBtnHTML/.test(header), false);
eq('nor a second refresh', (header.match(/telRefreshNow/g) || []).length, 0);
const top = grab('telRenderTopbar');
eq('the list header has the merged button', /telRefreshBtnHTML\('list'\)/.test(top), true);
eq('and keeps its search', /telSearchBtnHTML\('list'\)/.test(top), true);

// --- search, from the chat's ⋮ ----------------------------------------------
{
  let items = '';
  const run = new Function('telState', 'telPeerKey', 'telSheetItem', 'telT', 'telDialogFor', 'jsStr',
    'telIsBlocked', 'telSheet', 'icon', 'telEsc',
    grab('telThreadMenu') + '\nreturn telThreadMenu;');
  const menu = run({ peer: { kind: 'channel', id: '9', title: 'c' } }, (p) => p.kind + ':' + p.id,
    (ic, label, on) => `[${ic}|${label}|${on}]`, (k) => k, () => null, String, () => false,
    (h) => { items = h; }, String, String);
  menu();
  eq('the ⋮ menu offers search', /\[magnifier\|tel_search\|telCloseSheet\(\);telToggleSearch\('thread'\)\]/.test(items), true);
  eq('right after refresh', items.indexOf('tel_search') > items.indexOf('tel_refresh') &&
    items.indexOf('tel_search') < items.indexOf('tel_load_older'), true);
}

console.log(`${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
