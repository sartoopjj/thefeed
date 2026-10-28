// ===== TELEGRAM (real Telegram over the DNS tunnel) =====
// A section, not a modal: list → #telSidebar, conversation → #telPane, sheets
// → #telModal, selected by html.tel-section. Separate from chat.js (different
// protocol and account). Every byte is a DNS query, so: cache, fetch deltas
// only, one request at a time.

// ---------------------------------------------------------------- state ------

var telState = {
  open: false,
  info: null,            // last /api/tel/info
  authorized: false,
  user: null,            // signed-in account

  // Memory only: Telegram content must not reach the browser's disk.
  dialogs: [],
  cursor: null,          // {date,id,peer} — where the next page continues
  more: false,           // another page exists
  total: 0,              // rows Telegram says exist, when it says
  loadedOnce: false,
  tab: 'all',

  // peerKey -> {peer, msgs[], more, loading}. msgs is oldest-first.
  threads: {},
  peer: null,            // the open conversation
  draft: '',

  // peerKey -> the message id the "new messages" line sits after. Pinned the
  // moment unread messages are known about and held there for a sticky window,
  // so a refresh landing a second later cannot move the line out from under
  // the person reading it.
  sep: {},
  // Set when a line has just been pinned and not yet scrolled to. Cleared only
  // once the scroll actually lands: on a cold open the messages arrive after
  // the line is decided, so the intent has to outlive a render with nothing to
  // scroll to.
  sepScroll: '',

  busy: false,           // a user-initiated call is in flight
  busyLabel: '',
  // Which round trip of a multi-step action is running, and how many there
  // are. Zero means a single-step action, which says nothing.
  stepNo: 0,
  stepTotal: 0,
  // What the running action is about, when it is about one conversation.
  // Taken from the action, never from whatever pane happens to be open: a
  // dialog-list fetch labelled with the open chat's name says something that
  // is simply not true.
  opTitle: '',
  bg: false,             // a background refresh is in flight
  error: '',

  // login
  codeHash: '',
  codeVia: '',   // where Telegram says it put the code
  phone: '',
  needPassword: false,
  passwordHint: '',
  editCreds: false,
  dial: '',

  // connect follow-up
  server: '',
  attempt: 0,
  cancelled: false,

  nextAt: 0,             // when the next auto-refresh is due
  showIntro: false,

  // Searches the cache only. searchIn is 'list' or 'thread': the desktop
  // shows both at once and there is one box.
  search: '',
  searchIn: '' 
};

// Auto-refresh cadence. ONE value, whatever is open.
//
// A delta on an open conversation looks cheap, but over DNS nothing is: at
// twenty seconds the section refetches a channel continuously. Sixty is where
// an automatic check stops competing with the person using the link, and the
// refresh button covers anyone who wants it sooner.
//
// One value is also what keeps the two countdowns -- the list's and the open
// conversation's -- from disagreeing as the user moves between them.
var TEL_POLL_MS = 60000;
// How many messages one counter refresh asks about: a screenful, not the whole
// thread. Each id costs bytes in the answer, and every byte is a DNS query.
var TEL_STATS_MAX = 15;
// A message's counters are not asked about again within this. Scrolling back
// and forth over the same posts would otherwise spend the link re-reading
// numbers that cannot have moved.
var TEL_STATS_MIN_MS = 10000;
// How long after scrolling stops before the newly visible messages are asked
// about, so dragging through a channel does not fire a request per frame.
var TEL_STATS_SCROLL_MS = 600;
// Page size and the ceiling on how much of the list is ever held.
var TEL_DIALOG_PAGE = 40;
var TEL_DIALOG_MAX = 400;
// How many times the "load more chats" button waits for the link before
// giving up and letting the user press again.
var TEL_MORE_TRIES = 6;

// Messages per fetch. Downstream is where a conversation's bulk travels and
// every reply is a DNS query, so a page is kept small enough to paint quickly
// on a slow link; "load older" brings the rest.
var TEL_HISTORY_PAGE = 15;
// How many pages back a jump may walk. Each one is a DNS round trip, so a
// quote pointing far up a channel gives up rather than fetching for minutes.
var TEL_JUMP_MAX_PAGES = 3;
// How long the "new messages" line stays after the messages under it have been
// read. Without a delay the next refresh -- sixty seconds away at most, and
// immediately when the open conversation is the one that moved -- would dismiss
// the line before the user had a chance to see where the new posts began. Same
// value as the feed's, for the same reason.
var TEL_SEP_STICKY_MS = 10000;
// Unacknowledged messages get a synthetic id above any real one, so they sort
// last and are never mistaken for Telegram's.
var TEL_PENDING_BASE = 1e12;
var telPendingSeq = 0;
var TEL_MAX_ATTEMPTS = 3;

// Conversation tabs; a forum is a supergroup, so both sit under Groups.
var TEL_TABS = [
  { key: 'all', kinds: null },
  { key: 'user', kinds: ['user'] },
  { key: 'group', kinds: ['group', 'forum'] },
  { key: 'channel', kinds: ['channel'] },
  { key: 'bot', kinds: ['bot'] }
];

// --------------------------------------------------------------- helpers -----

function telT(k) { return (typeof t === 'function') ? (t(k) || k) : k; }
function telEsc(s) { return esc(String(s == null ? '' : s)); }
function telAttrQ(s) { return escAttr(String(s == null ? '' : s)); }

// telPeerKey identifies a conversation; the kind is part of it, since a user
// and a channel can share an id.
function telPeerKey(p) { return p ? (p.kind + ':' + p.id) : ''; }

function telIsFa() { return (typeof lang !== 'undefined' && lang === 'fa'); }

function telFmtTime(ts) {
  if (!ts) return '';
  return new Date(ts * 1000).toLocaleTimeString(telIsFa() ? 'fa-IR' : undefined,
    { hour: '2-digit', minute: '2-digit' });
}

// telFmtDate renders a day label: Jalali for Persian, Gregorian otherwise.
function telFmtDate(ts) {
  if (!ts) return '';
  var d = new Date(ts * 1000);
  var fa = telIsFa();
  var opts = { year: 'numeric', month: 'long', day: 'numeric' };
  if (fa) opts.calendar = 'persian';
  try { return d.toLocaleDateString(fa ? 'fa-IR' : undefined, opts); }
  catch (e) { return d.toLocaleDateString(); }
}

// telFmtWhen is the list's right-hand stamp: a time today, a date before that.
function telFmtWhen(ts) {
  if (!ts) return '';
  var d = new Date(ts * 1000);
  var now = new Date();
  if (d.toDateString() === now.toDateString()) return telFmtTime(ts);
  return d.toLocaleDateString(telIsFa() ? 'fa-IR' : undefined,
    telIsFa() ? { month: 'short', day: 'numeric', calendar: 'persian' }
      : { month: 'short', day: 'numeric' });
}

// telAvatar draws the lettered circle the feed and messenger lists use.
// telAvatar draws a conversation's picture, falling back to its initial.
//
// thumb is the tiny JPEG Telegram already sent with the conversation list --
// about forty pixels on its longest side, so it is soft, but at this size that
// reads as a photo rather than as a blur. It costs nothing: those bytes cross
// the link whether or not anything looks at them.
function telAvatar(name, size, thumb) {
  size = size || 44;
  if (thumb) {
    // Nothing from Telegram reaches an attribute un-escaped, base64 or not.
    return '<span class="tel-avatar tel-avatar-pic" style="width:' + size +
            // No loading="lazy": the bytes are already here, inside the attribute.
      // There is no fetch to defer, and deferring it only risks an avatar that
      // never paints -- which is what happened while the pane was off screen.
      'px;height:' + size + 'px"><img alt="" src="data:image/jpeg;base64,' +
      telAttrQ(thumb) + '"></span>';
  }
  var palette = ['#e57373', '#f06292', '#ba68c8', '#9575cd', '#7986cb',
    '#64b5f6', '#4fc3f7', '#4dd0e1', '#4db6ac', '#81c784',
    '#aed581', '#dce775', '#ffd54f', '#ffb74d', '#ff8a65'];
  var disp = (name || '').trim() || '?';
  var h = 0;
  for (var i = 0; i < disp.length; i++) h = (h * 31 + disp.charCodeAt(i)) >>> 0;
  return '<span class="tel-avatar" style="width:' + size + 'px;height:' + size +
    'px;font-size:' + Math.round(size * 0.42) + 'px;background:' + palette[h % palette.length] + '">' +
    telEsc(disp.charAt(0).toUpperCase()) + '</span>';
}

// ------------------------------------------------------------------- api -----

function telPost(body) {
  return {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body || {})
  };
}

async function telFetch(url, opts) {
  var r = await fetch(url, opts);
  if (!r.ok) throw new Error('http ' + r.status);
  return r.json();
}

// telReason translates a failure. tel_err_generic means the backend did not
// recognise the error, so the raw text is shown instead of a useless sentence.
function telReason(r, fallbackKey) {
  if (r && r.reason && r.reason !== 'tel_err_generic') {
    var txt = telT(r.reason);
    if (txt !== r.reason) return txt;
  }
  if (r && r.error) return r.error;
  return telT(fallbackKey || 'tel_err_generic');
}

// telHistAbort cancels the history fetch still in flight, if any.
//
// One tunnel carries one request at a time, so a fetch nobody is waiting for
// blocks the one they are: four taps means four pages of messages queued
// ahead of the channel actually on screen. The abort cancels the backend
// context too, which abandons the RPC rather than ignoring its answer.
var telHistCtl = null;

function telHistAbort() {
  if (telHistCtl) {
    try { telHistCtl.abort(); } catch (e) { }
    telHistCtl = null;
  }
}

// telCallQuiet is for calls the user did not ask for -- counters, read marks,
// warming the cache. A lost link must not put a banner in front of someone who
// is reading or typing: the shield already says it, the reconnect is already
// running, and the message they were sending goes through anyway. Raising it
// here showed "not connected" over a connection that was about to work.
async function telCallQuiet(url, body) {
  var had = telState.error;
  var r = await telCall(url, body);
  if (!r) telState.error = had;
  return r;
}

// telCall wraps every backend call: translated error, null on failure.
async function telCall(url, body, fallbackKey, signal) {
  try {
    var opts = body === undefined ? undefined : telPost(body);
    if (signal) opts = Object.assign({}, opts || {}, { signal: signal });
    var r = await telFetch(url, opts);
    if (r && r.ok) return r;
    telState.error = telReason(r, fallbackKey);
    // NOT a sign-out. authorized means "this account is signed in to
    // Telegram", which a dropped tunnel says nothing about: clearing it here
    // dropped the whole section back to the login screen on any transient
    // failure, which is what made a reload look like the cache and the account
    // had been wiped. The red shield is the only thing a lost link changes.
  } catch (e) {
    // An abort is this code's own doing, not a failure to report.
    if (e && e.name === 'AbortError') return null;
    telState.error = telT(fallbackKey || 'tel_err_generic');
  }
  return null;
}

// telOp runs one user-initiated call with the progress label and a guaranteed
// unlock. One tunnel, so a second operation is refused rather than queued.
// telSteps declares that the action about to run takes more than one round
// trip, so each can say which it is -- otherwise one "send" reads as two or
// three unrelated progress bars.
//
// The total may grow: a send only learns whether it needs the fallback
// read-back once it answers. Still better than leaving the user to count bars.
function telSteps(total, title) {
  telState.stepNo = 0;
  telState.stepTotal = total > 1 ? total : 0;
  telState.opTitle = title || '';
}

function telStepsMore(total) {
  if (telState.stepTotal && total > telState.stepTotal) telState.stepTotal = total;
}

// telStepAdvance counts a round trip that happens INSIDE an operation rather
// than as its own telOp. The bar is redrawn per request, not per operation, so
// a second fetch within one op is a second bar to the user and has to be
// numbered like one.
function telStepAdvance() {
  if (telState.stepTotal) {
    telState.stepNo++;
    telPaintProgress();
  }
}

function telStepsDone() {
  telState.stepNo = 0;
  telState.stepTotal = 0;
  telState.opTitle = '';
}

async function telOp(label, fn) {
  if (telState.busy || telState.bg) return null;
  telState.busy = true;
  telState.busyLabel = label || 'tel_working';
  // One step of the declared plan, counted as it starts.
  if (telState.stepTotal) telState.stepNo++;
  telState.error = '';
  telRender();
  try {
    return await fn();
  } finally {
    telState.busy = false;
    telState.busyLabel = '';
    telRender();
    telReleaseIdle();
  }
}

// telBgOp is the same for a background refresh, disabling nothing.
async function telBgOp(label, fn) {
  if (telState.busy || telState.bg) return null;
  if (telState.stepTotal) telState.stepNo++;
  telState.bg = true;
  telState.bgLabel = label || 'tel_checking_new';
  telPaintProgress();
  try {
    return await fn();
  } finally {
    telState.bg = false;
    telState.bgLabel = '';
    telPaintProgress();
    telReleaseIdle();
  }
}

// A user action that finds the tunnel busy parks here instead of being
// dropped.
var telIdleWaiters = [];

function telWhenIdle() {
  if (!telState.busy && !telState.bg) return Promise.resolve();
  return new Promise(function (resolve) { telIdleWaiters.push(resolve); });
}

function telReleaseIdle() {
  var waiting = telIdleWaiters;
  telIdleWaiters = [];
  waiting.forEach(function (resolve) { resolve(); });
}

// ----------------------------------------------------------- alerts ---------
// Rings and notifies while the page is open. A closed app is the backend
// watcher's job — nothing here runs then.

function telRequestNotifyPermission() {
  try {
    if (typeof Notification !== 'undefined' && Notification.permission === 'default') {
      Notification.requestPermission();
    }
  } catch (e) { }
}

function telSoundOff() {
  try { return localStorage.getItem('telSoundOff') === '1'; } catch (e) { return false; }
}

function telToggleSound() {
  try { localStorage.setItem('telSoundOff', telSoundOff() ? '0' : '1'); } catch (e) { }
  showToast(telT(telSoundOff() ? 'tel_sound_off' : 'tel_sound_on'));
  telRender();
}
window.telToggleSound = telToggleSound;

function telBeep() {
  if (telSoundOff()) return;
  try {
    var Ctx = window.AudioContext || window.webkitAudioContext;
    if (!Ctx) return;
    var ac = new Ctx();
    var o = ac.createOscillator(), g = ac.createGain();
    o.type = 'sine';
    o.frequency.value = 760;
    g.gain.value = 0.05;
    o.connect(g);
    g.connect(ac.destination);
    o.start();
    setTimeout(function () { o.stop(); ac.close(); }, 140);
  } catch (e) { }
}

// telNotifyNew announces messages the user is not already looking at.
function telNotifyNew(n, title) {
  if (n <= 0) return;
  var watching = telState.open && !document.hidden;
  telBeep();
  if (watching) return;
  try {
    if (typeof Notification !== 'undefined' && Notification.permission === 'granted') {
      new Notification(telT('tel_title'), {
        body: title
          ? telT('tel_new_from').replace('{name}', title)
          : telT('tel_new_messages').replace('{n}', n)
      });
    }
  } catch (e) { }
}

function telReady() {
  return !!(telState.info && telState.info.connected && telState.authorized);
}

// ----------------------------------------------- section open / close --------
// Each layer owns a history entry so back walks conversation → list → out.

var telHistoryPushed = false;
var telThreadPushed = false;
var telSuppressPopstate = 0;

function openTel(adopt) {
  document.documentElement.classList.add('tel-section');
  telState.open = true;
  if (adopt) {
    telHistoryPushed = true;
  } else if (!telHistoryPushed) {
    try { history.pushState({ view: 'telSection' }, ''); telHistoryPushed = true; } catch (e) { }
  }
  // First visit explains itself before asking for anything.
  if (!telIntroSeen()) telState.showIntro = true;
  telRender();
  telStartTimers();
  telRequestNotifyPermission();
  // Before anything that touches the link, and NOT awaited: the cached list
  // needs no tunnel, and behind telRefreshInfo()'s auth check a reload would
  // sit on "fetching…" for a round trip before showing conversations already
  // on disk. Authorization does not gate it either -- an unauthorized account
  // has nothing cached to show.
  telPrimeFromCache();
  telRefreshInfo().then(function () {
    // The cache does not need a tunnel, so the list is shown on being signed
    // in alone -- not on being connected, and not instead of connecting.
    // Gating this on the link is what made a reload show an empty section
    // while the tunnel was coming back up, as if the conversations had been
    // wiped; they are on disk, sealed, and already ours.
    if (telState.authorized) telPrimeFromCache();
    if (telState.info && telState.info.connecting) telFollowConnect();
    else if (!telReady()) telAutoCheckServers();
  });
}

function telTeardown() {
  telStopTimers();
  telState.open = false;
  telState.peer = null;
  document.documentElement.classList.remove('tel-section');
  document.documentElement.classList.remove('tel-thread');
  document.documentElement.classList.remove('tel-search-open');
  var app = document.getElementById('app'); if (app) app.classList.remove('chat-open');
  telCloseSheet();
  var sb = document.getElementById('telSidebar'); if (sb) sb.innerHTML = '';
  var pane = document.getElementById('telPane'); if (pane) pane.innerHTML = '';
}

