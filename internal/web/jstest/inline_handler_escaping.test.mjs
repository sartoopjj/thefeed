// A value inside an inline handler must be escaped for JAVASCRIPT, not HTML.
//
// escAttr turns ' into &#39;. The HTML parser decodes that back to ' before
// the JS parser ever sees the attribute, so the string re-opens and whatever
// follows is code. ui.js documents this on jsStr, and 28 call sites used
// escAttr anyway -- including channel names from the mirror server and media
// filenames from Telegram posts, both attacker-written.
//
// Reached, it is same-origin JS in the UI: POST /api/backup/export hands over
// the messenger recovery seed, the feed passphrase and the whole chat history.
import { readFileSync, readdirSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const jsDir = join(here, '..', 'static', 'js');

let pass = 0, fail = 0;
const eq = (name, got, want) => {
  if (JSON.stringify(got) === JSON.stringify(want)) { pass++; return; }
  fail++;
  console.error(`FAIL ${name}\n  got  ${JSON.stringify(got)}\n  want ${JSON.stringify(want)}`);
};

// --- no sink may use an HTML escaper for a JS string --------------------

// The shape: a value dropped between \'  and  \'  -- a single-quoted JS
// string inside a double-quoted HTML attribute.
const sink = /\\''\s*\+\s*(escAttr|tmEscAttr|esc)\s*\(/g;
const offenders = [];
for (const f of readdirSync(jsDir).filter(n => n.endsWith('.js'))) {
  const src = readFileSync(join(jsDir, f), 'utf8');
  src.split('\n').forEach((line, i) => {
    for (const m of line.matchAll(sink)) offenders.push(`${f}:${i + 1} uses ${m[1]}`);
  });
}
eq('every inline-handler value uses the JS-string escaper', offenders, []);

// --- and that escaper actually holds ------------------------------------

const ui = readFileSync(join(jsDir, 'ui.js'), 'utf8');
// Run the real jsStr, with the DOM bit esc() needs.
globalThis.document = {
  createElement: () => ({
    _t: '',
    appendChild(n) { this._t += n.t; },
    get innerHTML() { return this._t.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;'); },
  }),
  createTextNode: (t) => ({ t }),
};
const esc = new Function(ui.match(/function esc\(s\)[^\n]*/)[0] + '; return esc;')();
globalThis.esc = esc;
const jsStr = new Function(ui.match(/function jsStr\(s\) \{[\s\S]*?\n\}/)[0] + '; return jsStr;')();

// The audit's payload.
const payload = "');alert(document.domain);//";
eq('the quote that would close the string is backslash-escaped',
  jsStr(payload).startsWith("\\'"), true);
eq('and is NOT left as an HTML entity the parser decodes back',
  jsStr(payload).includes('&#39;'), false);
// A backslash of its own must not escape the escape.
eq('a trailing backslash cannot eat the closing quote',
  jsStr("\\").includes("\\\\"), true);
// Double quotes must not be able to close the ATTRIBUTE either.
eq('a double quote cannot close the attribute', jsStr('"').includes('&quot;'), true);
eq('and angle brackets cannot open a tag',
  jsStr('<script>').includes('<'), false);

console.log(`${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
