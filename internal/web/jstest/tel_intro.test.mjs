// The Telegram intro is read once per install, not once per port: dismissing
// it tells the backend, and the backend's answer hides it on a fresh port.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const src = readFileSync(join(here, '..', 'static', 'js', 'tel.js'), 'utf8');
const core = readFileSync(join(here, '..', 'static', 'js', 'core.js'), 'utf8');

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

function harness(open, showIntro) {
  const store = {};
  const seen = { posts: [], renders: 0 };
  const state = { open, showIntro };
  const api = new Function('telState', 'localStorage', 'fetch', 'telPost', 'telRender', 'telRefreshInfo', 'seen',
    fn('telIntroSeen') + fn('telDismissIntro') + fn('telIntroRead') +
    '; return { seen: telIntroSeen, dismiss: telDismissIntro, read: telIntroRead };',
  )(state, { getItem: (k) => store[k] || null, setItem: (k, v) => { store[k] = v; } },
    (url, opts) => { seen.posts.push([url, JSON.parse(opts.body)]); return Promise.resolve(); },
    (b) => ({ body: JSON.stringify(b) }), () => { seen.renders++; }, () => {}, seen);
  return { state, seen, api };
}

{
  const { state, seen, api } = harness(true, true);
  api.dismiss();
  eq('dismissing tells the backend', seen.posts, [['/api/settings', { telIntroSeen: true }]]);
  eq('and this page', api.seen(), true);
  eq('and closes it', state.showIntro, false);
}
{
  const { state, seen, api } = harness(true, true);
  api.read();
  eq('a fresh port: the backend\'s answer hides an intro already shown', [state.showIntro, seen.renders], [false, 1]);
  eq('and the page remembers it', api.seen(), true);
  eq('without telling the backend what it said', seen.posts.length, 0);
}
{
  const { seen, api } = harness(false, false);
  api.read();
  eq('nothing on screen: no render', seen.renders, 0);
}
eq('the settings load hands it over', /if \(s\.telIntroSeen === true && typeof telIntroRead === 'function'\) telIntroRead\(\);/.test(core), true);

console.log(`tel_intro: ${pass} passed, ${fail} failed`);
if (fail) process.exit(1);