// telCloseForNav tears down for a nav switch without the history unwind, and
// reports whether an entry is left for the next section to adopt.
window.telCloseForNav = function () {
  var had = telHistoryPushed || telThreadPushed;
  telHistoryPushed = false;
  telThreadPushed = false;
  telTeardown();
  return had;
};

function closeTel() {
  var steps = (telHistoryPushed ? 1 : 0) + (telThreadPushed ? 1 : 0);
  telHistoryPushed = false;
  telThreadPushed = false;
  telTeardown();
  if (steps > 0) {
    telSuppressPopstate += steps;
    try { history.go(-steps); } catch (e) { }
  }
}

window.addEventListener('popstate', function () {
  if (telSuppressPopstate > 0) { telSuppressPopstate--; return; }
  if (telThreadPushed) {
    telThreadPushed = false;
    telShowList();
    return;
  }
  if (telHistoryPushed) {
    telHistoryPushed = false;
    telTeardown();
  }
});

window.openTel = openTel;
window.closeTel = closeTel;

// ------------------------------------------------------------ onboarding -----
// Shown once, and from the ⋮ menu after that.

function telIntroSeen() {
  try { return localStorage.getItem('tel_intro_seen') === '1'; } catch (e) { return false; }
}

function telDismissIntro() {
  try { localStorage.setItem('tel_intro_seen', '1'); } catch (e) { }
  telState.showIntro = false;
  telRender();
  telRefreshInfo();
}
window.telDismissIntro = telDismissIntro;

function telShowIntro() { telState.showIntro = true; telRender(); }
window.telShowIntro = telShowIntro;

// TEL_INTRO_CARDS: one row each, icon + title + body, from i18n.
var TEL_INTRO_CARDS = [
  { icon: 'navTel', key: 'what' },
  { icon: 'heart', key: 'why' },
  { icon: 'antenna', key: 'how' },
  { icon: 'link', key: 'need' },
  { icon: 'shield', key: 'safe' },
  { icon: 'clock', key: 'slow' }
];

function telRenderIntro() {
  var html = '<div class="tel-intro-screen">' +
    '<div class="tel-intro-hero">' + icon('navTel') +
    '<h3 dir="auto">' + telEsc(telT('tel_intro_title')) + '</h3>' +
    '<p dir="auto">' + telEsc(telT('tel_intro_lead')) + '</p></div>';
  TEL_INTRO_CARDS.forEach(function (c) {
    html += '<div class="tel-intro-card">' +
      '<span class="tel-intro-ic">' + icon(c.icon) + '</span>' +
      '<span class="tel-intro-text"><b dir="auto">' + telEsc(telT('tel_intro_' + c.key + '_t')) + '</b>' +
      '<span dir="auto">' + telEsc(telT('tel_intro_' + c.key + '_b')) + '</span></span></div>';
  });
  html += '<button type="button" class="tel-btn tel-btn-primary" onclick="telDismissIntro()">' +
    telEsc(telT('tel_intro_ok')) + '</button>' +
    '</div>';
  return html;
}

// ------------------------------------------------------------- connection ----

async function telRefreshInfo() {
  try {
    telState.info = await telFetch('/api/tel/info');
    if (telState.info.reason) telState.error = telT(telState.info.reason);
  } catch (e) {
    // The backend is gone (restarted, or still starting). Keeping the last
    // answer would leave the page on a screen whose connection no longer
    // exists, where every button fails with "not connected".
    telState.error = telT('tel_err_backend');
    if (telState.info) {
      telState.info = Object.assign({}, telState.info, { connected: false, connecting: false });
    }
  }
  if (telState.info && telState.info.connected) await telAuthStatus();
  // hasSession is the backend's PERSISTED answer to "has Telegram confirmed
  // this session?", so it is what a freshly loaded page has to go on: nothing
  // is remembered in telState yet. Only clearing it here, never setting it,
  // meant every reload started out signed-out until the tunnel came back --
  // which is what put the phone-number screen in front of a signed-in user and
  // made the cached conversations look as if they had been wiped.
  else telState.authorized = !!(telState.info && telState.info.hasSession);
  // With a stored session the last known answer stands: the connection being
  // down says nothing about whether the account is signed in, and clearing it
  // here is what put the phone-number screen in front of a signed-in user.
  // A code hash belongs to the connection that issued it: signing in with one
  // from a dead connection is rejected as an invalid code.
  if (!telState.info || !telState.info.connected) telClearLoginStep();
  telRender();
  telAutoResume();
}

// telAutoResume rebuilds a connection that dropped on its own.
//
// Reconnecting costs about three queries -- two handshake fragments and the
// tunnel OPEN -- and reuses the stored auth key. That is less than fetching a
// single chat, so the first attempts come straight away rather than being
// rationed: a blip should heal before the user notices it. Only once it is
// clearly not a blip does it drop to a slow poll, which is what stops a server
// that is genuinely down from being hammered.
var TEL_RESUME_FAST_TRIES = 3;
var TEL_RESUME_FAST_MS = 1000;
var TEL_RESUME_SLOW_MS = 10000;
var telResumeAfter = 0;
var telResumeTries = 0;

function telAutoResume() {
  var info = telState.info || {};
  // Connected again: the next drop starts from scratch rather than serving out
  // a wait earned by an earlier failure.
  if (info.connected) { telResumeAfter = 0; telResumeTries = 0; return; }
  if (!info.resumeServer || info.connecting) return;
  if (telState.busy || telState.cancelled || telState.cancelling) return;
  var now = Date.now();
  if (now < telResumeAfter) return;
  telResumeTries++;
  telResumeAfter = now +
    (telResumeTries < TEL_RESUME_FAST_TRIES ? TEL_RESUME_FAST_MS : TEL_RESUME_SLOW_MS);
  telResume();
}

// telClearLoginStep drops the half-finished sign-in, which only means anything
// on the connection that started it.
function telClearLoginStep() {
  telState.codeHash = '';
  telState.codeVia = '';
  telState.needPassword = false;
  telState.passwordHint = '';
}

async function telAuthStatus() {
  try {
    var r = await telFetch('/api/tel/auth', telPost({ action: 'status' }));
    if (r.ok) { telState.authorized = !!r.authorized; telState.user = r.user || null; }
  } catch (e) { /* keep the last known state */ }
}

// Connect answers immediately and the work runs on the backend; /api/tel/info
// is followed until it settles.
async function telConnect(serverKey, attempt) {
  if (telState.busy && !attempt) return;
  // A running connect is not restarted by a second click.
  if (!attempt && telState.info && telState.info.connecting) return;
  telState.cancelled = false;
  telState.cancelling = false;
  telState.server = serverKey;
  telState.attempt = attempt || 1;
  await telOp('tel_connecting', async function () {
    var r = await telCall('/api/tel/connect', { server: serverKey }, 'tel_err_connect');
    if (!r) return;
    await telRefreshInfo();
    telFollowConnect();
  });
}
window.telConnect = telConnect;

// telFollowConnect polls while a connect is in flight.
function telFollowConnect() {
  clearTimeout(telState.followTimer);
  if (!telState.open) return;
  telState.followTimer = setTimeout(async function () {
    await telRefreshInfo();
    if (telState.cancelled) {
      // Still winding down: keep watching until the backend lets go.
      if (telState.info && telState.info.connecting) { telFollowConnect(); return; }
      telState.cancelling = false;
      telRender();
      return;
    }
    if (telState.info && telState.info.connecting) { telFollowConnect(); return; }
    if (telState.info && telState.info.connected) {
      if (telReady() && !telState.loadedOnce) telLoadDialogs();
      return;
    }
    // Retry: on a bad link one failure says little.
    if (telState.error && telState.attempt < TEL_MAX_ATTEMPTS && telState.server) {
      setTimeout(function () {
        if (!telState.cancelled) telConnect(telState.server, telState.attempt + 1);
      }, 3000);
    }
  }, 2000);
}

// telCancelConnect gives up on an attempt, following until the backend
// confirms it has stopped.
async function telCancelConnect() {
  telState.cancelled = true;
  telState.cancelling = true;
  telState.attempt = 0;
  telRender();
  try { await telFetch('/api/tel/disconnect', telPost({})); } catch (e) { }
  telState.busy = false;
  await telRefreshInfo();
  if (telState.info && telState.info.connecting) { telFollowConnect(); return; }
  telState.cancelling = false;
  telRender();
}
window.telCancelConnect = telCancelConnect;

async function telDisconnect() {
  telState.cancelled = true;
  clearTimeout(telState.followTimer);
  try { await telFetch('/api/tel/disconnect', telPost({})); } catch (e) { }
  telState.authorized = false;
  telState.peer = null;
  await telRefreshInfo();
}
window.telDisconnect = telDisconnect;

// telChangeServer drops the tunnel and returns to the picker, keeping the
// stored session.
async function telChangeServer() {
  // Same reasoning as telPickServer: only the half-finished login is bound to
  // the connection. The chats are the account's, so they survive and are there
  // the moment the new config connects.
  telState.codeHash = '';
  telState.codeVia = '';
  telState.needPassword = false;
  await telDisconnect();
}
window.telChangeServer = telChangeServer;

// telRecheck re-reads every config's metadata; the tunnel bit otherwise only
// arrives with ordinary feed traffic.
async function telRecheck() {
  await telOp('tel_rechecking', async function () {
    try {
      // The endpoint answers with the refreshed info, so no second call.
      telState.info = await telFetch('/api/tel/recheck', telPost({}));
      telState.error = telState.info.reason ? telT(telState.info.reason) : '';
    } catch (e) {
      telState.error = telT('tel_err_generic');
    }
  });
}
window.telRecheck = telRecheck;

// telResetData clears everything derived from one account.
function telResetData() {
  telState.dialogs = [];
  telState.threads = {};
  telState.cursor = null;
  telState.more = false;
  telState.total = 0;
  telState.loadedOnce = false;
  telState.peer = null;
  telState.draft = '';
}

// --------------------------------------------------------------- login -------

async function telSaveCredentials() {
  var id = parseInt(((document.getElementById('telApiId') || {}).value || '').trim(), 10);
  var hash = ((document.getElementById('telApiHash') || {}).value || '').trim();
  if (!id || !hash) { telState.error = telT('tel_creds_required'); telRender(); return; }
  var info = telState.info || {};
  // Nothing typed: just close the editor. Saving was spending a round trip --
  // the local write is free, but the info refresh behind it checks the
  // authorization over the tunnel -- to store the values that were already
  // there, and showing a progress bar for it suggested something happened.
  if (id === (info.apiId | 0) && hash === (info.apiHash || '')) {
    telState.editCreds = false;
    telRender();
    return;
  }
  // The stored session belongs to the app that created it, so changing these
  // signs the account out. Said before the fact, not discovered afterwards.
  if (info.hasSession && !confirm(telT('tel_creds_signout_confirm'))) return;
  await telOp('tel_working', async function () {
    var r = await telCall('/api/tel/credentials', { apiId: id, apiHash: hash });
    if (!r) return;
    telState.editCreds = false;
    if (r.signedOut) {
      telState.authorized = false;
      telState.user = null;
      telResetData();
    }
    await telRefreshInfo();
  });
}
window.telSaveCredentials = telSaveCredentials;

function telEditCredentials() { telState.editCreds = true; telCloseSheet(); telRender(); }
function telCancelCreds() { telState.editCreds = false; telRender(); }
window.telEditCredentials = telEditCredentials;
window.telCancelCreds = telCancelCreds;

// TEL_DIAL_CODES: local list, likeliest first; 'code' prefixes the number.
var TEL_DIAL_CODES = [
  { id: 'ir', code: '+98' },
  { id: 'tr', code: '+90' },
  { id: 'ae', code: '+971' },
  { id: 'de', code: '+49' },
  { id: 'gb', code: '+44' },
  { id: 'us', code: '+1' },
  { id: 'ca', code: '+1' },
  { id: 'fr', code: '+33' },
  { id: 'nl', code: '+31' },
  { id: 'se', code: '+46' },
  { id: 'it', code: '+39' },
  { id: 'es', code: '+34' },
  { id: 'ru', code: '+7' },
  { id: 'iq', code: '+964' },
  { id: 'af', code: '+93' },
  { id: 'az', code: '+994' },
  { id: 'am', code: '+374' },
  { id: 'ge', code: '+995' },
  { id: 'qa', code: '+974' },
  { id: 'om', code: '+968' },
  { id: 'au', code: '+61' },
  { id: 'my', code: '+60' },
  { id: 'in', code: '+91' }
];

function telDialCode() { return telState.dial || TEL_DIAL_CODES[0].code; }

function telOnDialChange(v) {
  telState.dial = v;
  var el = document.getElementById('telPhone');
  if (el) el.focus();
}
window.telOnDialChange = telOnDialChange;

// telFullPhone prefixes the dial code, leaving a full international number.
function telFullPhone() {
  var el = document.getElementById('telPhone');
  var typed = (el ? el.value : '') || telState.phone || '';
  var n = typed.replace(/[^0-9+]/g, '');
  if (n.indexOf('+') === 0) return n;
  return telDialCode() + n.replace(/^0+/, '');
}

async function telSendCode() {
  var phone = telFullPhone();
  if (!phone || phone === telDialCode()) { telState.error = telT('tel_err_phone_invalid'); telRender(); return; }
  telState.phone = phone;   // remembered, so a failure never clears the field
  await telOp('tel_sending_code', async function () {
    var r = await telCall('/api/tel/auth', { action: 'code', phone: phone }, 'tel_err_code');
    if (r) { telState.codeHash = r.codeHash; telState.codeVia = r.via || ''; }
  });
}
window.telSendCode = telSendCode;

// telResendCode asks for the code again; its new hash replaces ours, since a
// stale hash is rejected as an invalid code.
async function telResendCode() {
  await telOp('tel_sending_code', async function () {
    var r = await telCall('/api/tel/auth',
      { action: 'resend', phone: telState.phone, codeHash: telState.codeHash }, 'tel_err_code');
    if (r && r.codeHash) { telState.codeHash = r.codeHash; telState.codeVia = r.via || ''; }
  });
}
window.telResendCode = telResendCode;

async function telSignIn() {
  var code = ((document.getElementById('telCode') || {}).value || '').trim();
  if (!code) return;
  await telOp('tel_signing_in', async function () {
    var payload = {
      action: 'signin', phone: telState.phone, code: code, codeHash: telState.codeHash
    };
    try {
      var r = await telFetch('/api/tel/auth', telPost(payload));
      // A 2FA account needs the password step next; that is not a failure.
      if (r.needPassword) {
        telState.needPassword = true;
        telState.passwordHint = r.hint || '';
        return;
      }
      if (!r.ok) { telState.error = telReason(r, 'tel_err_signin'); return; }
    } catch (e) {
      telState.error = telT('tel_err_signin');
      return;
    }
    await telRefreshInfo();
    if (telReady()) telLoadDialogs();
  });
}
window.telSignIn = telSignIn;

async function telSignInPassword() {
  var pw = (document.getElementById('telPassword') || {}).value || '';
  if (!pw) return;
  await telOp('tel_signing_in', async function () {
    var r = await telCall('/api/tel/auth', { action: 'password', password: pw }, 'tel_err_signin');
    if (!r) return;
    telState.needPassword = false;
    await telRefreshInfo();
    if (telReady()) telLoadDialogs();
  });
}
window.telSignInPassword = telSignInPassword;

async function telLogout() {
  telCloseSheet();
  if (!confirm(telT('tel_logout_confirm'))) return;
  // Deliberately NOT inside telOp: telOp does nothing while another operation
  // holds the link, which would make "log out" during a fetch a no-op with
  // nothing on screen to say so. Signing out must always be possible, so it
  // takes no lock and waits for nobody.
  //
  // The account leaving this device is what matters: say so at once, abandon
  // whatever is in flight, and wipe over loopback -- no DNS link needed.
  telHistAbort();
  telState.authorized = false;
  telState.user = null;
  telState.codeHash = '';
  telState.codeVia = '';
  telState.needPassword = false;
  telResetData();
  telRender();
  try { await telFetch('/api/tel/auth', telPost({ action: 'logout' })); } catch (e) { }
  await telRefreshInfo();
}
window.telLogout = telLogout;

// ------------------------------------------------------------- dialogs -------

// telPrimeFromCache shows the conversations already on disk.
//
// Deliberately outside telOp: that lock exists to keep two things off the one
// tunnel, and this touches no tunnel at all. Taking it meant a connect in
// progress swallowed the load and the user stared at an empty list until the
// link came up -- the thing that looked like the cache had been wiped.
async function telPrimeFromCache() {
  if (telState.loadedOnce || telState.dialogs.length) return;
  var r = null;
  try {
    r = await telFetch('/api/tel/dialogs?cacheOnly=1&limit=' + TEL_DIALOG_PAGE);
  } catch (e) { return; }
  // An empty cache is not an error worth showing: the live load will fill it.
  if (!r || !r.ok || !r.page) return;
  if (telState.loadedOnce || telState.dialogs.length) return;
  // The cached answer carries Telegram's own total and a cursor, so the list
  // knows whether there is more and where it continues -- which is what puts
  // the "load more chats" button on screen. Nothing is fetched here: the rest
  // of the list costs a page of DNS queries each and is the user's to ask for.
  telApplyPage(r.page, true);
}
window.telPrimeFromCache = telPrimeFromCache;

