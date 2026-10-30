// Typing in a conversation must survive whatever the page does around it: a
// round trip starting or ending used to disable send and rebuild the text box,
// which on a phone keyboard dropped the word being typed.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const src = readFileSync(join(here, '..', 'static', 'js', 'tel.js'), 'utf8');

let pass = 0, fail = 0;
const ok = (name, cond) => { if (cond) pass++; else { fail++; console.error('FAIL ' + name); } };
const body = (name) => {
  const m = src.match(new RegExp('function ' + name + '\\([^)]*\\) \\{[\\s\\S]*?\\n\\}\\n'));
  if (!m) throw new Error('missing ' + name);
  return m[0];
};

const button = src.match(/'<button class="tel-send-btn"[^\n]*\n[^\n]*/)[0];
ok('send is never disabled: telSend waits for the tunnel itself', !/busy/.test(button));
ok('telSend does wait for the tunnel', /await telWhenIdle\(\)/.test(body('telSend')));

const stats = body('telRefreshStats');
ok('counters are a background operation', /telBgOp\(/.test(stats) && !/await telOp\(/.test(stats));

const render = body('telRender');
ok('a render keeps the live composer when it can', /telKeepComposer\(pane,/.test(render));
ok('and records which conversation the pane shows', /setAttribute\('data-peer'/.test(render));

const keep = body('telKeepComposer');
ok('only for the conversation already on screen', /getAttribute\('data-peer'\) !== key/.test(keep));
ok('the composer is never removed from the page', !/removeChild\(live\)/.test(keep) && /!== live/.test(keep));

const restore = body('telRestoreFocus');
ok('focus restore leaves an element that kept focus alone',
  /el === document\.activeElement\) return/.test(restore) &&
  restore.indexOf('activeElement') < restore.indexOf('setSelectionRange'));

// The jump-to-latest button: drawn with the conversation, re-evaluated on
// every scroll and after every render (a render can move the list).
ok('the conversation draws a jump-to-latest button', /id="telScrollDown"/.test(src));
ok('scrolling decides whether it shows', /onscroll="[^"]*telUpdateScrollDown\(\)/.test(src));
ok('and so does every render', /telUpdateScrollDown\(\);/.test(body('telRender')));
const upd = body('telUpdateScrollDown');
ok('it hides near the bottom and sits above the composer',
  /classList\.toggle\('hidden', !far\)/.test(upd) && /tel-compose/.test(upd));

console.log(`tel_composer: ${pass} passed, ${fail} failed`);
if (fail) process.exit(1);
