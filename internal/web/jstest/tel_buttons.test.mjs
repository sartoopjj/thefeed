// A bot's URL button opens the link sheet, which shows where it really goes;
// it never leaves the app on one tap.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const src = readFileSync(join(here, '..', 'static', 'js', 'tel.js'), 'utf8');

let pass = 0, fail = 0;
const eq = (name, got, want) => {
  if (JSON.stringify(got) === JSON.stringify(want)) { pass++; return; }
  fail++;
  console.error(`FAIL ${name}\n  got  ${JSON.stringify(got)}\n  want ${JSON.stringify(want)}`);
};
const fn = (name) => {
  const m = src.match(new RegExp('function ' + name + '\\([^)]*\\) \\{[\\s\\S]*?\\n\\}\\n'));
  if (!m) throw new Error('missing ' + name);
  return m[0];
};

const sheets = [];
const api = new Function('telState', 'telT', 'esc', 'escAttr', 'parseTgLink', 'showLinkSheet', 'openExternal', 'telJumpToMsg', 'telOpenByUsername',
  'function telEsc(s){return esc(String(s==null?"":s))}function telAttrQ(s){return escAttr(String(s==null?"":s))}' +
  fn('telSafeText') + fn('telSafeURL') + fn('telButtonsHTML') + fn('telLinkClick') +
  'return { buttons: telButtonsHTML, click: telLinkClick };',
)({ peer: { username: 'here' } }, (k) => k, (s) => s, (s) => s,
  (u) => { const m = /^https:\/\/t\.me\/(\w+)/.exec(u); return m ? { user: m[1] } : null; },
  (url, extra) => sheets.push({ url, extra: extra ? extra.label : null }),
  () => { throw new Error('opened without the sheet'); }, () => {}, () => {});

const html = api.buttons({ buttons: [{ text: 'example.org', url: 'https://203.0.113.9/login' }, { text: 'Refresh' }] });
eq('a URL button is a link inside .tel-btns', /<span class="tel-btns"><a [^>]*href="https:\/\/203\.0\.113\.9\/login"/.test(html), true);

// A tap as the browser delivers it: the target is the button (or its text).
function tap(href) {
  const el = {
    getAttribute: (k) => (k === 'href' ? href : null),
    closest(sel) { return sel.split(',').map((s) => s.trim()).includes('.tel-btns a') ? el : null; },
  };
  let prevented = false;
  api.click({ target: el, preventDefault() { prevented = true; }, stopPropagation() {} });
  return prevented;
}
eq('the tap does not go straight out', tap('https://203.0.113.9/login'), true);
eq('the sheet shows the real target', sheets[0], { url: 'https://203.0.113.9/login', extra: null });
tap('https://t.me/examplechannel');
eq('a Telegram target can be opened here', sheets[1], { url: 'https://t.me/examplechannel', extra: 'tel_open_channel' });

console.log(`${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