// --------------------------------------------------- new-messages line -------
// Landing on the newest message is the worst place to open a conversation
// with unread posts: the user sees the end of a run with no way to tell where
// it started. The feed draws a line for this, and so does every Telegram
// client.
//
// The boundary is Telegram's own read mark (readInbox), not a local guess, so
// the line falls where it does on the phone.

// telSepFor is the message id the line sits after in this conversation, or 0.
function telSepFor(key) {
  var s = telState.sep[key];
  return s ? s.id : 0;
}

// telSepPin fixes the line at id and asks for one scroll to it.
//
// First pin wins until the sticky window runs out. A conversation that keeps
// receiving while it is open would otherwise walk the line forwards message by
// message, which both re-scrolls the reader and quietly redefines "new" as
// "the last one".
function telSepPin(key, id) {
  if (!id || !key) return;
  if (telState.sep[key]) return;
  telState.sep[key] = { id: id };
  telState.sepScroll = key;
  telSepArmCommit(key);
}

// telSepArmCommit drops the line once the sticky window is over, and renders so
// it actually disappears rather than waiting for the next refresh.
function telSepArmCommit(key) {
  var s = telState.sep[key];
  if (!s || s.timer) return;
  s.timer = setTimeout(function () {
    delete telState.sep[key];
    if (telState.sepScroll === key) telState.sepScroll = '';
    telRender();
  }, TEL_SEP_STICKY_MS);
}

// telSepFromUnread places the line from a count when the read mark is missing
// -- a row cached before the mark was carried, say.
//
// Counts incoming messages back from the newest: Telegram's unread count is
// exactly how many of those sit after the mark. Gives up rather than guessing
// when that many are not loaded; a line in the wrong place is worse than none.
function telSepFromUnread(th, n) {
  if (!n || !th || !th.msgs.length) return 0;
  var seen = 0;
  for (var i = th.msgs.length - 1; i >= 0; i--) {
    var m = th.msgs[i];
    if (m.out || m.service) continue;
    seen++;
    if (seen === n) return m.id - 1;
  }
  return 0;
}

// telScrollToSep puts the line at the top of the view. Returns false when it is
// not on screen, so the caller can keep the intent for a later render.
function telScrollToSep() {
  var el = document.getElementById('telNewSep');
  if (!el) return false;
  // Instant, not smooth: a smooth scroll janks the Android WebView compositor,
  // and the next render would interrupt the animation anyway.
  el.scrollIntoView({ block: 'start' });
  return true;
}

// telScrollToMsg puts one message on screen and flashes it. Returns false when
// that message is not loaded, so the caller can decide whether to go and get
// it -- over DNS that is a real cost, never something to do speculatively.
function telScrollToMsg(id) {
  var el = document.querySelector('#telMsgs .tel-msg[data-mid="' + (id | 0) + '"]');
  if (!el) return false;
  // Instant, not smooth: a long smooth scroll eats the highlight before it
  // arrives, and the animation janks the Android WebView compositor.
  el.scrollIntoView({ block: 'center' });
  el.classList.remove('tel-msg-flash');
  void el.offsetWidth; // restart the animation when the same message is hit twice
  el.classList.add('tel-msg-flash');
  return true;
}
window.telScrollToMsg = telScrollToMsg;

// telJumpToMsg goes to a message in the open conversation, fetching back
// towards it if it is not loaded yet.
//
// Bounded on purpose: each page back is a round trip over DNS, so a quote
// pointing years up a channel gives up rather than walking there for minutes.
async function telJumpToMsg(id) {
  if (!id || !telState.peer) return;
  if (telScrollToMsg(id)) return;
  var key = telPeerKey(telState.peer);
  for (var i = 0; i < TEL_JUMP_MAX_PAGES; i++) {
    var th = telThread(key);
    // Already older than the target and still not found: it was deleted, or
    // it lives in another conversation.
    if (th.msgs.length && th.msgs[0].id <= id) break;
    if (!th.more && th.fetched && th.msgs.length) break;
    await telThreadFetchOlder();
    if (telPeerKey(telState.peer) !== key) return; // the user moved on
    if (telScrollToMsg(id)) return;
  }
  telState.error = telT('tel_err_msg_not_found');
  telRender();
}
window.telJumpToMsg = telJumpToMsg;

// telJoinChannel subscribes to a channel being read through a link.
//
// Separate from opening it on purpose: following a link must never sign the
// user up to anything, so this only ever runs from the menu item they chose.
async function telJoinChannel(key) {
  var d = telDialogFor(key);
  if (!d) return;
  await telOp('tel_working', async function () {
    var r = await telCall('/api/tel/join', { peer: d.peer }, 'tel_err_generic');
    if (!r) return;
    d.visiting = false;
  });
  telRender();
  // The list Telegram keeps is the real one; ours was a placeholder.
  telRefreshDialogs(true);
}
window.telJoinChannel = telJoinChannel;

// telRefreshStats brings view and reaction counts on screen up to date.
//
// History only ever answers with messages, so a post read in its first minute
// would keep the counts it had then for ever: no reactions, a handful of
// views. This asks for the counters alone -- no message bodies cross the link
// -- so a whole screen costs a fraction of re-reading the page.
async function telRefreshStats(key, peer) {
  var th = telThread(key);
  var ids = telStatsWanted(th);
  if (!ids.length) return;
  // Views and reactions are two Telegram methods and therefore two round
  // trips. Asked separately so each draws its own bar under its own name:
  // as one request they produced two identical bars and the second looked
  // like the first had restarted.
  var wants = [];
  if (peer.kind === 'channel' || peer.kind === 'forum') {
    wants.push({ what: 'views', label: 'tel_updating_views' });
  }
  wants.push({ what: 'reactions', label: 'tel_updating_reactions' });
  telStepsMore(telState.stepNo + wants.length);

  var stats = [];
  for (var w = 0; w < wants.length; w++) {
    // Through telOp like any other round trip: it holds the one tunnel for as
    // long as a fetch does, and a wait with no progress bar is a wait the user
    // cannot account for.
    var part = await telOp(wants[w].label, function (wq) {
      return function () {
        return telCallQuiet('/api/tel/stats', { peer: peer, ids: ids, what: wq });
      };
    }(wants[w].what));
    if (part && part.stats) stats = stats.concat(part.stats);
    // The conversation may have been left behind between the two.
    if (telPeerKey(telState.peer) !== key) return;
  }
  var r = { stats: stats };
  if (!r.stats.length) return;
  // The user may have moved on while this was in flight.
  if (telPeerKey(telState.peer) !== key) return;
  var now = Date.now();
  if (!th.statsAt) th.statsAt = {};
  ids.forEach(function (id) { th.statsAt[id] = now; });
  var byID = {};
  r.stats.forEach(function (s) { byID[s.id] = s; });
  var changed = false;
  th.msgs.forEach(function (m) {
    var s = byID[m.id];
    if (!s) return;
    // A missing count is Telegram declining to say, not a drop to zero.
    if (s.views && s.views > (m.views || 0)) { m.views = s.views; changed = true; }
    if (s.reactions && s.reactions.length) { m.reactions = s.reactions; changed = true; }
  });
  if (changed) telRender();
}
window.telRefreshStats = telRefreshStats;

// telStatsWanted picks which messages to ask about: the ones ON SCREEN, minus
// any asked about recently.
//
// The counters belong to what the reader is looking at. Asking about every
// message ever loaded made one refresh cost more than fetching the messages
// had, and asking again about a post scrolled past twice in ten seconds spent
// the link on numbers that could not have moved.
function telStatsWanted(th) {
  var now = Date.now();
  var fresh = th.statsAt || {};
  var visible = telVisibleMsgIDs();
  var pool = visible.length ? visible : th.msgs.map(function (m) { return m.id; });
  var out = [];
  for (var i = pool.length - 1; i >= 0 && out.length < TEL_STATS_MAX; i--) {
    var id = pool[i];
    if (!id || id <= 0) continue;
    if (fresh[id] && now - fresh[id] < TEL_STATS_MIN_MS) continue;
    out.push(id);
  }
  return out.reverse();
}

// telVisibleMsgIDs lists the messages inside the scroller's viewport.
function telVisibleMsgIDs() {
  var box = document.getElementById('telMsgs');
  if (!box) return [];
  var top = box.getBoundingClientRect().top, bottom = top + box.clientHeight;
  var out = [];
  box.querySelectorAll('.tel-msg[data-mid]').forEach(function (el) {
    var r = el.getBoundingClientRect();
    if (r.bottom < top || r.top > bottom) return; // fully off screen
    var id = parseInt(el.getAttribute('data-mid'), 10);
    if (id > 0) out.push(id);
  });
  return out;
}

// telStatsOnScroll asks about whatever scrolling brought into view, once it
// stops. Bounded by the same ten-second rule, so a reader moving up and down a
// channel does not turn the link into a counter feed.
var telStatsScrollTimer = 0;
function telStatsOnScroll() {
  clearTimeout(telStatsScrollTimer);
  telStatsScrollTimer = setTimeout(function () {
    var peer = telState.peer;
    if (!peer || telState.busy || telState.bg) return;
    if (!telReady()) return;
    var key = telPeerKey(peer);
    if (!telStatsWanted(telThread(key)).length) return;
    telRefreshStats(key, peer);
  }, TEL_STATS_SCROLL_MS);
}
window.telStatsOnScroll = telStatsOnScroll;

// telPrimeThreadFromCache fills a conversation from what is already on disk.
// Outside telOp for the same reason as telPrimeFromCache: no tunnel involved.
async function telPrimeThreadFromCache(key, peer) {
  var r = null;
  try {
    r = await telFetch(telHistoryQuery(peer, { limit: TEL_HISTORY_PAGE }) + '&cacheOnly=1');
  } catch (e) { return; }
  if (!r || !r.ok || !r.messages || !r.messages.length) return;
  // The user may have moved on while this was in flight.
  if (telPeerKey(telState.peer) !== key) return;
  var th = telThread(key);
  if (th.msgs.length) return;
  // NOT live: these came off the disk, so their counts are as old as the
  // cache is and the refresh that follows must still ask about them.
  telMergeMessages(th, r.messages, false);
  telState.jumpToBottom = true;
  telRender();
}
window.telPrimeThreadFromCache = telPrimeThreadFromCache;

// telLoadDialogs fetches the first page and replaces the list.
//
// force=true means the user asked Telegram, so the backend must not answer
// from its cache: that is the only way a conversation started on another
// device shows up here.
async function telLoadDialogs(force) {
  await telOp('tel_loading_chats', async function () {
    var r = await telCall('/api/tel/dialogs?limit=' + TEL_DIALOG_PAGE +
      (force ? '&refresh=1' : ''));
    if (!r) return;
    telApplyPage(r.page, true);
    telState.loadedOnce = true;
    // ONE page, and that is all: the rest is the user's to ask for, one page
    // per press of "load more chats".
  });
}
window.telLoadDialogs = telLoadDialogs;

// telRefreshDialogs merges the newest page into the cache.
async function telRefreshDialogs(background) {
  var run = async function () {
    var before = telUnreadTotal();
    // A manual ⟳ is the user asking Telegram; a background pass is happy with
    // whatever the backend's own polling has already learnt.
    var r = await telCall('/api/tel/dialogs?limit=' + TEL_DIALOG_PAGE +
      (background ? '' : '&refresh=1'));
    if (!r) return;
    telApplyPage(r.page, false);
    var grew = telUnreadTotal() - before;
    if (background && grew > 0) telNotifyNew(grew);
  };
  if (background) await telBgOp('tel_updating_chats', run);
  else await telOp('tel_loading_chats', run);
}
window.telRefreshDialogs = telRefreshDialogs;

// telLoadMoreDialogs pulls the next page from the cursor.
async function telLoadMoreDialogs(background) {
  if (!telState.more || !telState.cursor) return;
  var c = telState.cursor;
  var q = '/api/tel/dialogs?limit=' + TEL_DIALOG_PAGE +
    '&offsetDate=' + encodeURIComponent(c.date || 0) +
    '&offsetId=' + encodeURIComponent(c.id || 0);
  // Telegram pages from this peer, and cannot address it without its hash.
  if (c.peer && c.peer.id) {
    q += '&offsetKind=' + encodeURIComponent(c.peer.kind) +
      '&offsetAccessHash=' + encodeURIComponent(c.peer.accessHash || 0);
  }
  var ran = false;
  var added = 0;
  var run = async function () {
    ran = true;
    var r = await telCall(q);
    if (r) added = telApplyPage(r.page, false);
  };
  if (background) await telBgOp('tel_loading_chats', run);
  else await telOp('tel_loading_chats', run);
  // null means it never ran: telOp and telBgOp drop a call when something
  // else holds the link. The caller has to be able to tell that apart from a
  // page that genuinely brought nothing.
  return ran ? added : null;
}
window.telLoadMoreDialogs = telLoadMoreDialogs;

// telLoadMoreNow is the "load more chats" button. It WAITS for the link rather
// than being dropped: the list refreshes every minute and a fetch takes tens
// of seconds, so a press almost always lands while something holds the tunnel.
// Disabling or silently swallowing it leaves the button the user needs most
// dead most of the time, with nothing to say why.
async function telLoadMoreNow() {
  if (telState.morePending) return;
  telState.morePending = true;
  telRender();
  try {
    // A few goes: the link may be taken again between the wait ending and the
    // call starting. Bounded, so a tunnel that never frees up gives the
    // button back instead of spinning for ever.
    for (var i = 0; i < TEL_MORE_TRIES; i++) {
      await telWhenIdle();
      var added = await telLoadMoreDialogs(false);
      if (added !== null) return;
    }
  } finally {
    telState.morePending = false;
    telRender();
  }
}
window.telLoadMoreNow = telLoadMoreNow;

// telApplyPage merges a page in; replace only on an explicit reload, or the
// list shrinks back to one page on every refresh.
function telApplyPage(page, replace) {
  if (!page) return 0;
  var rows = page.dialogs || [];
  if (replace) telState.dialogs = [];
  var byKey = {};
  telState.dialogs.forEach(function (d, i) { byKey[telPeerKey(d.peer)] = i; });
  var added = 0;
  rows.forEach(function (d) {
    var k = telPeerKey(d.peer);
    if (byKey[k] === undefined) {
      byKey[k] = telState.dialogs.length;
      telState.dialogs.push(d);
      added++;
    } else {
      // Keep the cached row's object identity but take the fresh values.
      var cur = telState.dialogs[byKey[k]];
      cur.peer = d.peer;             // a renamed chat / rotated access hash
      cur.unread = d.unread;
      cur.topMsg = d.topMsg;
      cur.when = d.when;             // the list's stamp, or it freezes
      cur.readOutbox = d.readOutbox; // how far the other side has read
      cur.readInbox = d.readInbox;   // how far WE have, on any device
      cur.pinned = d.pinned;
      // Muted was missing from this list, and a row that already existed
      // therefore kept whatever it was first fetched with FOR EVER. Telegram
      // sent the right value, the backend stored the right value, and the row
      // the user is looking at ignored it -- so a channel silenced on the
      // phone went on ringing here however many times the list was refreshed.
      cur.muted = d.muted;
      // Whether that mute value is an answer or a placeholder. The backend
      // repairs such rows from Telegram; carrying the flag keeps the row here
      // faithful to the one there.
      cur.notifyUnknown = d.notifyUnknown;
      if (d.preview) cur.preview = d.preview;
    }
  });
  // Pinned first, then by the newest message we know of — Telegram's own order.
  telState.dialogs.sort(function (a, b) {
    if (!!a.pinned !== !!b.pinned) return a.pinned ? -1 : 1;
    return (b.topMsg || 0) - (a.topMsg || 0);
  });
  telState.more = !!page.more && telState.dialogs.length < TEL_DIALOG_MAX;
  telState.total = page.totalKnown || telState.total;
  telState.cursor = page.more
    ? { date: page.nextDate || 0, id: page.nextId || 0, peer: page.nextPeer || null }
    : null;
  telRender();
  return added;
}

// telDialogPagesExpected is how many fetches the whole list will take, from
// what Telegram said the first page.
function telDialogPagesExpected() {
  var known = telState.total || telState.dialogs.length;
  var reach = Math.min(known, TEL_DIALOG_MAX);
  return Math.max(1, Math.ceil(reach / TEL_DIALOG_PAGE));
}

// The list is fetched ONE page at a time, and only when asked. Walking the
// rest in the background costs six or seven pages of DNS queries on a large
// account before the section settles, and it yields the link to whatever the
// user is doing, so it never finishes. One press, one page, cached like the
// first.

function telSetTab(key) {
  telState.tab = key;
  telRender();
}
window.telSetTab = telSetTab;

// telToggleSearch opens or closes the search box for one surface: 'list' or
// 'thread'.
function telToggleSearch(where) {
  telState.searchIn = (telState.searchIn === where) ? '' : where;
  telState.search = '';
  telRender();
  var el = document.getElementById('telSearch');
  if (el) el.focus();
}
window.telToggleSearch = telToggleSearch;

function telOnSearch(v) {
  telState.search = v;
  telRender();
}
window.telOnSearch = telOnSearch;

