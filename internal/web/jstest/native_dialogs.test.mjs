// No window.confirm / alert / prompt anywhere in the frontend.
//
// The iOS app's WebView has no handler for them: confirm() answers "no" and
// prompt() answers null without showing anything, and alert() shows nothing.
// A button behind one of them silently does nothing on iPhone (Telegram's
// "Sign out" did exactly that). Use showConfirmDialog / showInputDialog /
// showToast instead.
import { readFileSync, readdirSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const root = join(here, '..', 'static');

let pass = 0, fail = 0;
const ok = (name, cond) => { if (cond) pass++; else { fail++; console.error('FAIL ' + name); } };

// A call: not a method (x.confirm, tmConfirm), optionally through window.
const call = /(?:^|[^.\w$])(?:window\.)?(?:confirm|alert|prompt)\s*\(/;
// Comments may name them; blank block comments, cut line comments (but not
// the // in a URL or a string).
const code = (src) => src.replace(/\/\*[\s\S]*?\*\//g, (m) => m.replace(/[^\n]/g, ' '))
  .split('\n').map((l) => l.replace(/(^|[^:"'\\])\/\/.*$/, '$1'));

function offenders(src) {
  const out = [];
  code(src).forEach((l, i) => { if (call.test(l)) out.push(i + 1); });
  return out;
}

// The check itself catches what it is for, and nothing it is not.
ok('catches confirm(', offenders('if (!confirm(x)) return;').length === 1);
ok('catches window.confirm(', offenders('resolve(window.confirm(m));').length === 1);
ok('catches alert(', offenders('if (j.ok) { alert(t("x")) }').length === 1);
ok('catches prompt(', offenders('var v = prompt("name");').length === 1);
ok('catches inline handlers', offenders('<a onclick="return confirm(\'x\')">').length === 1);
ok('ignores the app dialogs', offenders('await showConfirmDialog(m); tmConfirm(m); chatPrompt(m);').length === 0);
ok('ignores comments', offenders('// replaces confirm()\n/* alert(x) */ var a = 1;').length === 0);

const files = readdirSync(join(root, 'js')).filter((f) => f.endsWith('.js')).map((f) => join('js', f));
files.push('index.html');
for (const f of files) {
  const bad = offenders(readFileSync(join(root, f), 'utf8'));
  ok(f + (bad.length ? ' has native dialogs at line ' + bad.join(', ') : ''), bad.length === 0);
}

console.log(`${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
