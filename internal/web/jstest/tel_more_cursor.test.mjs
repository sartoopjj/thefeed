// "Load more chats" must name the conversation it continues from.
//
// Telegram pages the list from a message id AND the peer it belongs to. Both
// went out as offsetId, so the backend read the message id as the peer's id
// and every next page was asked for from a conversation that does not exist.
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

const fn = src.match(/async function telLoadMoreDialogs\([\s\S]*?\n}/)[0];
let asked = '';
const telState = {
  more: true,
  cursor: { date: 1961, id: 4991, peer: { kind: 'bot', id: '2009', accessHash: '77' } },
};
const run = new Function('telState', 'telCall', 'telApplyPage', 'telOp', 'telBgOp', 'TEL_DIALOG_PAGE',
  fn + '\nreturn telLoadMoreDialogs;');
const telLoadMoreDialogs = run(telState, async (u) => { asked = u; return null; }, () => 0,
  async (_k, f) => f(), async (_k, f) => f(), 40);
await telLoadMoreDialogs(false);

const q = new URLSearchParams(asked.split('?')[1] || '');
eq('continues after the last message', q.get('offsetId'), '4991');
eq('and its date', q.get('offsetDate'), '1961');
eq('names the conversation by its own id', q.get('offsetPeerId'), '2009');
eq('with its kind', q.get('offsetPeerKind'), 'bot');
eq('and its access hash', q.get('offsetPeerAccessHash'), '77');

console.log(`${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