// telSearchBarHTML draws the box on `where` only; two would share an id.
function telSearchBarHTML(where, placeholderKey) {
  if (telState.searchIn !== where) return '';
  return '<div class="tel-search-row">' +
    '<span class="tel-search-ic">' + icon('magnifier') + '</span>' +
    '<input id="telSearch" class="tel-input tel-search" type="search" dir="auto" ' +
    'placeholder="' + telAttrQ(telT(placeholderKey)) + '" value="' + telAttrQ(telState.search) + '" ' +
    'oninput="telOnSearch(this.value)">' +
    '<button class="tel-icon-btn" aria-label="' + telAttrQ(telT('tel_close')) +
    '" onclick="telToggleSearch(\'' + where + '\')">' + icon('close') + '</button></div>';
}

function telSearchBtnHTML(where) {
  return '<button class="tel-icon-btn' + (telState.searchIn === where ? ' active' : '') +
    '" title="' + telAttrQ(telT('tel_search')) + '" aria-label="' + telAttrQ(telT('tel_search')) +
    '" onclick="telToggleSearch(\'' + where + '\')">' + icon('magnifier') + '</button>';
}

// telSearchFor is the query in force on one surface, empty elsewhere.
function telSearchFor(where) {
  return telState.searchIn === where ? telState.search.trim().toLowerCase() : '';
}

function telTabDef(key) {
  for (var i = 0; i < TEL_TABS.length; i++) if (TEL_TABS[i].key === key) return TEL_TABS[i];
  return TEL_TABS[0];
}

// telTabRows is the list for one tab, narrowed by the search box.
function telTabRows(key) {
  var def = telTabDef(key);
  var q = telSearchFor('list');
  return telState.dialogs.filter(function (d) {
    if (def.kinds && def.kinds.indexOf(d.peer.kind) < 0) return false;
    if (!q) return true;
    return (d.peer.title || '').toLowerCase().indexOf(q) >= 0 ||
      (d.peer.username || '').toLowerCase().indexOf(q) >= 0 ||
      (d.preview || '').toLowerCase().indexOf(q) >= 0;
  });
}

// telUnreadTotal separates "something arrived" from "the list changed".
function telUnreadTotal() {
  var n = 0;
  telState.dialogs.forEach(function (d) { n += d.unread || 0; });
  return n;
}

// telTabUnread counts unread conversations in a tab; its dot.
function telTabUnread(key) {
  var n = 0;
  telTabRows(key).forEach(function (d) { if (d.unread > 0) n++; });
  return n;
}

// -------------------------------------------------------------- thread -------

function telThread(key) {
  // fetched records that a history request has COMPLETED for this thread.
  // Without it an untouched thread is indistinguishable from an empty one, and
  // the moment before its first fetch starts reads as "no messages".
  if (!telState.threads[key]) telState.threads[key] = { msgs: [], more: true, fetched: false };
  return telState.threads[key];
}

function telDialogFor(key) {
  for (var i = 0; i < telState.dialogs.length; i++) {
    if (telPeerKey(telState.dialogs[i].peer) === key) return telState.dialogs[i];
  }
  return null;
}

// telEnterThread switches the UI into a conversation: state, history layer and
// the mobile pane swap. Fetching is the caller's.
function telEnterThread(key, peer) {
  var th = telThread(key);
  th.peer = peer;
  telState.peer = peer;
  telState.draft = '';
  telState.search = '';
  telState.searchIn = '';
  // Each conversation opens at its newest message, whatever the last one did.
  telState.jumpToBottom = true;
  if (!telThreadPushed) {
    try { history.pushState({ view: 'telThread' }, ''); telThreadPushed = true; } catch (e) { }
  }
  if (typeof mobileQuery !== 'undefined' && mobileQuery.matches) {
    var app = document.getElementById('app'); if (app) app.classList.add('chat-open');
  }
  document.documentElement.classList.add('tel-thread');
  telRender();
  return th;
}

// telOpenPeer renders the cached messages at once and fetches only the delta.
async function telOpenPeer(key) {
  var d = telDialogFor(key);
  var peer = d ? d.peer : (telState.threads[key] && telState.threads[key].peer);
  if (!peer) return;
  telCloseSheet();
  // Whatever the previous conversation was still fetching, nobody is waiting
  // for it any more -- and it is holding the only link.
  if (telPeerKey(telState.peer) !== key) telHistAbort();
  // Read before anything clears the badge: once unread is zeroed at the end of
  // this, there is nothing left that knows where the unread run began.
  var unreadAt = d ? (d.unread || 0) : 0;
  if (unreadAt > 0) telSepPin(key, d.readInbox || 0);
  var th = telEnterThread(key, peer);
  telScheduleNext();
  // Show what we already have before asking for more. A page of history costs
  // about half a minute over DNS, and spending it in front of a blank pane is
  // what made an opened channel look like it had no posts.
  if (!th.msgs.length) await telPrimeThreadFromCache(key, peer);
  // Second chance for a row cached before the read mark was carried: counting
  // back needs the messages, which have only just arrived.
  if (unreadAt > 0) telSepPin(key, telSepFromUnread(th, unreadAt));
  // Wait for the tunnel; a stale open (the user moved on) does nothing.
  await telWhenIdle();
  if (telPeerKey(telState.peer) !== key) return;
  // The messages, then each counter: views and reactions are separate
  // Telegram methods, so a channel is three round trips and a private chat
  // (which has no views) is two.
  // One step planned: the messages. The counter refresh widens this itself
  // (telStepsMore) only if it actually has something to ask about -- messages
  // that just arrived carry current counts, so usually it has nothing, and
  // promising three steps up front would have been a promise of two round
  // trips that never happen.
  telSteps(1, peer.title);
  if (!th.msgs.length) await telThreadFetchOlder();
  else await telThreadFetchNew();
  // Counters last, and only for a conversation still open: the messages
  // matter more than the numbers on them, so they get the link first.
  if (telPeerKey(telState.peer) === key) await telRefreshStats(key, peer);
  telStepsDone();
  // Last chance: on a cold open the history fetch above is what brought the
  // messages, so neither attempt earlier had anything to count back through.
  if (unreadAt > 0 && telPeerKey(telState.peer) === key) {
    telSepPin(key, telSepFromUnread(th, unreadAt));
  }
  // Clear the unread badge once the messages are actually on screen.
  if (d && d.unread > 0) {
    d.unread = 0;
    telRender();
    telMarkRead(peer, telNewestRealID(th) || d.topMsg || 0);
  }
}
window.telOpenPeer = telOpenPeer;

function telShowList() {
  telState.peer = null;
  telState.search = '';
  telState.searchIn = '';
  document.documentElement.classList.remove('tel-thread');
  var app = document.getElementById('app'); if (app) app.classList.remove('chat-open');
  telScheduleNext();
  telRender();
}

// telBackToList routes through history, like the hardware back key.
function telBackToList() {
  if (telThreadPushed) { try { history.back(); } catch (e) { } return; }
  telShowList();
}
window.telBackToList = telBackToList;

// telThreadFetchNew asks only for what is newer than the newest message held.
async function telThreadFetchNew(background) {
  var peer = telState.peer;
  if (!peer) return;
  var key = telPeerKey(peer);
  var th = telThread(key);
  // Cursor from the newest ACKNOWLEDGED id: a placeholder's synthetic id
  // would ask for messages newer than one that does not exist.
  var was = telNewestRealID(th);
  var q = telHistoryQuery(peer, { limit: TEL_HISTORY_PAGE, minId: was });
  var run = async function () {
    telHistCtl = new AbortController();
    var r = await telCall(q, undefined, undefined, telHistCtl.signal);
    if (!r) return;
    // Answered, so "no messages" is now a fact about the thread rather than
    // about how far we have got.
    th.fetched = true;
    var msgs = r.messages || [];
    var added = telMergeMessages(th, msgs, true);
    if (added) {
      telTouchPreview(key, th);
      // Only what the other side sent is news; our own echo is not.
      var incoming = msgs.filter(function (m) { return !m.out && !m.service; }).length;
      // Posts that landed while the conversation was open get the line too.
      // The boundary is where we were before this fetch, so it reads as "from
      // here down is what arrived while you were looking at this".
      if (incoming && was) telSepPin(key, was);
      if (background && incoming) telNotifyNew(incoming, peer.title);
      // No forced scroll: telRender holds the reader's place.
      telRender();
    }
  };
  if (background) await telBgOp('tel_checking_new', run);
  else await telOp('tel_refreshing', run);
}
window.telThreadFetchNew = telThreadFetchNew;

// telThreadFetchOlder pages BACKWARDS from the oldest message held.
async function telThreadFetchOlder() {
  var peer = telState.peer;
  if (!peer) return;
  var key = telPeerKey(peer);
  var th = telThread(key);
  var offsetID = th.msgs.length ? th.msgs[0].id : 0;
  await telOp('tel_loading_messages', async function () {
    telHistCtl = new AbortController();
    var r = await telCall(telHistoryQuery(peer, { limit: TEL_HISTORY_PAGE, offsetId: offsetID }),
      undefined, undefined, telHistCtl.signal);
    if (!r) return;
    th.fetched = true;
    var msgs = r.messages || [];
    var added = telMergeMessages(th, msgs, true);
    // A short answer is the start of the conversation: stop offering "older".
    th.more = msgs.length >= TEL_HISTORY_PAGE && added > 0;
    // Prepended: the next render anchors from the bottom.
    if (added && offsetID) telState.prepended = true;
    telTouchPreview(key, th);
  });
  // The first page opens at the newest message; later ones hold the place.
  if (offsetID === 0) {
    telState.jumpToBottom = true;
    telRender();
  }
}
window.telThreadFetchOlder = telThreadFetchOlder;

function telHistoryQuery(peer, opts) {
  var q = '/api/tel/history?kind=' + encodeURIComponent(peer.kind) +
    '&id=' + encodeURIComponent(peer.id) +
    '&accessHash=' + encodeURIComponent(peer.accessHash || 0) +
    '&limit=' + (opts.limit || TEL_HISTORY_PAGE);
  if (opts.offsetId) q += '&offsetId=' + encodeURIComponent(opts.offsetId);
  if (opts.minId) q += '&minId=' + encodeURIComponent(opts.minId);
  return q;
}

// telNewestRealID is the newest id Telegram has acknowledged.
function telNewestRealID(th) {
  for (var i = th.msgs.length - 1; i >= 0; i--) {
    if (th.msgs[i].id < TEL_PENDING_BASE) return th.msgs[i].id;
  }
  return 0;
}

// telMergeMessages folds an answer in, oldest-first; returns how many were new.
// live says these came from Telegram just now, as opposed to off the disk.
//
// It decides whether their view and reaction counts may be treated as current.
// A message straight from Telegram carries the counts Telegram has for it
// RIGHT NOW, so asking again immediately is two round trips over DNS per
// conversation opened, for numbers that just arrived. The same messages read
// back from the cache carry whatever the counts were when they were stored --
// which may be days old, and is the whole reason the counter refresh exists.
// Getting this backwards means a post read in its first minute keeps its empty
// reaction row for ever.
function telMergeMessages(th, msgs, live) {
  var have = {};
  th.msgs.forEach(function (m) { have[m.id] = true; });
  var added = 0;
  var now = Date.now();
  if (!th.statsAt) th.statsAt = {};
  msgs.forEach(function (m) {
    if (live) th.statsAt[m.id] = now;
    if (have[m.id]) return;
    have[m.id] = true;
    th.msgs.push(m);
    added++;
  });
  if (added) th.msgs.sort(function (a, b) { return a.id - b.id; });
  return added;
}

// telTouchPreview keeps the list row in step without another dialogs fetch.
function telTouchPreview(key, th) {
  var d = telDialogFor(key);
  if (!d || !th.msgs.length) return;
  // topMsg orders the list, so a placeholder's id must never reach it.
  var newest = telNewestRealID(th);
  if (newest) d.topMsg = Math.max(d.topMsg || 0, newest);
  var last = th.msgs[th.msgs.length - 1];
  if (last.text) d.preview = last.text;
  if (last.date) d.when = Math.max(d.when || 0, last.date);
}

async function telMarkRead(peer, maxID) {
  if (!maxID) return;
  // Best effort: an unread badge that lingers is not worth an error banner.
  try { await telFetch('/api/tel/read', telPost({ peer: peer, maxId: maxID })); } catch (e) { }
}

async function telSend() {
  var box = document.getElementById('telInput');
  // The draft in state survives a render that rebuilt the box.
  var text = ((box && box.value) || telState.draft || '').trim();
  if (!text || !telState.peer) return;
  var peer = telState.peer;
  var key = telPeerKey(peer);
  var th = telThread(key);

  // Shown at once with a clock: a send takes seconds to minutes here.
  var pending = {
    id: TEL_PENDING_BASE + (++telPendingSeq),
    date: Math.floor(Date.now() / 1000), text: text, out: true, pending: true
  };
  th.msgs.push(pending);
  telState.draft = '';
  if (box) box.value = '';
  telState.jumpToBottom = true;
  telRender();

  // Wait for the tunnel rather than letting telOp refuse the send.
  await telWhenIdle();
  // Two round trips: the send, then the read mark. A third is added below if
  // Telegram does not name the message in its answer.
  telSteps(2, peer.title);
  var ran = await telOp('tel_sending', async function () {
    var r = await telCall('/api/tel/send', { peer: peer, text: text }, 'tel_err_send');
    if (!r) {
      // Kept on screen, marked, so the text is not lost.
      pending.pending = false;
      pending.failed = true;
      return;
    }
    telThread(telPeerKey(peer)).fetched = true;
    if (r.msgId) {
      // Telegram told us the id in the send's own answer, so the placeholder
      // simply becomes the real message. Reading it back cost a second round
      // trip -- about half the wait -- to learn something already known.
      pending.id = r.msgId;
      pending.pending = false;
      // A placeholder sorts last on its synthetic id, so if anything the other
      // side sent arrived while this one was in flight, that message is now
      // sitting after ours. Nothing was ADDED here, so the merge's own sort
      // never runs: order it explicitly or the two stay swapped until the next
      // fetch happens to add something.
      th.msgs.sort(function (a, b) { return a.id - b.id; });
      telTouchPreview(key, th);
      return true;
    }
    // It did not say (some chats answer with a bare update), so fall back to
    // the delta -- one more round trip.
    telStepsMore(3);
    telStepAdvance();
    var h = await telCall(telHistoryQuery(peer, { limit: 10, minId: telNewestRealID(th) }));
    if (h) {
      telMergeMessages(th, h.messages || [], true);
      telDropPending(th, pending);
      telTouchPreview(key, th);
    }
    return true;
  });
  // Refused anyway: mark it, rather than leave a clock that never resolves.
  if (!ran) {
    pending.pending = false;
    pending.failed = true;
    telState.error = telT('tel_err_busy');
  }
  telState.jumpToBottom = true;
  telRender();
  await telRefreshTicks();
  telStepsDone();
}
window.telSend = telSend;

function telDropPending(th, pending) {
  th.msgs = th.msgs.filter(function (m) { return m !== pending; });
}

// telRetrySend re-sends a message that failed, reusing its placeholder.
async function telRetrySend(id) {
  var th = telThread(telPeerKey(telState.peer));
  var msg = th.msgs.filter(function (m) { return m.id === id; })[0];
  if (!msg) return;
  telDropPending(th, msg);
  telState.draft = msg.text;
  var box = document.getElementById('telInput');
  if (box) box.value = msg.text;
  await telSend();
}
window.telRetrySend = telRetrySend;

// telRefreshTicks re-reads this conversation's row for the read mark, only
// while a sent message is still waiting for its second tick.
async function telRefreshTicks(background) {
  var peer = telState.peer;
  if (!peer) return;
  var key = telPeerKey(peer);
  if (!telTicksPending(key)) return;
  var run = async function () {
    var r = await telCall('/api/tel/peer?kind=' + encodeURIComponent(peer.kind) +
      '&id=' + encodeURIComponent(peer.id) +
      '&accessHash=' + encodeURIComponent(peer.accessHash || 0));
    if (!r || !r.dialog) return;
    var d = telDialogFor(key);
    if (d) {
      d.readOutbox = r.dialog.readOutbox;
      d.unread = r.dialog.unread;
    } else {
      telState.dialogs.unshift(r.dialog);
    }
    telRender();
  };
  if (background) await telBgOp('tel_checking_ticks', run);
  else await telOp('tel_checking_ticks', run);
}

// telTicksPending reports a sent message the other side has not read yet.
function telTicksPending(key) {
  var th = telState.threads[key];
  if (!th) return false;
  var d = telDialogFor(key);
  var read = (d && d.readOutbox) || 0;
  for (var i = th.msgs.length - 1; i >= 0; i--) {
    var m = th.msgs[i];
    if (!m.out || m.pending || m.failed) continue;
    return m.id > read;
  }
  return false;
}
function telInputKey(e) {
  if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); telSend(); }
}
window.telInputKey = telInputKey;

// telGrowInput grows the composer with the message; CSS caps the height.
function telGrowInput(el) {
  el.style.height = 'auto';
  el.style.height = el.scrollHeight + 'px';
}
window.telGrowInput = telGrowInput;

// telDeleteChat leaves a channel, or clears a conversation's history.
async function telDeleteChat(key) {
  telCloseSheet();
  var d = telDialogFor(key);
  if (!d) return;
  var isChannel = d.peer.kind === 'channel' || d.peer.kind === 'forum';
  if (!confirm(telT(isChannel ? 'tel_leave_confirm' : 'tel_delete_confirm'))) return;
  await telOp('tel_working', async function () {
    var r = await telCall('/api/tel/delete', { peer: d.peer, revoke: false });
    if (!r) return;
    telState.dialogs = telState.dialogs.filter(function (x) { return telPeerKey(x.peer) !== key; });
    delete telState.threads[key];
    if (telPeerKey(telState.peer) === key) telShowList();
    showToast(telT('tel_deleted'));
  });
}
window.telDeleteChat = telDeleteChat;

// telStartNewChat opens a conversation with an @username not in the list.
async function telStartNewChat() {
  var el = document.getElementById('telNewName');
  var name = (el ? el.value : '').trim();
  if (!name) return;
  telCloseSheet();
  await telOp('tel_working', async function () {
    var r = await telCall('/api/tel/resolve', { username: name }, 'tel_err_username');
    if (!r || !r.peer) return;
    var key = telPeerKey(r.peer);
    var th = telThread(key);
    th.peer = r.peer;
    // Put it at the top of the list so it has a row to come back to.
    if (!telDialogFor(key)) {
      // visiting: opened by name or through a link, not subscribed to. The
      // join offer hangs off this, so it only appears where joining means
      // something.
      telState.dialogs.unshift({ peer: r.peer, unread: 0, topMsg: 0, preview: '', visiting: true });
    }
    // Opened inline: telOpenPeer would wait for the tunnel this call holds.
    var th = telEnterThread(key, r.peer);
    var h = await telCall(telHistoryQuery(r.peer, { limit: TEL_HISTORY_PAGE }));
    telThread(telPeerKey(r.peer)).fetched = true;
    if (h) {
      telMergeMessages(th, h.messages || [], true);
      telTouchPreview(key, th);
    }
    telState.jumpToBottom = true;
    telRender();
  });
}
window.telStartNewChat = telStartNewChat;

// --------------------------------------------------------------- timers ------
// One ticker drives the countdown and the auto-refresh.

function telStartTimers() {
  telStopTimers();
  telState.ticker = setInterval(telTick, 1000);
  telScheduleNext();
}

function telStopTimers() {
  clearInterval(telState.ticker); telState.ticker = null;
  clearTimeout(telState.followTimer);
}

// telScreenKey is the part of info that decides WHICH screen is shown, so a
// change in it means the page has to be rebuilt rather than repainted.
function telScreenKey(info) {
  if (!info) return 'none';
  return [!!info.hasCredentials, !!info.connected, !!info.connecting,
    !!info.hasSession, info.resumeServer || '', info.server || ''].join('|');
}

function telPollInterval() {
  return TEL_POLL_MS;
}

function telScheduleNext() { telState.nextAt = Date.now() + telPollInterval(); }

function telTick() {
  if (!telState.open) return;
  telPaintCountdown();
  // Here as well as after a refresh: the first retries are a second apart, and
  // only this loop runs that often.
  telAutoResume();
  telState.ticks = (telState.ticks || 0) + 1;
  // In flight: only the query counter is worth refreshing, every other tick.
  if (telState.busy || telState.bg || (telState.info && telState.info.connecting)) {
    if (telState.ticks % 2 === 0) {
      telFetch('/api/tel/info').then(function (info) {
        var before = telScreenKey(telState.info);
        telState.info = info;
        telPaintProgress();
        // The cheap paint above moves the counter but cannot move between
        // screens, and this is the one place info is replaced without a full
        // render. Asking "did the connecting flag just clear?" was too narrow:
        // when the FIRST fetch had failed, info went from absent to present
        // here, and the page stayed for ever on the screen chosen with no info
        // at all -- which asks for an api id and hash already stored.
        if (telScreenKey(info) !== before) telRender();
      }).catch(function () { });
    }
    return;
  }
  if (!telReady() || document.hidden) return;
  if (Date.now() < telState.nextAt) return;
  telScheduleNext();
  if (telState.peer) {
    // One extra row only while a tick is outstanding.
    telThreadFetchNew(true).then(function () { telRefreshTicks(true); });
  } else {
    telRefreshDialogs(true);
  }
}

// telRefreshNow is the ⟳ button; it also restarts the countdown.
//
// `what` says WHICH ⟳ was pressed -- 'list' for the sidebar's, 'thread' for
// the open conversation's. It has to be told, not inferred from whether a
// conversation is open: on desktop both panes are on screen at once, so the
// sidebar's button was refreshing the open chat instead of the list, and a
// message arriving in any other conversation showed no count until the
// background pass came round a minute later. On mobile only one of the two is
// ever visible, which is why asking the state looked right.
// No argument (the ⋮ menu, which exists in every state) keeps that: refresh
// whatever is in front.
async function telRefreshNow(what) {
  telScheduleNext();
  if (!telReady()) { await telRefreshInfo(); return; }
  var thread = what ? what === 'thread' : !!telState.peer;
  if (thread && telState.peer) {
    var rk = telPeerKey(telState.peer), rp = telState.peer;
    telSteps(1, telState.peer.title); // the messages; the counters add their own
    await telThreadFetchNew();
    // Refreshing a conversation means its counts too, in a private chat as
    // much as a channel: reactions move on messages whose text never will.
    if (telPeerKey(telState.peer) === rk) await telRefreshStats(rk, rp);
    telStepsDone();
  } else await telRefreshDialogs();
}
window.telRefreshNow = telRefreshNow;

// --------------------------------------------------------------- render ------
// Sidebar: every pre-conversation state and the list. Pane: the open
// conversation. Mobile shows one at a time, desktop both.

function telRender() {
  if (!telState.open) return;
  // The floating search row in the conversation pane pushes the progress strip
  // and the messages down; CSS needs to know it is there.
  document.documentElement.classList.toggle('tel-search-open', telState.searchIn === 'thread');
  // innerHTML resets scroll and focus, so both are carried across.
  var focus = telCaptureFocus();
  var listScroll = telCaptureScroll('telBody');
  var msgScroll = telCaptureScroll('telMsgs');
  var sb = document.getElementById('telSidebar');
  if (sb) {
    sb.innerHTML = telRenderSidebar();
    hydrateIcons(sb);
  }
  var pane = document.getElementById('telPane');
  if (pane) {
    pane.innerHTML = telRenderPane();
    hydrateIcons(pane);
  }
  telRestoreScroll('telBody', listScroll, false);
  telRestoreScroll('telMsgs', msgScroll, true);
  // A multi-line draft must not snap back to one line.
  var input = document.getElementById('telInput');
  if (input && telState.draft) telGrowInput(input);
  telRestoreFocus(focus);
  telPaintProgress();
  telPaintCountdown();
}

function telCaptureScroll(id) {
  var el = document.getElementById(id);
  if (!el) return null;
  return {
    top: el.scrollTop,
    // Stable measure for a message list, where history prepends.
    fromBottom: el.scrollHeight - el.scrollTop - el.clientHeight
  };
}

// telRestoreScroll puts a pane back where it was: anchored from the top for
// appended messages, from the bottom for prepended history.
function telRestoreScroll(id, saved, isThread) {
  var el = document.getElementById(id);
  if (!el) return;
  // A pending jump wins over every other anchoring rule: it is the one the
  // user asked for by tapping a quote or a link to a post.
  if (isThread && telState.jumpTo) {
    var want = telState.jumpTo;
    telState.jumpTo = 0;
    if (telScrollToMsg(want)) return;
  }
  // A freshly pinned "new messages" line is the next claim on the view, ahead
  // of opening at the bottom: landing on the newest post is exactly what hid
  // where the new ones started. Only ever once per line -- re-renders during
  // the sticky window must not keep yanking the reader back to it.
  if (isThread && telState.sepScroll && telState.sepScroll === telPeerKey(telState.peer)) {
    if (telScrollToSep()) {
      telState.sepScroll = '';
      telState.jumpToBottom = false;
      return;
    }
    // Not on screen yet: the messages are still arriving. Keep the intent and
    // fall through to the ordinary anchoring for this render.
  }
  if (isThread && telState.jumpToBottom) {
    telState.jumpToBottom = false;
    el.scrollTop = el.scrollHeight;
    return;
  }
  if (!saved) {
    // Nothing to restore: a conversation opens at its newest message.
    if (isThread) el.scrollTop = el.scrollHeight;
    return;
  }
  // Reading the newest message keeps following it as more arrive.
  if (isThread && saved.fromBottom < 40) {
    el.scrollTop = el.scrollHeight;
    return;
  }
  if (isThread && telState.prepended) {
    telState.prepended = false;
    el.scrollTop = el.scrollHeight - el.clientHeight - saved.fromBottom;
    return;
  }
  el.scrollTop = saved.top;
}

function telCaptureFocus() {
  var el = document.activeElement;
  if (!el || !el.id || !/^(INPUT|TEXTAREA)$/.test(el.tagName)) return null;
  return { id: el.id, start: el.selectionStart, end: el.selectionEnd };
}

function telRestoreFocus(f) {
  if (!f) return;
  var el = document.getElementById(f.id);
  if (!el) return;
  el.focus();
  try { el.setSelectionRange(f.start, f.end); } catch (e) { /* type has no range */ }
}

function telRenderSidebar() {
  var info = telState.info || {};
  var html = telRenderTopbar();
  // Outside the scrolling body: inside, it would push every row down.
  html += telRenderProgress();
  html += '<div class="tel-body" id="telBody">';
  if (info.shared) html += telRenderSharedWarning();
  // A link that is already being rebuilt is not news: the red shield says it,
  // and a banner on top of it only makes a passing blip look like a failure
  // the user has to act on.
  var retrying = !info.connected && !!info.resumeServer;
  if (telState.error && !retrying) {
    html += '<div class="tel-banner tel-banner-warn" dir="auto">' + telEsc(telState.error) + '</div>';
  }
  // Nothing has been heard from the backend yet (a fresh page load). Every
  // branch below reads a field that is simply absent until then, so deciding
  // now shows the WRONG screen: an unset hasCredentials asked for the api id
  // and hash of someone who had already entered them.
  // Being signed in is a property of the stored session, NOT of a live
  // connection. A drop must leave the user exactly where they were -- the
  // shield turns red and the reconnect runs underneath. Sending them back to
  // the config picker, or worse to the phone-number screen, threw away the
  // open chat and read as having been signed out.
  var signedIn = telState.authorized || info.hasSession;
  if (telState.info === null) html += telRenderLoading();
  else if (telState.showIntro) html += telRenderIntro();
  else if (!info.hasCredentials || telState.editCreds) html += telRenderCredentials(info);
  // The picker is only for having nowhere to go back to at all.
  else if (!info.connected && !info.resumeServer && !signedIn) html += telRenderPickServer(info);
  else if (!signedIn) html += telRenderLogin();
  else html += telRenderDialogs();
  html += '</div>';
  if (telReady() && !telState.showIntro) {
    html += '<button type="button" class="tel-fab" onclick="telNewChatSheet()" aria-label="' +
      telAttrQ(telT('tel_new_chat')) + '" title="' + telAttrQ(telT('tel_new_chat')) + '">' +
      icon('pencil') + '</button>';
  }
  return html;
}

// The topbar is present in every state, so the ⋮ menu always reachable.
function telRenderTopbar() {
  var info = telState.info || {};
  var html = '<div class="tel-topbar">';
  html += '<button class="tel-back" onclick="closeTel()" aria-label="' +
    telAttrQ(telT('tel_close')) + '">' + icon('arrowLeft') + '</button>';
  html += '<div class="tel-topbar-title"><span class="tel-title" dir="auto">' +
    telEsc(telT('tel_title')) + '</span>' +
    '<span class="tel-badge">' + telEsc(telT('tel_badge')) + '</span></div>';
  if (telReady()) html += telSearchBtnHTML('list') + telCountdownHTML();
  html += '<button class="tel-icon-btn tel-refresh-btn' + ((telState.busy || telState.bg) ? ' spinning' : '') + '"' +
    (telState.busy ? ' disabled' : '') + ' title="' + telAttrQ(telT('tel_refresh')) +
    '" aria-label="' + telAttrQ(telT('tel_refresh')) + '" onclick="telRefreshNow(\'list\')">' +
    icon('refresh') + '</button>';
  // ALWAYS present, never conditional. The re-check lives inside the sheet it
  // opens, so hiding the shield when nothing looks usable removes the one
  // control that could make something usable again. Red says "nothing is
  // carrying this right now"; that is the only thing a drop changes, and the
  // user stays on whatever they were doing while the reconnect runs behind it.
  // Tri-state, not a boolean. Before /api/tel/info answers we do not KNOW the
  // link is down, and painting red on that guess made every page reload look
  // like a disconnection that had not happened.
  var down = !!telState.info && !info.connected;
  html += '<button class="tel-icon-btn tel-shield' + (down ? ' tel-shield-down' : '') + '" title="' +
    telAttrQ(telT(down ? 'tel_reconnecting' : 'tel_change_server')) +
    '" aria-label="' + telAttrQ(telT(down ? 'tel_reconnecting' : 'tel_change_server')) +
    '" onclick="telServerSheet()">' + icon('shield') + '</button>';
  html += '<button class="tel-icon-btn" title="' + telAttrQ(telT('tel_menu')) +
    '" aria-label="' + telAttrQ(telT('tel_menu')) + '" onclick="telMenu()">' + icon('more') + '</button>';
  html += '</div>';
  return html;
}

// telServerChoices is the configs whose server carries the tunnel.
function telServerChoices() {
  var all = (telState.info || {}).servers || [];
  return all.filter(function (s) { return s.tunnel; });
}

// telServerSheet lists those configs, the way Telegram's shield lists proxies.
// Which config carries the session decides everything about it, so it gets a
// button of its own; the menu keeps its entry too.
function telServerSheet() {
  var info = telState.info || {};
  var items = '';
  // Everything known to carry the tunnel; if nothing is confirmed yet (a fresh
  // backend has not asked), fall back to every config rather than an empty
  // sheet, and let the per-row mark say which are unconfirmed.
  var choices = telServerChoices();
  if (!choices.length) choices = (info.servers || []);
  choices.forEach(function (s) {
    var on = s.key === info.server;
    items += '<button class="tel-sheet-item' + (on ? ' tel-sheet-on' : '') + '" onclick="telCloseSheet();telPickServer(' +
      telAttrQ(JSON.stringify(s.key)) + ')">' +
      icon(on ? 'success' : 'shield') + ' <span dir="auto">' +
      telEsc(s.name || s.domain || s.key) +
      // Stale means the capability bit has not been confirmed lately, not that
      // the server said no: still pickable, just marked.
      (s.verified ? '' : ' <span class="tel-sheet-note">' + telEsc(telT('tel_unconfirmed')) + '</span>') +
      '</span></button>';
  });
  items += telSheetItem('refresh', telT('tel_recheck'), 'telCloseSheet();telRecheck()');
  telSheet(items);
}
window.telServerSheet = telServerSheet;

// telPickServer switches to another config, or reconnects to this one when the
// connection has dropped.
async function telPickServer(key) {
  var info = telState.info || {};
  if (telState.busy) return;
  // A dropped connection clears info.server, so fall back to the config we
  // would resume. Without this, picking the config you are ALREADY on counted
  // as a switch and threw away the loaded chats.
  var current = info.server || info.resumeServer || '';
  if (key === current && info.connected) return;

  // Only a half-finished login is bound to the connection that started it:
  // Telegram ties phone_code_hash to it, so carrying one across is rejected as
  // an invalid code. The chats are NOT -- they belong to the Telegram account,
  // and the config is only the transport carrying it. Clearing them made a
  // switch look like being signed out, and cost a full reload on a link where
  // that is minutes.
  if (key !== current) {
    telState.codeHash = '';
    telState.codeVia = '';
    telState.needPassword = false;
  }
  await telConnect(key);
}
window.telPickServer = telPickServer;

// telCountdownHTML is the ring counting down to the next automatic check.
function telCountdownHTML() {
  return '<button class="tel-icon-btn tel-timer-btn" onclick="telToastNextPoll()" aria-label="' +
    telAttrQ(telT('tel_next_check').replace('{n}', '')) + '">' +
    '<span class="tel-timer-ring"><span class="tel-timer-num"></span></span></button>';
}

function telToastNextPoll() { showToast(telState._nextText || ''); }
window.telToastNextPoll = telToastNextPoll;

function telPaintCountdown() {
  var secs = Math.max(0, Math.ceil((telState.nextAt - Date.now()) / 1000));
  var text = telT('tel_next_check').replace('{n}', secs);
  telState._nextText = text;
  var total = telPollInterval() / 1000;
  var deg = Math.round(Math.max(0, Math.min(1, secs / total)) * 360);
  document.querySelectorAll('.tel-timer-btn').forEach(function (btn) {
    btn.title = text;
    var ring = btn.querySelector('.tel-timer-ring');
    if (ring) ring.style.background = 'conic-gradient(var(--accent) ' + deg + 'deg, var(--border) ' + deg + 'deg)';
    var num = btn.querySelector('.tel-timer-num');
    if (num) num.textContent = secs;
  });
}

// telRenderCodeVia says which way Telegram sent the code. Telegram usually
// delivers it INSIDE an app the user is already signed in to rather than by
// SMS, and without saying so a delivered code looks like a missing one.
function telRenderCodeVia() {
  if (!telState.codeVia) return '';
  var k = 'tel_code_via_' + telState.codeVia;
  var txt = telT(k);
  if (txt === k) return '';
  return '<div class="tel-hint" dir="auto">' + telEsc(txt) + '</div>';
}

// telResume rebuilds the connection on the config we already had. Reading the
// key here rather than baking it into the markup keeps it out of an onclick
// string, where a server key would have to be escaped correctly every time.
function telResume() {
  var key = (telState.info || {}).resumeServer;
  if (key) telConnect(key);
}

window.telResume = telResume;

// telRenderLoading holds the first paint until the backend has answered.
function telRenderLoading() {
  return '<div class="tel-step"><div class="tel-step-head" dir="auto">' +
    telEsc(telT('tel_loading')) + '</div></div>';
}

// telCheckedServers reports whether the backend has actually looked at the
// configs yet, as opposed to simply not knowing.
function telCheckedServers() {
  if (telAutoChecked) return true;
  var all = ((telState.info || {}).servers) || [];
  return all.some(function (s) { return s.verified; });
}

// telAutoCheckServers looks once, by itself, the first time the section opens
// with nothing confirmed. Finding out costs a handful of queries and is what
// the user would press anyway.
var telAutoChecked = false;
function telAutoCheckServers() {
  if (telAutoChecked || telState.busy) return;
  var info = telState.info || {};
  if (info.connected || info.connecting) return;
  if (!(info.servers || []).length) return;        // nothing to check
  if (telCheckedServers()) { telAutoChecked = true; return; }
  telAutoChecked = true;
  telRecheck();
}

// telRenderProgress is the strip reporting the running operation and its live
// query count.
function telRenderProgress() {
  // No id: rendered on both surfaces, updated by class.
  return '<div class="tel-prog-wrap">' +
    '<div class="tel-prog"><div class="tel-prog-fill"></div></div>' +
    // dir="auto", like every other line of text here: the strip sits inside
    // panes that are pinned to ltr for bubble sides, and without this the
    // Persian label inherited that and sat against the wrong edge.
    '<span class="tel-prog-label" dir="auto"></span></div>';
}

// telPhaseLabel names the step a connect is on. The first steps carry no
// message, so there is no total to count against and a bare number would say
// nothing about which part is slow.
function telPhaseLabel(phase) {
  if (!phase) return telT('tel_connecting');
  var txt = telT('tel_phase_' + phase);
  return txt === 'tel_phase_' + phase ? telT('tel_connecting') : txt;
}

// telPaintProgress updates the strip in place, never via a full render.
function telPaintProgress() {
  var info = telState.info || {};
  // Connecting counts: it runs on the backend, with no call in flight here.
  var connecting = !!info.connecting;
  var running = telState.busy || telState.bg || connecting;
  var label = '';
  if (telState.busy) label = telT(telState.busyLabel || 'tel_working');
  else if (telState.bg) label = telT(telState.bgLabel || 'tel_checking_new');
  else if (connecting) label = telPhaseLabel(info.phase);
  // Which conversation this is for. Switching channels mid-fetch is otherwise
  // invisible: every step reads the same, so there is no way to tell that the
  // tap landed and the old fetch was dropped.
  // Which round trip of the action this is. A send is two or three; opening a
  // conversation is two; walking the conversation list is one per page.
  // Without it each bar reads as a fresh, unexplained wait.
  if (running && telState.stepTotal > 1) {
    label += ' · ' + telT('tel_step')
      .replace('{n}', Math.min(telState.stepNo, telState.stepTotal))
      .replace('{total}', telState.stepTotal);
    // No retry count here. The cell counters are PER MTPROTO MESSAGE, not per
    // step: one step sends several messages (the request, acks, gotd's own
    // traffic) and each restarts the count. Reading that as "the step started
    // over" labelled ordinary progress as a failed attempt. A real retry is
    // something only the backend knows -- it is the one that rebuilds the
    // connection and re-runs the call -- so it has to be reported from there,
    // not guessed from a counter that legitimately restarts.
  }
  if (running && telState.opTitle) {
    // Telegram's string, dropped into a Persian sentence. Two things are
    // needed, not one: telSafeText strips the bidi overrides a channel could
    // put in its own name to scramble the rest of the line, and the isolate
    // keeps a Latin-script name from dragging the punctuation around it to
    // the wrong side. U+2068 is first-strong, so a name in either script
    // lands the right way round inside an RTL label.
    label += ' — ⁨' + telSafeText(telState.opTitle) + '⁩';
  }
  if (connecting && telState.attempt > 1) {
    label += ' — ' + telT('tel_attempt') + ' ' + telState.attempt;
  }
  // The step's own progress, never a session-wide total -- that only grows and
  // says nothing about the wait in front of the user. Each half of a round
  // trip gets the number meaningful for it, and nothing outside its half:
  //
  //   going out  "n of m" -- cells the relay has ACKNOWLEDGED (the upstream
  //              offset, which only moves on an ack), so a cell re-sent five
  //              times still counts once. A known total, so it has an end.
  //   coming back  bytes still held for us. The relay reports what is LEFT,
  //              not what there was, so a count would climb without end.
  //
  // The split matters because the upload's total is NOT cleared when its last
  // cell is acknowledged: shown past that point the strip sits on a finished
  // "10 of 10" for the whole return trip, which over DNS is most of the wait.
  var sent = info.sentCells || 0, total = info.totalCells || 0;
  var uploading = running && total > 0 && sent < total;
  var pct = uploading ? Math.min(100, Math.round(sent * 100 / total)) : 0;
  if (uploading) {
    label += ' · ' + telT('tel_of_queries').replace('{n}', sent).replace('{total}', total);
  } else if (running && info.downLeft > 0) {
    label += ' · ' + telT('tel_receiving').replace('{n}', telFmtBytes(info.downLeft));
  } else if (running) {
    label += ' · ' + telT('tel_awaiting_reply');
  }
  // Repainted here rather than left to a full render: during a long fetch only
  // this cheap paint runs, so a shield drawn while the link was down stayed red
  // after it came back.
  // The refresh button's spinner, repainted here for the same reason as the
  // shield: a background pass never goes through a full render, so the class
  // was set by whatever render happened to be running and then never cleared.
  // The result was a button spinning for ever with no progress bar under it.
  document.querySelectorAll('.tel-icon-btn.spinning, .tel-refresh-btn').forEach(function (b) {
    if (b.querySelector('.icon')) b.classList.toggle('spinning', !!(telState.busy || telState.bg));
  });
  document.querySelectorAll('.tel-shield').forEach(function (b) {
    b.classList.toggle('tel-shield-down', !info.connected);
    var t = telT(info.connected ? 'tel_change_server' : 'tel_reconnecting');
    b.title = t;
    b.setAttribute('aria-label', t);
  });
  telProgShow(!!running);
  document.querySelectorAll('.tel-prog-wrap').forEach(function (w) {
    w.classList.toggle('known', pct > 0);
    var fill = w.querySelector('.tel-prog-fill');
    if (fill) fill.style.width = pct > 0 ? pct + '%' : '';
    var lb = w.querySelector('.tel-prog-label');
    // Blank means "between operations", and the strip is still up then -- a
    // bar with no text under it reads as broken, so keep the last label.
    if (lb && label) lb.textContent = label;
  });
}

/* One user action is usually several round trips -- the messages, then views,
   then reactions -- and each is its own operation with its own bar. On a
   degraded link they are tens of seconds each, so the strip looks continuous.
   On a working one they are a couple of hundred milliseconds, and a strip
   bound to each one strobes.
   So visibility is held: it stays up across the gap between operations, and
   once up it stays long enough to be read. */
var TEL_PROG_LINGER_MS = 600;
var TEL_PROG_MIN_MS = 450;
var telProgOn = false;
var telProgSince = 0;
var telProgHideAt = null;

// telProgShow owns the strip's visibility. telProgOn is the truth, re-applied
// on every paint so a full render (which rebuilds the strip without the class)
// does not drop it.
function telProgShow(on) {
  if (on) {
    if (telProgHideAt) { clearTimeout(telProgHideAt); telProgHideAt = null; }
    if (!telProgOn) { telProgOn = true; telProgSince = Date.now(); }
  } else if (telProgOn && !telProgHideAt) {
    var shown = Date.now() - telProgSince;
    telProgHideAt = setTimeout(function () {
      telProgHideAt = null;
      telProgOn = false;
      telProgApply();
    }, Math.max(TEL_PROG_LINGER_MS, TEL_PROG_MIN_MS - shown));
  }
  telProgApply();
}

function telProgApply() {
  document.documentElement.classList.toggle('tel-prog-on', telProgOn);
  document.querySelectorAll('.tel-prog-wrap').forEach(function (w) {
    w.classList.toggle('on', telProgOn);
  });
}

// Shared backend: the session is stored on the machine running the client, so
// everyone pointing a browser here gets the same account.
function telRenderSharedWarning() {
  return '<div class="tel-banner tel-banner-danger" dir="auto">' +
    '<b>' + telEsc(telT('tel_shared_title')) + '</b><br>' +
    telEsc(telT('tel_shared_body')) + '</div>';
}

// Telegram identifies the app by api id/hash; this build ships none, so they
// are asked for up front and prefilled with what is stored.
function telRenderCredentials(info) {
  var html = '<div class="tel-step">' +
    '<div class="tel-step-head" dir="auto">' + telEsc(telT('tel_creds_intro')) + '</div>' +
    '<label class="tel-label" for="telApiId" dir="auto">' + telEsc(telT('tel_api_id')) + '</label>' +
    '<input id="telApiId" class="tel-input" type="text" inputmode="numeric" dir="ltr" value="' +
    telAttrQ(info.apiId || '') + '">' +
    '<label class="tel-label" for="telApiHash" dir="auto">' + telEsc(telT('tel_api_hash')) + '</label>' +
    '<input id="telApiHash" class="tel-input" type="text" dir="ltr" autocomplete="off" value="' +
    telAttrQ(info.apiHash || '') + '">' +
    '<div class="tel-banner tel-banner-warn" dir="auto">' + telEsc(telT('tel_creds_warn')) + '</div>' +
    // Only where there is something to lose: on first entry there is no
    // session, and warning about signing out of an account nobody is in yet
    // reads as a threat for no reason.
    (info.hasSession ? '<div class="tel-banner tel-banner-warn" dir="auto">' +
      telEsc(telT('tel_creds_signout_confirm')) + '</div>' : '') +
    '<button class="tel-btn tel-btn-primary" onclick="telSaveCredentials()"' +
    (telState.busy ? ' disabled' : '') + '>' + telEsc(telT('tel_save')) + '</button>';
  if (telState.editCreds) {
    html += '<button class="tel-btn" onclick="telCancelCreds()">' + telEsc(telT('tel_cancel')) + '</button>';
  }
  html += '<div class="tel-hint" dir="auto">' + telEsc(telT('tel_creds_hint')) + '</div></div>';
  return html;
}

// Config picker: only servers that advertise the tunnel in signed metadata.
function telRenderPickServer(info) {
  var servers = (info.servers || []).filter(function (s) { return s.tunnel; });
  var connecting = telState.busy || !!info.connecting;
  var html = '<div class="tel-step">';
  if (!servers.length) {
    // Not knowing is not knowing there is none. Which configs carry the
    // tunnel lives in backend memory, so a restart wipes it and every config
    // reads as incapable -- condemning them before anyone has looked.
    if (!telCheckedServers()) {
      html += '<div class="tel-banner" dir="auto">' + telEsc(telT('tel_checking_servers')) + '</div></div>';
      return html;
    }
    html += '<div class="tel-banner" dir="auto">' + telEsc(telT('tel_no_server')) + '</div>';
    if ((info.servers || []).length) {
      html += '<div class="tel-hint" dir="auto">' + telEsc(telT('tel_no_server_hint')) + '</div>';
    }
    html += '<button class="tel-btn" onclick="telRecheck()"' + (connecting ? ' disabled' : '') + '>' +
      telEsc(telT(telState.busy ? 'tel_rechecking' : 'tel_recheck')) + '</button></div>';
    return html;
  }
  // While connecting, asking the user to pick a config contradicts the
  // progress strip right above it.
  if (connecting) {
    html += '<div class="tel-step-head" dir="auto">' +
      telEsc(telT('tel_connecting_to').replace('{name}', telServerName(telState.server))) +
      '</div>';
  } else {
    // "To start…" is wrong when only switching servers.
    html += '<div class="tel-step-head" dir="auto">' +
      telEsc(telT(info.hasSession ? 'tel_pick_config_again' : 'tel_pick_config')) + '</div>';
  }
  html += '<div class="tel-srv-list">';
  servers.forEach(function (s) {
    // Only the config being connected to is worth showing mid-attempt.
    if (connecting && s.key !== telState.server) return;
    html += '<button type="button" class="tel-srv" onclick="telConnect(\'' + telAttrQ(s.key) + '\')"' +
      (connecting ? ' disabled' : '') + '>' +
      '<span class="tel-srv-shield">' + icon('shield') + '</span>' +
      '<span class="tel-srv-main"><span class="tel-srv-name" dir="auto">' +
      telEsc(s.name || s.key) + '</span>' +
      '<span class="tel-srv-dom" dir="ltr">' + telEsc(s.domain) + '</span></span>' +
      '</button>';
  });
  html += '</div>';
  if (connecting) {
    html += '<div class="tel-hint" dir="auto">' + telEsc(telT('tel_connect_slow')) + '</div>' +
      '<button class="tel-btn" onclick="telCancelConnect()"' +
      (telState.cancelling ? ' disabled' : '') + '>' +
      telEsc(telT(telState.cancelling ? 'tel_cancelling' : 'tel_cancel')) + '</button>';
  } else {
    html += '<button class="tel-btn" onclick="telRecheck()">' + telEsc(telT('tel_recheck')) + '</button>';
  }
  html += '</div>';
  return html;
}

function telRenderLogin() {
  var html = '<div class="tel-step">';
  if (telState.needPassword) {
    html += '<div class="tel-step-head" dir="auto">' + telEsc(telT('tel_password_intro')) + '</div>';
    // Telegram stores the user's own reminder for exactly this moment.
    if (telState.passwordHint) {
      html += '<div class="tel-hint" dir="auto">' +
        telEsc(telT('tel_2fa_hint').replace('{hint}', telState.passwordHint)) + '</div>';
    }
    html += '<label class="tel-label" for="telPassword" dir="auto">' + telEsc(telT('tel_password')) + '</label>' +
      '<input id="telPassword" class="tel-input" type="password" autocomplete="off" dir="ltr">' +
      '<button class="tel-btn tel-btn-primary" onclick="telSignInPassword()"' +
      (telState.busy ? ' disabled' : '') + '>' + telEsc(telT('tel_continue')) + '</button></div>';
    return html;
  }
  if (!telState.codeHash) {
    var typed = telState.phone || '';
    if (typed.indexOf(telDialCode()) === 0) typed = typed.slice(telDialCode().length);
    var sel = '<select id="telDial" class="tel-input tel-dial" dir="ltr" onchange="telOnDialChange(this.value)">';
    TEL_DIAL_CODES.forEach(function (c) {
      sel += '<option value="' + telAttrQ(c.code) + '"' + (c.code === telDialCode() ? ' selected' : '') +
        '>' + telEsc(telT('tel_country_' + c.id)) + ' (' + telEsc(c.code) + ')</option>';
    });
    sel += '</select>';
    html += '<div class="tel-step-head" dir="auto">' + telEsc(telT('tel_login_intro')) + '</div>' +
      '<label class="tel-label" for="telPhone" dir="auto">' + telEsc(telT('tel_phone')) + '</label>' +
      '<div class="tel-phone-row" dir="ltr">' + sel +
      '<input id="telPhone" class="tel-input" type="tel" inputmode="tel" dir="ltr" value="' +
      telAttrQ(typed) + '"></div>' +
      '<button class="tel-btn tel-btn-primary" onclick="telSendCode()"' +
      (telState.busy ? ' disabled' : '') + '>' + telEsc(telT('tel_send_code')) + '</button>' +
      '<div class="tel-hint" dir="auto">' + telEsc(telT('tel_login_hint')) + '</div></div>';
    return html;
  }
  html += '<div class="tel-step-head" dir="auto">' +
    telEsc(telT('tel_code_sent').replace('{phone}', telLTR(telState.phone))) + '</div>' +
    telRenderCodeVia() +
    '<label class="tel-label" for="telCode" dir="auto">' + telEsc(telT('tel_code')) + '</label>' +
    '<input id="telCode" class="tel-input" type="text" inputmode="numeric" autocomplete="one-time-code" dir="ltr">' +
    '<button class="tel-btn tel-btn-primary" onclick="telSignIn()"' +
    (telState.busy ? ' disabled' : '') + '>' + telEsc(telT('tel_continue')) + '</button>' +
    '<button class="tel-btn" onclick="telResendCode()"' + (telState.busy ? ' disabled' : '') + '>' +
    telEsc(telT('tel_resend')) + '</button>' +
    '<div class="tel-hint" dir="auto">' + telEsc(telT('tel_code_hint')) + '</div></div>';
  return html;
}

// ----- the conversation list -----

function telRenderDialogs() {
  var html = telSearchBarHTML('list', 'tel_search_chats') + telRenderTabs();
  var rows = telTabRows(telState.tab);
  if (!rows.length) {
    if (telState.busy || telState.bg) {
      html += '<div class="tel-empty" dir="auto">' + telEsc(telT('tel_loading')) + '</div>';
    } else if (telSearchFor('list')) {
      html += '<div class="tel-empty" dir="auto">' + telEsc(telT('tel_search_none')) + '</div>';
    } else if (telState.loadedOnce) {
      html += '<div class="tel-empty" dir="auto">' + telEsc(telT('tel_no_dialogs_tab')) + '</div>';
    } else {
      // Never a dead "loading…": the first fetch may have failed.
      html += '<div class="tel-empty" dir="auto">' + telEsc(telT('tel_no_dialogs')) + '</div>' +
        '<div class="tel-more"><button class="tel-btn tel-btn-primary" onclick="telLoadDialogs(true)">' +
        telEsc(telT('tel_reload_chats')) + '</button></div>';
    }
    return html;
  }
  html += '<div class="tel-list">';
  rows.forEach(function (d) {
    var key = telPeerKey(d.peer);
    var active = telPeerKey(telState.peer) === key;
    html += '<div class="tel-row' + (active ? ' active' : '') +
      '" onclick="telOpenPeer(\'' + telAttrQ(key) + '\')">' +
      telAvatar(d.peer.title, 44, d.peer.thumb) +
      '<span class="tel-row-main">' +
      '<span class="tel-row-top">' +
      (d.pinned ? '<span class="tel-pin">' + icon('pinned') + '</span>' : '') +
      '<span class="tel-row-title" dir="auto">' + telFlag(telEsc(d.peer.title || d.peer.username || key)) + '</span>' +
      // Muted shows on the ROW, always -- not only as the unread badge's
      // colour. A silenced channel with nothing unread has no badge to tint,
      // so it read exactly like one that is still allowed to interrupt.
      (d.muted ? '<span class="tel-row-muted" title="' + telAttrQ(telT('tel_muted')) +
        '" aria-label="' + telAttrQ(telT('tel_muted')) + '">' + icon('bellOff') + '</span>' : '') +
      '</span>' +
      '<span class="tel-row-prev" dir="auto">' + telFlag(telEsc(d.preview || '')) + '</span>' +
      '</span>' +
      '<span class="tel-row-side">' +
      '<span class="tel-row-when">' + telEsc(telFmtWhen(d.when)) + '</span>' +
      (d.unread ? '<span class="tel-unread' + (d.muted ? ' muted' : '') + '">' +
        telEsc(d.unread > 99 ? '99+' : d.unread) + '</span>' : '') +
      '</span>' +
      '<button class="tel-icon-btn tel-row-menu" aria-label="' + telAttrQ(telT('tel_menu')) +
      '" onclick="event.stopPropagation();telRowMenu(\'' + telAttrQ(key) + '\')">' + icon('more') + '</button>' +
      '</div>';
  });
  html += '</div>';
  // Paging runs in the background; the button is for "now".
  if (telState.more) {
    html += '<div class="tel-more">' +
      '<button class="tel-btn" onclick="telLoadMoreNow()"' +
      (telState.morePending ? ' disabled' : '') + '>' +
      telEsc(telT(telState.morePending ? 'tel_loading_chats' : 'tel_load_more')) + '</button>' +
      '<span class="tel-hint" dir="auto">' + telEsc(telLoadedLine()) + '</span></div>';
  } else if (telState.dialogs.length) {
    html += '<div class="tel-more"><span class="tel-hint" dir="auto">' +
      telEsc(telLoadedLine()) + '</span></div>';
  }
  return html;
}

function telLoadedLine() {
  var n = telState.dialogs.length;
  // "of N" only while there is actually more to fetch. Telegram's count
  // includes conversations this client cannot show -- one whose entity never
  // arrives is dropped -- so the two numbers can differ by a few and never
  // meet. Saying "197 of 198" when 197 is everything there is reads as a
  // failure, and sent the user pressing a button that had nothing to get.
  if (telState.more && telState.total > n) {
    return telT('tel_loaded_of').replace('{n}', n).replace('{total}', telState.total);
  }
  return telT('tel_loaded_n').replace('{n}', n);
}

function telRenderTabs() {
  var html = '<div class="tel-tabs" data-no-swipe>';
  TEL_TABS.forEach(function (tb) {
    var n = telTabUnread(tb.key);
    var count = telTabRows(tb.key).length;
    html += '<button type="button" class="tel-tab' + (telState.tab === tb.key ? ' active' : '') +
      '" onclick="telSetTab(\'' + tb.key + '\')">' +
      telEsc(telT('tel_tab_' + tb.key)) +
      (count ? '<span class="tel-tab-n">' + telEsc(count) + '</span>' : '') +
      (n ? '<span class="tel-tab-dot" aria-hidden="true"></span>' : '') +
      '</button>';
  });
  html += '</div>';
  return html;
}

// ----- the open conversation -----

// telVisibleMessages drops service messages (cached for the cursor, no bubble)
// and applies the search.
function telVisibleMessages(th) {
  var q = telSearchFor('thread');
  return th.msgs.filter(function (m) {
    if (m.service) return false;
    if (!q) return true;
    return (m.text || '').toLowerCase().indexOf(q) >= 0 ||
      (m.from || '').toLowerCase().indexOf(q) >= 0;
  });
}

// telLinkClick intercepts a tap on a link inside a message.
//
// Going straight out to the browser is wrong twice over: it leaves the app on
// a link the user may only have wanted to read, and a Telegram link is
// something this section can open ITSELF. So it asks first -- copy, open
// outside, or open the channel here -- which is what the feed and the Mirror
// already do, through the same shared sheet.
function telLinkClick(e) {
  var a = e.target && e.target.closest ? e.target.closest('.tel-msg-text a, .tel-reply a') : null;
  if (!a) return;
  var url = a.getAttribute('href') || '';
  if (!url) return;
  e.preventDefault();
  e.stopPropagation();

  var extra = null;
  var tg = (typeof parseTgLink === 'function') ? parseTgLink(url) : null;
  if (tg && tg.user) {
    // A link to a post in the conversation already open is just a jump. Going
    // through resolve would spend a round trip to arrive where we are.
    var here = telState.peer && telState.peer.username &&
      telState.peer.username.toLowerCase() === String(tg.user).toLowerCase();
    if (here && tg.msgId) {
      telJumpToMsg(tg.msgId);
      return;
    }
    extra = {
      label: telT('tel_open_channel').replace('{u}', tg.user),
      // Opens it, and ONLY opens it. Reading a channel is not joining one,
      // and a tap on a link must never sign the user up to anything. Joining
      // is offered separately, once the user can see what they would join.
      action: function () { telOpenByUsername(tg.user, tg.msgId); },
    };
  }
  if (typeof showLinkSheet === 'function') showLinkSheet(url, extra);
  else if (typeof openExternal === 'function') openExternal(url);
}
window.telLinkClick = telLinkClick;

// telOpenByUsername resolves @name and opens that conversation here.
async function telOpenByUsername(user, msgId) {
  await telOp('tel_working', async function () {
    var r = await telCall('/api/tel/resolve', { username: user }, 'tel_err_username');
    if (!r || !r.peer) return;
    var key = telPeerKey(r.peer);
    if (!telDialogFor(key)) {
      // A row to come back to. Not a subscription: the list is ours, and
      // Telegram is not told anything by this.
      // visiting: opened by name or through a link, not subscribed to. The
      // join offer hangs off this, so it only appears where joining means
      // something.
      telState.dialogs.unshift({ peer: r.peer, unread: 0, topMsg: 0, preview: '', visiting: true });
    }
    var th = telEnterThread(key, r.peer);
    var h = await telCall(telHistoryQuery(r.peer, { limit: TEL_HISTORY_PAGE }));
    th.fetched = true;
    if (h) {
      telMergeMessages(th, h.messages || [], true);
      telTouchPreview(key, th);
    }
    if (msgId) telState.jumpTo = msgId;
  });
  telRender();
}
window.telOpenByUsername = telOpenByUsername;

// telReplyHTML draws what a message is answering, the way every Telegram
// client does: a quoted strip above it.
//
// The data was already arriving and simply never shown, so channel posts that
// answer another post looked like they stood alone. Telegram sends the quoted
// text with the message when it has one; otherwise the original is looked up
// among the messages already loaded, and failing both the strip still says
// "this is a reply" rather than silently dropping the fact.
function telReplyHTML(m, th) {
  if (!m.replyTo) return '';
  var text = m.replyQuote || '';
  var who = '';
  if (!text && th) {
    for (var i = 0; i < th.msgs.length; i++) {
      if (th.msgs[i].id === m.replyTo) {
        text = th.msgs[i].text || telMediaLabel(th.msgs[i]);
        who = th.msgs[i].from || '';
        break;
      }
    }
  }
  var body = text ? telFlag(telEsc(telSafeText(text))) : telEsc(telT('tel_reply_to'));
  // Clickable: a quote you cannot follow is half a reply.
  return '<span class="tel-reply" role="button" tabindex="0" dir="auto"' +
    ' onclick="event.stopPropagation();telJumpToMsg(' + (m.replyTo | 0) + ')"' +
    ' title="' + telAttrQ(telT('tel_reply_to')) + '">' +
    (who ? '<span class="tel-reply-who">' + telEsc(telSafeText(who)) + '</span>' : '') +
    '<span class="tel-reply-text">' + body + '</span></span>';
}

// telMediaLabel names an attachment in words, for a reply strip quoting a
// message that has no text of its own.
function telMediaLabel(m) {
  if (!m || !m.media) return '';
  var k = telT('tel_media_' + m.media);
  return k === 'tel_media_' + m.media ? telT('tel_media_other') : k;
}

// telStatsHTML is a channel post's views and reactions, the way the feed shows
// them: a count with an eye, then each emoji with its own count.
function telStatsHTML(m) {
  var html = '';
  if (m.views) {
    html += '<span class="tel-views">' + icon('views') + telEsc(telFmtCount(m.views)) + '</span>';
  }
  return html;
}

// telReactionsHTML is a row of its own, under the message and above the time.
//
// Not part of the meta row: beside the timestamp a reaction on a short message
// like "hi" ended up alongside the text instead of under it, which is not
// where any Telegram client puts it.
function telReactionsHTML(m) {
  var html = '';
  if (m.reactions && m.reactions.length) {
    html += '<div class="tel-reacts">';
    m.reactions.forEach(function (r) {
      // A button, not a label: tapping sets this reaction, and tapping the one
      // already chosen takes it back -- the emoji is passed through telAttrQ
      // because it is Telegram's string, not ours.
      html += '<button type="button" class="tel-react' + (r.chosen ? ' chosen' : '') +
        '" onclick="event.stopPropagation();telReact(' + (m.id | 0) + ',\'' +
        telAttrQ(r.emoji) + '\')">' + telEsc(telSafeText(r.emoji)) +
        '<span class="tel-react-n">' + telEsc(telFmtCount(r.count)) + '</span></button>';
    });
    html += '</div>';
  }
  return html;
}

// telReact sets or clears this account's reaction on one message.
//
// Drawn before it is sent, like the message ticks: a reaction that waits half
// a minute for a DNS round trip before showing anything feels broken. Telegram
// then answers with the real counts and those replace the guess -- including
// when it refuses, so a reaction the channel does not allow springs back
// instead of lingering as a count that is not there.
async function telReact(msgID, emoji) {
  var peer = telState.peer;
  if (!peer) return;
  var th = telThread(telPeerKey(peer));
  var m = null;
  for (var i = 0; i < th.msgs.length; i++) {
    if (th.msgs[i].id === msgID) { m = th.msgs[i]; break; }
  }
  if (!m) return;
  var before = (m.reactions || []).map(function (r) { return { emoji: r.emoji, count: r.count, chosen: r.chosen }; });
  var mine = before.filter(function (r) { return r.chosen; })[0];
  var want = (mine && mine.emoji === emoji) ? '' : emoji; // tapping your own takes it back
  m.reactions = telGuessReactions(before, emoji, want !== '');
  telRender();

  var r = await telCall('/api/tel/react', { peer: peer, msgId: msgID, emoji: want }, 'tel_err_generic');
  if (!r) {
    m.reactions = before; // refused, or the link went: undo the guess
    telRender();
    return;
  }
  m.reactions = r.reactions || [];
  telRender();
}
window.telReact = telReact;

// telGuessReactions is the optimistic count: one off the old choice, one onto
// the new, and a reaction that falls to zero disappears.
function telGuessReactions(list, emoji, adding) {
  var out = [];
  var found = false;
  list.forEach(function (r) {
    var c = r.count, chosen = false;
    if (r.chosen) c -= 1;              // whatever was picked before is released
    if (r.emoji === emoji) {
      found = true;
      if (adding) { c += 1; chosen = true; }
    }
    if (c > 0) out.push({ emoji: r.emoji, count: c, chosen: chosen });
  });
  if (adding && !found) out.push({ emoji: emoji, count: 1, chosen: true });
  return out;
}

// telFmtCount shortens the big numbers a channel carries.
function telFmtCount(n) {
  n = Number(n) || 0;
  if (n >= 1000000) return (n / 1000000).toFixed(n % 1000000 === 0 ? 0 : 1) + 'M';
  if (n >= 1000) return (n / 1000).toFixed(n % 1000 === 0 ? 0 : 1) + 'K';
  return String(n);
}

// telMediaHTML names an attachment: kind, file name and size. Nothing is
// downloaded, so this line is all the user gets.
function telMediaHTML(m) {
  if (!m.media) return '';
  var label = telT('tel_media_' + m.media);
  if (label === 'tel_media_' + m.media) label = telT('tel_media_other');
  if (m.mediaName) label += ': ' + telSafeText(m.mediaName);
  if (m.mediaSize) label += ' · ' + telFmtBytes(m.mediaSize);
  return '<span class="tel-media-chip">' + icon(TEL_MEDIA_ICON[m.media] || 'paperclip') +
    '<span dir="auto">' + telEsc(label) + '</span></span>';
}

var TEL_MEDIA_ICON = {
  photo: 'views', video: 'play', videonote: 'play', voice: 'voice', audio: 'music',
  sticker: 'heart', gif: 'play', file: 'document', geo: 'pin', contact: 'messenger',
  poll: 'stats', game: 'play', invoice: 'bank', dice: 'heart', story: 'views'
};

// telLTR isolates a left-to-right run inside right-to-left text. A phone
// number is digits plus a leading '+', and '+' is a neutral character: at the
// edge of an RTL run the bidi algorithm gives it the surrounding direction and
// draws it on the WRONG side of the number. The isolate pins the whole run.
function telLTR(s) {
  return '\u2066' + s + '\u2069';   // LRI ... PDI
}

// telFmtBytes formats a size with a TRANSLATED unit. A Latin unit inside
// Persian text makes the bidi algorithm reorder the line, so the number and its
// unit end up on opposite sides of whatever follows.
function telFmtBytes(n) {
  var units = ['tel_unit_b', 'tel_unit_kb', 'tel_unit_mb', 'tel_unit_gb'];
  var i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return (i === 0 ? n : n.toFixed(1)) + ' ' + telT(units[i]);
}

// telButtonsHTML draws a bot's inline keyboard: URL buttons open, the rest are
// shown flat (pressing them needs a call back to the bot).
// telSafeText strips the characters that reorder what is drawn.
//
// U+202A..U+202E and U+2066..U+2069 override direction for everything after
// them, which is how a file called "...cod.exe" is displayed as "...exe.doc".
// Official clients neutralise them for the same reason. Applied to values that
// ARRIVE from Telegram -- never to our own strings, which use the isolates
// deliberately (see telLTR).
// telFlag swaps the Iranian flag for the lion and sun, exactly as the feed,
// the mirror and chat already do. Applied to text that is ALREADY escaped,
// because it inserts markup: running it on raw text would let a message
// containing the flag smuggle the <img> past the escaping.
function telFlag(html) {
  return String(html).replace(/\uD83C\uDDEE\uD83C\uDDF7/g,
    '<img src="/static/lion-sun.svg" alt="\u{1F981}\u2600\uFE0F" style="height:1.1em;vertical-align:middle">');
}

function telSafeText(s) {
  return String(s == null ? '' : s).replace(/[\u202A-\u202E\u2066-\u2069]/g, '');
}

// telSafeURL returns a button's URL only when its scheme can do nothing but
// navigate.
//
// Whoever sent the message chose this value. Escaping it keeps it inside the
// attribute but says nothing about what it DOES: "javascript:..." in an href
// runs in our origin the moment the user taps a button a stranger sent them,
// which is a message turning into code execution. Only http, https and
// Telegram's own tg: links are allowed through; anything else is shown as an
// inert button, the same way a tap-to-act button is.
function telSafeURL(u) {
  var raw = String(u == null ? '' : u).trim();
  // The parser drops control characters BEFORE reading the scheme, so
  // "java\tscript:" is still javascript:. Test a stripped copy, return the
  // original.
  var probe = raw.replace(/[\u0000-\u0020]/g, '').toLowerCase();
  return /^(https?|tg):/.test(probe) ? raw : '';
}

function telButtonsHTML(m) {
  if (!m.buttons || !m.buttons.length) return '';
  var html = '<span class="tel-btns">';
  m.buttons.forEach(function (b) {
    var href = telSafeURL(b.url);
    if (href) {
      // The real target is the title: a label reading "google.com" on a
      // button that goes elsewhere is the oldest trick there is.
      html += '<a class="tel-kbd tel-kbd-url" href="' + telAttrQ(href) +
        '" target="_blank" rel="noopener noreferrer" dir="auto" title="' +
        telAttrQ(href) + '">' + telEsc(telSafeText(b.text)) + '</a>';
    } else {
      html += '<span class="tel-kbd" dir="auto" title="' +
        telAttrQ(telT('tel_button_unsupported')) + '">' +
        telEsc(telSafeText(b.text)) + '</span>';
    }
  });
  return html + '</span>';
}

// telTickHTML is a sent message's state: waiting, failed, sent or read.
function telTickHTML(m, readOutbox) {
  if (!m.out) return '';
  if (m.failed) return '<span class="tel-tick tel-tick-failed">' + icon('fail') + '</span>';
  if (m.pending) return '<span class="tel-tick">' + icon('clock') + '</span>';
  if (readOutbox && m.id <= readOutbox) {
    return '<span class="tel-tick tel-tick-read">' + icon('tickDouble') + '</span>';
  }
  return '<span class="tel-tick">' + icon('tickSingle') + '</span>';
}

function telRenderPane() {
  if (!telState.peer) {
    if (!telReady()) return '';
    return '<div class="tel-pane-empty" dir="auto">' + telEsc(telT('tel_pick_conversation')) + '</div>';
  }
  var peer = telState.peer;
  var th = telThread(telPeerKey(peer));
  var html = '<div class="tel-topbar tel-thread-bar">' +
    '<button class="tel-back" onclick="telBackToList()" aria-label="' +
    telAttrQ(telT('tel_back')) + '">' + icon('arrowLeft') + '</button>' +
    '<div class="tel-topbar-title">' + telAvatar(peer.title, 34, peer.thumb) +
    '<span class="tel-thread-meta"><span class="tel-title" dir="auto">' +
    telFlag(telEsc(peer.title || peer.username || '')) + '</span>' +
    '<span class="tel-thread-sub" dir="auto">' + telEsc(telT('tel_kind_' + peer.kind)) +
    (peer.username ? ' · @' + telEsc(peer.username) : '') + '</span></span></div>' +
    telSearchBtnHTML('thread') + telCountdownHTML() +
    '<button class="tel-icon-btn' + ((telState.busy || telState.bg) ? ' spinning' : '') + '"' +
    (telState.busy ? ' disabled' : '') + ' title="' + telAttrQ(telT('tel_refresh')) +
    '" aria-label="' + telAttrQ(telT('tel_refresh')) + '" onclick="telRefreshNow(\'thread\')">' +
    icon('refresh') + '</button>' +
    '<button class="tel-icon-btn" title="' + telAttrQ(telT('tel_menu')) +
    '" aria-label="' + telAttrQ(telT('tel_menu')) + '" onclick="telThreadMenu()">' +
    icon('more') + '</button></div>';
  html += telRenderProgress();
  html += telSearchBarHTML('thread', 'tel_search_messages');

  // One listener on the container, not one per link: the list is rebuilt on
  // every render and per-link handlers would be re-bound each time.
  html += '<div class="tel-msgs" id="telMsgs" onclick="telLinkClick(event)"' +
    ' onscroll="telStatsOnScroll()">';
  if (peer.kind === 'forum') {
    html += '<div class="tel-banner" dir="auto">' + telEsc(telT('tel_forum_note')) + '</div>';
  }
  if (th.more && th.msgs.length && !telSearchFor('thread')) {
    html += '<div class="tel-older"><button class="tel-btn" onclick="telThreadFetchOlder()"' +
      (telState.busy ? ' disabled' : '') + '>' + telEsc(telT('tel_load_older')) + '</button></div>';
  }
  if (!th.msgs.length) {
    // Three different things, not one "no messages": still loading, loaded
    // and genuinely empty, and never loaded because the request failed --
    // without the third, a dropped connection reports a channel full of posts
    // as empty.
    var state = 'tel_loading';
    if (th.fetched && !telState.busy) state = 'tel_no_messages';
    else if (!th.fetched && !telState.busy && telState.error) state = 'tel_thread_unavailable';
    html += '<div class="tel-empty" dir="auto">' + telEsc(telT(state)) + '</div>';
  }
  var lastDate = '';
  var lastFrom = '';
  // Everything the other side has read carries a second tick.
  var d = telDialogFor(telPeerKey(peer));
  var readOutbox = (d && d.readOutbox) || 0;
  // Only a group needs sender names; a private conversation has exactly two.
  var showFrom = peer.kind === 'group' || peer.kind === 'forum' || peer.kind === 'channel';
  var shown = telVisibleMessages(th);
  if (!shown.length && th.msgs.length) {
    html += '<div class="tel-empty" dir="auto">' + telEsc(telT('tel_search_none')) + '</div>';
  }
  // Where the unread run starts. Suppressed while a search is on: the line
  // belongs to the conversation's order, and filtered results are not it.
  var sepAfter = telSearchFor('thread') ? 0 : telSepFor(telPeerKey(peer));
  var sepDrawn = false;
  shown.forEach(function (m) {
    // Our own message is never "new to you", so it cannot open the run --
    // sending a reply and coming back would otherwise put the line under it.
    if (sepAfter && !sepDrawn && !m.out && m.id > sepAfter) {
      html += '<div class="tel-new-sep" id="telNewSep"><span dir="auto">' +
        telEsc(telT('new_messages')) + '</span></div>';
      sepDrawn = true;
    }
    var dateStr = telFmtDate(m.date);
    if (dateStr && dateStr !== lastDate) {
      html += '<div class="tel-date-sep"><span dir="auto">' + telEsc(dateStr) + '</span></div>';
      lastDate = dateStr;
      lastFrom = '';
    }
    html += '<div class="tel-msg ' + (m.out ? 'out' : 'in') +
      (m.failed ? ' failed' : '') + '" dir="auto" data-mid="' + (m.id | 0) + '"' +
      (m.failed ? ' onclick="telRetrySend(' + m.id + ')" title="' +
        telAttrQ(telT('tel_retry')) + '"' : '') + '>';
    // Consecutive messages from one person are named once, like Telegram.
    if (showFrom && !m.out && m.from && m.from !== lastFrom) {
      html += '<span class="tel-msg-from" dir="auto">' + telEsc(m.from) + '</span>';
    }
    lastFrom = m.out ? '' : (m.from || '');
    html += telReplyHTML(m, th);
    var media = telMediaHTML(m);
    if (media && !m.text) html += media;
    if (m.text) {
      // linkify() is the feed's: markdown links, bare URLs with Persian
      // trailing punctuation trimmed, and @mentions. It escapes everything
      // else itself and only matches http(s), so no scheme can sneak through.
      html += '<span class="tel-msg-text">' + telFlag(linkify(telSafeText(m.text))) + '</span>';
    }
    if (media && m.text) html += media;
    html += telReactionsHTML(m) + telButtonsHTML(m) +
      '<span class="tel-msg-meta">' + telStatsHTML(m) + '<span class="tel-msg-time">' +
      telEsc(telFmtTime(m.date)) + '</span>' + telTickHTML(m, readOutbox) + '</span></div>';
  });
  html += '</div>';

  // Telegram refuses a message here, so offering a composer would only
  // produce a failure the user cannot act on. No notice either: a channel you
  // only read is the normal case, and labelling it reads as something being
  // wrong. Leaving the composer out already says it.
  if (peer.readOnly) {
    return html;
  }
  html += '<div class="tel-compose">' +
    '<textarea id="telInput" class="tel-compose-input" rows="1" dir="auto" placeholder="' +
    telAttrQ(telT('tel_write')) + '" oninput="telState.draft=this.value;telGrowInput(this)" ' +
    'onkeydown="telInputKey(event)">' + telEsc(telState.draft) + '</textarea>' +
    '<button class="tel-send-btn" onclick="telSend()"' + (telState.busy ? ' disabled' : '') +
    ' aria-label="' + telAttrQ(telT('tel_send')) + '" title="' + telAttrQ(telT('tel_send')) + '">' +
    icon('send') + '</button></div>';
  return html;
}

// --------------------------------------------------------------- sheets ------
// One bottom sheet at a time, dismissed by the backdrop or Cancel.

function telCloseSheet() {
  var el = document.getElementById('telSheet');
  if (el) el.remove();
}
window.telCloseSheet = telCloseSheet;

function telSheet(itemsHTML) {
  telCloseSheet();
  var host = document.getElementById('telModal');
  if (!host) return;
  var wrap = document.createElement('div');
  wrap.className = 'tel-sheet-overlay';
  wrap.id = 'telSheet';
  wrap.onclick = function (e) { if (e.target === wrap) telCloseSheet(); };
  wrap.innerHTML = '<div class="tel-sheet">' + itemsHTML +
    '<button class="tel-sheet-item tel-sheet-cancel" onclick="telCloseSheet()">' +
    telEsc(telT('tel_cancel')) + '</button></div>';
  host.appendChild(wrap);
  hydrateIcons(wrap);
}

function telSheetItem(iconName, label, onclick) {
  return '<button class="tel-sheet-item" onclick="' + onclick + '">' +
    icon(iconName) + ' <span dir="auto">' + telEsc(label) + '</span></button>';
}

// telMenu is the ⋮ available in EVERY state, so nothing can strand the user.
function telMenu() {
  var info = telState.info || {};
  var items = '';
  items += telSheetItem('info', telT('tel_how_it_works'), 'telCloseSheet();telShowIntro()');
  items += telSheetItem('refresh', telT('tel_refresh'), 'telCloseSheet();telRefreshNow()');
  // Alerts: the sound is this page's, the background check is the backend's.
  items += telSheetItem('music', telT(telSoundOff() ? 'tel_sound_off' : 'tel_sound_on'),
    'telToggleSound();telMenu()');
  items += telSheetItem(info.watch ? 'success' : 'clock',
    telT(info.watch ? 'tel_watch_on' : 'tel_watch_off'), 'telToggleWatch()');
  if (telReady()) {
    items += telSheetItem('antenna', telT('tel_reload_chats'), 'telCloseSheet();telLoadDialogs(true)');
  }
  items += telSheetItem('shield', telT('tel_change_server') +
    (info.server ? ' · ' + telServerName(info.server) : ''), 'telCloseSheet();telChangeServer()');
  items += telSheetItem('edit', telT('tel_edit_creds'), 'telEditCredentials()');
  items += telSheetItem('log', telT('tel_log'), 'telCloseSheet();telOpenLog()');
  if (info.connected) {
    items += telSheetItem('close', telT('tel_disconnect'), 'telCloseSheet();telDisconnect()');
  }
  if (info.hasSession || telState.authorized) {
    items += '<button class="tel-sheet-item tel-sheet-danger" onclick="telLogout()">' +
      icon('delete') + ' <span dir="auto">' + telEsc(telT('tel_logout')) + '</span></button>';
  }
  telSheet(items);
}
window.telMenu = telMenu;

// telToggleWatch flips background checking; it spends queries, so it is opt-in.
async function telToggleWatch() {
  var on = !(telState.info && telState.info.watch);
  telCloseSheet();
  if (on) telRequestNotifyPermission();
  await telOp('tel_working', async function () {
    var r = await telCall('/api/tel/watch', { on: on });
    if (!r) return;
    await telRefreshInfo();
    showToast(telT(on ? 'tel_watch_on_toast' : 'tel_watch_off_toast'));
  });
}
window.telToggleWatch = telToggleWatch;

function telServerName(key) {
  var list = (telState.info && telState.info.servers) || [];
  for (var i = 0; i < list.length; i++) {
    if (list[i].key === key) return list[i].name || list[i].domain || key;
  }
  return key;
}

// telOpenLog shows the app log, where every tunnel query is printed.
function telOpenLog() {
  if (typeof toggleLog === 'function') { closeTel(); toggleLog(); return; }
  showToast(telT('tel_err_generic'));
}
window.telOpenLog = telOpenLog;

// telToggleMute flips a conversation's notifications. The setting is
// Telegram's own, so it follows the account everywhere rather than being a
// local preference this app alone honours.
async function telToggleMute(key) {
  var d = telDialogFor(key);
  if (!d) return;
  var want = !d.muted;
  // Shown straight away: the round trip is seconds on this link, and a switch
  // that does not move when tapped reads as broken.
  d.muted = want;
  telRender();
  var r = await telCall('/api/tel/mute', telPost({ peer: d.peer, muted: want }), 'tel_err_generic');
  if (!r || !r.ok) {
    d.muted = !want; // put it back: Telegram did not take it
    telRender();
  }
}
window.telToggleMute = telToggleMute;

function telRowMenu(key) {
  var d = telDialogFor(key);
  if (!d) return;
  var isChannel = d.peer.kind === 'channel' || d.peer.kind === 'forum';
  var items = telSheetItem('messenger', telT('tel_open'), 'telCloseSheet();telOpenPeer(\'' + telAttrQ(key) + '\')');
  items += telSheetItem(d.muted ? 'bell' : 'bellOff',
    telT(d.muted ? 'tel_unmute' : 'tel_mute'),
    'telCloseSheet();telToggleMute(\'' + telAttrQ(key) + '\')');
  if (d.peer.username) {
    // Pass the key, not the value: an attribute decodes its escapes before
    // the JS is parsed.
    items += telSheetItem('copy', '@' + d.peer.username,
      'telCloseSheet();telCopyUsername(\'' + telAttrQ(key) + '\')');
  }
  items += '<button class="tel-sheet-item tel-sheet-danger" onclick="telDeleteChat(\'' + telAttrQ(key) + '\')">' +
    icon('delete') + ' <span dir="auto">' +
    telEsc(telT(isChannel ? 'tel_leave' : 'tel_delete')) + '</span></button>';
  telSheet(items);
}
window.telRowMenu = telRowMenu;

function telThreadMenu() {
  var peer = telState.peer;
  if (!peer) return;
  var key = telPeerKey(peer);
  var items = telSheetItem('refresh', telT('tel_refresh'), 'telCloseSheet();telThreadFetchNew()');
  items += telSheetItem('clock', telT('tel_load_older'), 'telCloseSheet();telThreadFetchOlder()');
  // Mute belongs here as much as in the list: Telegram puts it in the open
  // conversation's own menu, which is where someone reading a noisy channel
  // actually reaches for it.
  var md = telDialogFor(key);
  if (md) {
    items += telSheetItem(md.muted ? 'bell' : 'bellOff',
      telT(md.muted ? 'tel_unmute' : 'tel_mute'),
      'telCloseSheet();telToggleMute(\'' + telAttrQ(key) + '\')');
  }
  if (peer.username) {
    items += telSheetItem('copy', '@' + peer.username,
      'telCloseSheet();telCopyUsername(\'' + telAttrQ(key) + '\')');
  }
  items += telSheetItem('arrowLeft', telT('tel_back'), 'telCloseSheet();telBackToList()');
  var vd = telDialogFor(key);
  if (vd && vd.visiting && (peer.kind === 'channel' || peer.kind === 'forum')) {
    items += telSheetItem('success', telT('tel_join'),
      'telCloseSheet();telJoinChannel(\'' + telAttrQ(key) + '\')');
  }
  if (telDialogFor(key)) {
    var isChannel = peer.kind === 'channel' || peer.kind === 'forum';
    items += '<button class="tel-sheet-item tel-sheet-danger" onclick="telDeleteChat(\'' + telAttrQ(key) + '\')">' +
      icon('delete') + ' <span dir="auto">' +
      telEsc(telT(isChannel ? 'tel_leave' : 'tel_delete')) + '</span></button>';
  }
  telSheet(items);
}
window.telThreadMenu = telThreadMenu;

function telNewChatSheet() {
  telSheet('<div class="tel-sheet-head" dir="auto">' + telEsc(telT('tel_new_chat')) + '</div>' +
    '<div class="tel-sheet-form">' +
    '<input id="telNewName" class="tel-input" type="text" dir="ltr" placeholder="@username" autocomplete="off">' +
    '<button class="tel-btn tel-btn-primary" onclick="telStartNewChat()">' +
    telEsc(telT('tel_open')) + '</button></div>' +
    '<div class="tel-hint" dir="auto">' + telEsc(telT('tel_new_chat_hint')) + '</div>');
  var el = document.getElementById('telNewName');
  if (el) el.focus();
}
window.telNewChatSheet = telNewChatSheet;

// telCopyUsername resolves the @name from the peer key, so no Telegram value
// is interpolated into an inline handler.
function telCopyUsername(key) {
  var d = telDialogFor(key);
  var peer = d ? d.peer : (telState.threads[key] && telState.threads[key].peer);
  if (!peer || !peer.username) return;
  telCopy('@' + peer.username);
}
window.telCopyUsername = telCopyUsername;

function telCopy(v) {
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(v).then(function () { showToast(telT('tel_copied')); },
      function () { showToast(v); });
    return;
  }
  showToast(v);
}
window.telCopy = telCopy;
