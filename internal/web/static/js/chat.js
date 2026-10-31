// ===== Chat messenger =====
// Standalone E2E messenger over the chat sub-domains. All markup is rendered
// here into #chatModal; state comes from /api/chat/*.

function chatIsMobile() {
  return 'ontouchstart' in window && window.innerWidth < 768;
}

// CHAT_KEEP_FOCUS goes on buttons pressed while typing (send, refresh). A
// press moves focus to the button, and on a phone losing focus closes the
// keyboard; cancelling mousedown keeps focus where it is and the click still
// fires.
var CHAT_KEEP_FOCUS = ' onmousedown="event.preventDefault()"';

var chatState = {
  open: false,
  view: 'list', // 'list' | 'thread'
  peer: null,
  info: null,      // { exists, address, backedUp, serverCount }
  avail: null,     // { available, reason, servers (each with its own limits), needsCreate } | null = checking
  availChecking: false, // an availability probe is in flight
  threads: [],
  contacts: {},
  pinned: {},        // addr -> true
  pendingServer: {}, // addr -> serverKey, for a new chat before its first send binds it
  peerStatus: {},    // addr -> {accepted, delivered, emojis}
  pendingServers: {}, // addr -> [serverKey] with not-yet-delivered out messages (for ✓✓ probes)
  scrollPeer: '',    // peer the scroll counters below belong to
  msgCount: 0,       // messages currently rendered in the open thread
  seenCount: 0,      // messages seen while at the bottom (for the ↓ button's unread badge)
  lastActivity: 0,   // epoch ms of the last sent/received message (drives the adaptive poll rate)
  sending: false,
  sendProg: null,   // {done,total} of the in-flight send, so a mid-send thread
                    // re-render can repaint the progress bar it destroys
  polling: false,   // a manual refresh is in flight (drives the ⟳ spinner)
  cdTimer: null,    // 1s ticker for the "next check in Ns" countdown
  nextPollAt: 0,    // epoch ms of the next BACKEND poll (pushed over SSE)
  pollIntervalMs: 20000, // backend's current cadence, for the ring's full sweep
  vvBound: false,   // visualViewport listener attached
  notifyReady: false,
  promptCb: null,    // pending chatPrompt callback
  renderPending: false, // a poll/status render was deferred while a popup was open
  guideAfterAvail: false, // show "checking servers" + open the server sheet once availability resolves
  draft: '', // current compose text — single source of truth (see chatRenderThread)
  curServer: '', // server the open conversation currently sends via
  quotaByServer: {}, // serverKey -> { remaining, resetUnix }: each server counts its own sends
  safetyOpen: false, // inline safety-code explainer expanded
  connecting: false, // "connecting…" indicator currently shown for the in-flight send
  sendConnTimer: null, // delayed reveal of the "connecting…" indicator
  sendError: '' // persistent error banner text
};

// First-run server guidance: while NO chat server is enabled, every messenger
// open probes all configs (with a "checking servers" popup) and then opens the
// server sheet so the user actually turns one on — instead of landing on the
// "pick a server" dead-end and forgetting. The "is a server enabled" truth is
// server-side (info.anyEnabled), NOT localStorage — Android changes the
// loopback port between launches, which wipes localStorage but not the on-disk
// enabled set. The probe is retried a few times so a momentary resolver outage
// doesn't make every server look chatless.
var CHAT_AVAIL_RETRIES = 4;      // probe attempts while guiding (resolvers may be flaky)
var CHAT_AVAIL_RETRY_MS = 2500;  // wait between probe attempts
var CHAT_AVAIL_TIMEOUT_MS = 12000; // hard cap per probe — the DNS round trip can be
                                   // slow (esp. Android); without it the "checking
                                   // servers" overlay could hang the whole UI

// Polling is owned ENTIRELY by the Go backend loop now — the frontend never
// polls on a timer (that would multiply server queries with every open tab).
// The backend polls every 20s (10s right after a send/receive) while a UI is
// connected, 90s in the background, and pushes its next-poll time over SSE so
// the countdown ring stays in sync. The only client-initiated poll is the
// manual ⟳ refresh button (chatPollNow). See chatOnSSE's 'nextpoll' handler.

// Pull-to-refresh (mobile): drag the message list / thread down past
// CHAT_PULL_TRIGGER px to poll for new mail. CHAT_REFRESH_MIN_MS throttles it —
// a pull within this window of the last refresh is ignored, so a flick can't
// spam the server.
var CHAT_PULL_TRIGGER = 64;        // px the finger must travel to fire
var CHAT_REFRESH_MIN_MS = 1000;    // min gap between pull refreshes

function chatT(key) { return t(key) || key }

var chatB32 = 'abcdefghijklmnopqrstuvwxyz234567';
function chatCanonAddr(s) {
  s = (s || '').trim().toLowerCase();
  if (!/^[a-z2-7]{20}$/.test(s)) return '';
  var last = chatB32.indexOf(s[19]);
  if (last < 0) return '';
  return s.slice(0, 19) + chatB32[last & 0x10];
}

function chatName(addr) {
  if (chatState.contacts[addr]) return chatState.contacts[addr];
  return addr.slice(0, 6) + '…' + addr.slice(-4);
}

// chatDir: UI base direction (rtl for Persian) — for lines starting with an LTR value.
function chatDir() { return (typeof lang !== 'undefined' && lang === 'fa') ? 'rtl' : 'ltr'; }

// chatFmtBidi fills a "{key}" template, escaping literals and <bdi>-wrapping
// values so an LTR value doesn't scramble RTL text.
function chatFmtBidi(template, vars) {
  return String(template).replace(/\{(\w+)\}|[^{]+/g, function (seg, key) {
    if (key != null) return '<bdi>' + esc(vars[key] != null ? String(vars[key]) : '') + '</bdi>';
    return esc(seg);
  });
}

// chatAvatarHTML renders an initial avatar (coloured circle + first letter;
// colour derived from the name, so stable per contact).
function chatAvatarColor(s) {
  var palette = ['#e57373', '#f06292', '#ba68c8', '#9575cd', '#7986cb',
    '#64b5f6', '#4fc3f7', '#4dd0e1', '#4db6ac', '#81c784',
    '#aed581', '#dce775', '#ffd54f', '#ffb74d', '#ff8a65'];
  var h = 0;
  for (var i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) >>> 0;
  return palette[h % palette.length];
}
function chatAvatarHTML(name, size) {
  size = size || 36;
  var disp = (name || '').trim() || '?';
  return '<div class="chat-avatar" style="width:' + size + 'px;height:' + size +
    'px;font-size:' + Math.round(size * 0.45) + 'px;background:' + chatAvatarColor(disp) + '">' +
    esc(disp.charAt(0).toUpperCase()) + '</div>';
}

// chatServerLabel maps a server key to its display name (operator-set, defaults
// to the domain).
function chatServerLabel(key) {
  if (!key) return '';
  var list = (chatState.avail && chatState.avail.servers) || [];
  for (var i = 0; i < list.length; i++) {
    if (list[i].key === key) return list[i].name || list[i].domain || key;
  }
  return key;
}

function chatFmtTime(ts) {
  if (!ts) return '';
  var d = new Date(ts * 1000);
  var locale = (typeof lang !== 'undefined' && lang === 'fa') ? 'fa-IR' : undefined;
  return d.toLocaleTimeString(locale, { hour: '2-digit', minute: '2-digit' });
}

// chatFmtDate renders a day label for the thread date separators — Jalali for
// Persian, Gregorian otherwise, matching the feed (media.js).
function chatFmtDate(ts) {
  if (!ts) return '';
  var d = new Date(ts * 1000);
  var fa = (typeof lang !== 'undefined' && lang === 'fa');
  var opts = { year: 'numeric', month: 'long', day: 'numeric' };
  if (fa) opts.calendar = 'persian';
  try {
    return d.toLocaleDateString(fa ? 'fa-IR' : undefined, opts);
  } catch (e) {
    return d.toLocaleDateString();
  }
}

// ===== open / close + hardware-back history =====
// The messenger is a full-screen modal with two single-pane views (list and
// thread). To make the Android/iOS hardware back button — and browser back —
// navigate thread→list→close instead of dropping out of the app, each layer
// pushes a history entry; the popstate handler unwinds them one at a time.
// Mirrors the telemirror modal pattern. Explicit close/back route through
// history too (history.go/back) so the button and the hardware key take the
// exact same path and the stack stays balanced.
var chatHistoryPushed = false; // messenger modal layer pushed
var chatThreadPushed = false;  // conversation (thread) layer pushed
var chatSuppressPopstate = 0;  // swallow the popstates our own history.go fires

function openMessenger(adopt) {
  // Chat is reparented into the shell: list → #chatSidebar, thread → #chatPane.
  // The section state (not a modal) drives the pane swap, like Mirror.
  document.documentElement.classList.add('chat-section');
  chatState.open = true;
  // `adopt` = we're switching from another section (Mirror), which left its
  // single history entry in place rather than popping it. Reuse that entry
  // instead of pushing a new one — avoids history.go() popstate cross-talk.
  if (adopt) {
    chatHistoryPushed = true;
  } else if (!chatHistoryPushed) {
    try { history.pushState({ view: 'chatMessenger' }, ''); chatHistoryPushed = true; } catch (e) { }
  }
  chatRenderList();
  if (chatState.view !== 'thread' || !chatState.peer) chatRenderContentEmpty();
  chatLoadInfo();
  chatLoadThreads();
  chatRequestNotifyPermission();
  chatStartForegroundTimer();
  chatBindViewport();
}

// chatTeardown hides the modal and stops the timers. It does NOT touch history
// — the caller decides whether to unwind (explicit close) or not (popstate has
// already popped us).
function chatTeardown() {
  chatStopForegroundTimer();
  chatUnbindViewport();
  chatState.open = false;
  chatState.view = 'list';
  chatState.peer = null;
  document.documentElement.classList.remove('chat-section');
  document.documentElement.classList.remove('chat-thread');
  var app = document.getElementById('app'); if (app) app.classList.remove('chat-open');
  var sb = document.getElementById('chatSidebar'); if (sb) sb.innerHTML = '';
  var pane = document.getElementById('chatPane'); if (pane) pane.innerHTML = '';
}

// chatCloseForNav tears chat down for a nav SECTION SWITCH (→ Mirror) without
// the history.go() unwind — that unwind fires popstates the other section's
// handler misreads. Returns whether chat owned a history entry so the next
// section can adopt it (keeping a single section entry on the stack).
window.chatCloseForNav = function () {
  var had = chatHistoryPushed || chatThreadPushed;
  chatHistoryPushed = false;
  chatThreadPushed = false;
  chatTeardown();
  return had;
};

// Placeholder shown in the content pane while no conversation is open
// (desktop two-pane), so it isn't blank next to the chat list.
function chatRenderContentEmpty() {
  var pane = document.getElementById('chatPane');
  if (pane) pane.innerHTML = '<div class="chat-content-empty">' +
    esc(chatT('chat_pick_conversation')) + '</div>';
}

// closeMessenger is the explicit close (the list-view back arrow, the
// not-configured banner). Tear down, then pop whatever history layers we
// pushed, suppressing the popstates that the unwind fires.
function closeMessenger() {
  var steps = (chatHistoryPushed ? 1 : 0) + (chatThreadPushed ? 1 : 0);
  chatHistoryPushed = false;
  chatThreadPushed = false;
  chatTeardown();
  document.documentElement.classList.remove('chat-thread');
  if (steps > 0) {
    chatSuppressPopstate += steps;
    try { history.go(-steps); } catch (e) { }
  }
}

// Hardware / browser back: unwind one layer at a time — thread→list, then
// close the modal. History already popped the entry, so we just sync the UI.
window.addEventListener('popstate', function () {
  if (chatSuppressPopstate > 0) { chatSuppressPopstate--; return; }
  if (chatThreadPushed) {
    chatThreadPushed = false;
    chatShowList();
    return;
  }
  if (chatHistoryPushed) {
    chatHistoryPushed = false;
    chatTeardown();
  }
});

function chatStopForegroundTimer() {
  if (chatState.cdTimer) { clearInterval(chatState.cdTimer); chatState.cdTimer = null; }
}

// chatBumpActivity records local activity (a send/receive). The poll CADENCE is
// owned by the backend now — the frontend never polls on a timer — so this only
// keeps the local stamp for any UI that reads it.
function chatBumpActivity() {
  chatState.lastActivity = Date.now();
}

// chatStartForegroundTimer drives the "next check in Ns" countdown ring while
// the messenger is open. The backend is the sole poller and pushes its schedule
// over SSE (chatOnSSE 'nextpoll'); here we just animate toward chatState.
// nextPollAt. NO polling happens on a frontend timer — that would multiply
// server queries with every open tab.
function chatStartForegroundTimer() {
  chatStopForegroundTimer();
  chatState.cdTimer = setInterval(chatUpdateCountdown, 1000);
  chatUpdateCountdown();
}

// Circular next-poll timer (a depleting ring with the seconds inside). Lives in
// the topbar corner; tapping it shows the full "next check in N sec" as a toast.
function chatTimerBtnHTML() {
  return '<button class="chat-icon-btn chat-timer-btn" onclick="chatToastNextPoll()" ' +
    'aria-label="' + escAttr(chatT('chat_next_check').replace('{n}', '')) + '">' +
    '<span class="chat-timer-ring"><span class="chat-timer-num"></span></span></button>';
}
window.chatToastNextPoll = function () {
  showToast(chatState._nextPollText || chatT('chat_next_check').replace('{n}', '?'));
};
function chatUpdateCountdown() {
  var secs = Math.max(0, Math.ceil((chatState.nextPollAt - Date.now()) / 1000));
  var text = chatT('chat_next_check').replace('{n}', secs);
  chatState._nextPollText = text;
  var total = ((chatState.pollIntervalMs || 20000) / 1000) || 1;
  var deg = Math.round(Math.max(0, Math.min(1, secs / total)) * 360);
  var rings = document.querySelectorAll('.chat-timer-btn');
  for (var i = 0; i < rings.length; i++) {
    rings[i].title = text;
    var ring = rings[i].querySelector('.chat-timer-ring');
    if (ring) ring.style.background = 'conic-gradient(var(--accent) ' + deg + 'deg, var(--border) ' + deg + 'deg)';
    var num = rings[i].querySelector('.chat-timer-num');
    if (num) num.textContent = secs;
  }
  var el = document.getElementById('chatNextPoll'); // back-compat if any view keeps it
  if (el) el.textContent = text;
}

// chatBindViewport keeps the input above the on-screen keyboard on mobile:
// without this, focusing the textarea shifts the whole fixed modal. We resize
// the modal to the visual viewport (which shrinks when the keyboard opens) so
// only the keyboard area is hidden and the input stays visible.
function chatBindViewport() {
  var vv = window.visualViewport;
  if (!vv || chatState.vvBound) return;
  vv.addEventListener('resize', chatViewportFit);
  vv.addEventListener('scroll', chatViewportFit);
  chatState.vvBound = true;
  chatViewportFit();
}

function chatUnbindViewport() {
  var vv = window.visualViewport;
  if (vv && chatState.vvBound) {
    vv.removeEventListener('resize', chatViewportFit);
    vv.removeEventListener('scroll', chatViewportFit);
  }
  chatState.vvBound = false;
  var m = document.getElementById('chatModal');
  if (m) { m.style.paddingBottom = ''; }
}

function chatViewportFit() {
  var vv = window.visualViewport;
  var m = document.getElementById('chatModal');
  if (!vv || !m) return;
  var kb = Math.max(0, window.innerHeight - vv.height - vv.offsetTop);
  // Note whether the thread was pinned to the bottom BEFORE we resize for the
  // keyboard (measure first — the padding change alters clientHeight).
  var body = document.getElementById('chatMsgsBody');
  var wasAtBottom = body && (body.scrollHeight - body.scrollTop - body.clientHeight < 80);
  m.style.paddingBottom = kb ? ('calc(var(--safe-bottom) + ' + kb + 'px)') : '';
  // Keyboard just opened while reading the latest messages → keep them visible
  // instead of letting the shrunk viewport hide them behind the keyboard.
  if (kb > 0 && wasAtBottom && body) {
    requestAnimationFrame(function () { body.scrollTop = body.scrollHeight; });
  }
}

// ---- new-message notification (foreground only) ----
// True background push isn't possible here: there's no push server, and the
// transport is censorship-resistant DNS. So we notify only while the app is
// open — a sound, plus a Web Notification when the tab/conversation isn't
// focused (works on desktop browsers and the Android WebView in foreground;
// iOS has no background mode for this either).

function chatRequestNotifyPermission() {
  try {
    if (typeof Notification !== 'undefined' && Notification.permission === 'default') {
      Notification.requestPermission().then(function () { chatState.notifyReady = true; });
    } else {
      chatState.notifyReady = true;
    }
  } catch (e) { }
}

// chatSoundOff reports whether the user muted the new-message sound (persisted).
function chatSoundOff() {
  try { return localStorage.getItem('chatSoundOff') === '1'; } catch (e) { return false; }
}

// chatToggleSound flips and persists the mute setting.
function chatToggleSound() {
  try { localStorage.setItem('chatSoundOff', chatSoundOff() ? '0' : '1'); } catch (e) { }
  showToast(chatT(chatSoundOff() ? 'chat_sound_off' : 'chat_sound_on'));
  if (chatState.view === 'list') chatRenderList(); else if (chatState.view === 'thread') chatRenderThread();
}

function chatBeep() {
  if (chatSoundOff()) return;
  try {
    var Ctx = window.AudioContext || window.webkitAudioContext;
    if (!Ctx) return;
    var ac = new Ctx();
    var o = ac.createOscillator(), g = ac.createGain();
    o.type = 'sine'; o.frequency.value = 880;
    g.gain.value = 0.06;
    o.connect(g); g.connect(ac.destination);
    o.start();
    setTimeout(function () { o.stop(); ac.close(); }, 150);
  } catch (e) { }
}

function chatNotifyNew(n) {
  // Skip if the user is actively looking at the conversation list/thread.
  var focused = chatState.open && !document.hidden;
  chatBeep();
  if (focused) return;
  try {
    if (typeof Notification !== 'undefined' && Notification.permission === 'granted') {
      new Notification(chatT('chat_btn'), { body: chatT('chat_new_messages').replace('{n}', n) });
    }
  } catch (e) { }
}

// chatLoadInfo fetches the LOCAL identity (instant — no DNS) so the address
// renders immediately, then kicks off the slow availability check.
async function chatLoadInfo() {
  try {
    var r = await fetch('/api/chat/info');
    chatState.info = await r.json();
  } catch (e) { chatState.info = null }
  if (chatState.open && chatState.view === 'list') chatRenderList();
  // No identity yet → show the opt-in prompt, don't probe servers (and don't
  // create anything) until the user enables the messenger.
  if (chatState.info && chatState.info.exists) {
    // Guide to the server sheet whenever no server is enabled yet — re-checked
    // from server-side truth every open (not localStorage), so it keeps asking
    // until the user finally turns one on, then goes quiet.
    chatState.guideAfterAvail = !chatState.info.anyEnabled;
    chatCheckAvailability();
  }
}

// chatEnableMessenger is the explicit opt-in: create the local identity, then
// load availability so the user can choose which servers to turn on.
async function chatEnableMessenger() {
  try {
    var r = await fetch('/api/chat/enable', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ action: 'create' })
    });
    if (!r.ok) { showToast(chatT('chat_err_generic')); return; }
  } catch (e) { showToast(chatT('chat_err_generic')); return; }
  chatState.avail = null;
  chatLoadInfo();
}

// chatCheckAvailability does the DNS capability probe in the background. The
// overlap guard lets the foreground timer call it repeatedly (to self-heal a
// rebooting server) without stacking slow probes.
// chatFetchTimeout is fetch with a hard timeout, so a hung DNS round trip can
// never wedge the UI (e.g. leave the "checking servers" overlay up forever).
function chatFetchTimeout(url, ms) {
  var ctrl = new AbortController();
  var t = setTimeout(function () { ctrl.abort(); }, ms);
  return fetch(url, { signal: ctrl.signal }).finally(function () { clearTimeout(t); });
}

async function chatCheckAvailability(force) {
  if (chatState.availChecking) return;
  chatState.availChecking = true;
  var guiding = chatState.guideAfterAvail;
  chatState.guideAfterAvail = false;
  // force=1 → ask the server to clear per-config backoffs first, so a manual
  // retry re-probes even servers parked after an earlier failure.
  var availURL = '/api/chat/availability' + (force ? '?retry=1' : '');
  // Everything below runs under one try/finally: availChecking and the blocking
  // "checking" overlay MUST be cleared even if a probe throws/aborts, or the
  // whole messenger wedges (taps eaten, renders deferred).
  try {
    if (guiding) chatShowCheckingServers();
    // While guiding, retry a few times: if the resolvers are momentarily down
    // every server looks chatless, so one failed probe must not be the verdict.
    var attempts = guiding ? CHAT_AVAIL_RETRIES : 1;
    for (var i = 0; i < attempts; i++) {
      try {
        var r = await chatFetchTimeout(availURL, CHAT_AVAIL_TIMEOUT_MS);
        var fresh = await r.json();
        // A row can come back without limits (not probed this time); keep
        // that server's last known ones.
        chatKeepRowLimits(fresh, chatState.avail);
        chatState.avail = fresh;
      } catch (e) { chatState.avail = { available: false, reason: 'chat_err_generic' }; }
      if (!chatState.open) break;                 // user left — stop probing
      if (chatAvailServers().length > 0) break;   // found a chat-capable server
      if (i + 1 < attempts) {
        await new Promise(function (res) { setTimeout(res, CHAT_AVAIL_RETRY_MS); });
        if (!chatState.open) break;
      }
    }
  } finally {
    chatState.availChecking = false;
    if (guiding) chatHideCheckingServers();
  }
  // Chat-capable servers exist but none enabled → open the toggles so the user
  // actually turns one on (only on the list, never over an open conversation).
  if (guiding && chatState.open && chatState.view === 'list' &&
    chatAvailServers().length > 0 && chatUsableServers().length === 0) {
    chatServerSheet();
  }
  if (!chatState.open) return;
  if (chatState.view === 'list') chatRenderList();
  else chatRenderThread();
  // If the server picker is open (showing disk-backed "checking" rows), refresh
  // it in place now that the live per-server status has landed.
  if (document.querySelector('.chat-server-sheet')) chatServerSheet();
}

// chatShowCheckingServers / chatHideCheckingServers: the first-run "probing all
// configs" popup. It uses the .chat-sheet-overlay class so the render guard
// keeps it (and focus) intact while the probe runs.
function chatShowCheckingServers() {
  if (document.getElementById('chatCheckingOverlay')) return;
  var ov = document.createElement('div');
  ov.className = 'chat-sheet-overlay';
  ov.id = 'chatCheckingOverlay';
  // Tappable backdrop: a slow/stuck DNS probe must never trap the user behind a
  // full-screen blocking spinner. Tapping cancels the guide and frees the UI.
  ov.onclick = function (e) {
    if (e.target !== ov) return;
    chatState.guideAfterAvail = false;
    chatHideCheckingServers();
    chatFlushPendingRender();
  };
  ov.innerHTML = '<div class="chat-sheet chat-checking-card">' +
    '<div class="chat-spinner" aria-hidden="true"></div>' +
    '<div class="chat-checking-title">' + esc(chatT('chat_checking_servers')) + '</div>' +
    '<div class="chat-sheet-hint">' + esc(chatT('chat_checking_servers_sub')) + '</div>' +
    '<button class="chat-sheet-item chat-sheet-cancel" onclick="chatState.guideAfterAvail=false;chatHideCheckingServers();chatFlushPendingRender()">' +
    esc(chatT('cancel')) + '</button></div>';
  document.getElementById('chatModal').appendChild(ov);
}
function chatHideCheckingServers() {
  var o = document.getElementById('chatCheckingOverlay');
  if (o) o.remove();
}

function chatAvailable() { return !!(chatState.avail && chatState.avail.available) }

function chatRetryAvailability() {
  chatState.avail = null;       // back to "checking…"
  if (chatState.view === 'list') chatRenderList();
  chatCheckAvailability(true);  // force: clear server-side backoffs first
}

async function chatLoadThreads() {
  try {
    var r = await fetch('/api/chat/threads');
    chatState.threads = (await r.json()) || [];
    chatState.pinned = {};
    chatState.threads.forEach(function (th) { if (th.pinned) chatState.pinned[th.addr] = true });
    var rc = await fetch('/api/chat/contacts');
    var contacts = (await rc.json()) || [];
    chatState.contacts = {};
    contacts.forEach(function (c) { chatState.contacts[c.addr] = c.name });
  } catch (e) { }
  chatUpdateBadge();
  // Refresh the sidebar list whenever chat is open — in the two-pane shell it's
  // visible alongside an open thread (desktop) or hidden behind it (mobile), so
  // keepView keeps the thread in place while the list updates.
  if (chatState.open) chatRenderList(chatState.view === 'thread');
}

function chatUpdateBadge() {
  var total = 0;
  (chatState.threads || []).forEach(function (th) { total += th.unread || 0 });
  var b = document.getElementById('chatSidebarBadge');
  if (!b) return;
  b.textContent = total;
  b.className = 'chat-sidebar-badge' + (total > 0 ? ' on' : '');
}

// chatMaxMap returns a new {serverKey:seq} holding the larger value per key
// across a and b (delivery counters only ever rise).
function chatMaxMap(a, b) {
  var out = {};
  [a, b].forEach(function (m) {
    if (!m) return;
    for (var k in m) if (m[k] > (out[k] || 0)) out[k] = m[k];
  });
  return out;
}

// chatMergeStatus combines two {accepted,delivered} status sources into one,
// taking the per-server max so the freshest counter from either side wins.
function chatMergeStatus(a, b) {
  return {
    accepted: chatMaxMap(a && a.accepted, b && b.accepted),
    delivered: chatMaxMap(a && a.delivered, b && b.delivered),
    emojis: (a && a.emojis) || (b && b.emojis)
  };
}

// Refresh the unread badge whenever the app returns to the foreground: a
// message stored while we were backgrounded (the case behind a tapped
// notification) has no live SSE event to replay, so reload the persisted
// threads. Without this the badge stays stale until the messenger is opened.
document.addEventListener('visibilitychange', function () {
  if (!document.hidden) chatLoadThreads();
});

function chatQuotaSvg(frac) {
  var r = 5, cx = 6, cy = 6, sz = 12;
  var circ = 2 * Math.PI * r;
  var fill = circ * frac;
  var color = frac <= 0.2 ? 'var(--error,#e53935)' : 'var(--text-dim)';
  return '<svg width="' + sz + '" height="' + sz + '" viewBox="0 0 ' + sz + ' ' + sz + '">' +
    '<circle cx="' + cx + '" cy="' + cy + '" r="' + r + '" fill="none" stroke="var(--border)" stroke-width="1.5"/>' +
    '<circle cx="' + cx + '" cy="' + cy + '" r="' + r + '" fill="none" stroke="' + color + '" stroke-width="1.5"' +
    ' stroke-dasharray="' + fill.toFixed(2) + ' ' + circ.toFixed(2) + '"' +
    ' stroke-linecap="round" transform="rotate(-90 ' + cx + ' ' + cy + ')"/>' +
    '</svg>';
}

// chatLimitsKey is the server the open conversation sends via, or '' when
// there is none yet.
function chatLimitsKey() {
  return chatState.curServer || (chatState.peer && chatState.pendingServer[chatState.peer]) || '';
}

// chatKeepRowLimits copies each server's limits from prev into fresh where
// fresh lacks them.
function chatKeepRowLimits(fresh, prev) {
  if (!fresh || !fresh.servers || !prev || !prev.servers) return;
  var old = {};
  prev.servers.forEach(function (s) { if (s.limits) old[s.key] = s.limits; });
  fresh.servers.forEach(function (s) { if (!s.limits && old[s.key]) s.limits = old[s.key]; });
}

// chatCurLimits returns the limits of the server the conversation uses. With
// no server chosen yet it takes the smallest of each limit across the usable
// servers, so the UI never promises more than the strictest one allows.
function chatCurLimits() {
  var rows = (chatState.avail && chatState.avail.servers) || [];
  var key = chatLimitsKey();
  if (key) {
    for (var i = 0; i < rows.length; i++) if (rows[i].key === key) return rows[i].limits || null;
    return null;
  }
  var out = null;
  rows.forEach(function (s) {
    if (!s.limits || !s.available || !s.enabled) return;
    if (!out) { out = Object.assign({}, s.limits); return; }
    Object.keys(s.limits).forEach(function (k) {
      if (s.limits[k] > 0 && (!(out[k] > 0) || s.limits[k] < out[k])) out[k] = s.limits[k];
    });
  });
  return out;
}

function chatCurQuota() {
  var key = chatLimitsKey();
  return key ? chatState.quotaByServer[key] || null : null;
}

function chatQuotaIcon() {
  var q = chatCurQuota();
  var lim = chatCurLimits();
  var cap = (lim && lim.sendPerHour) || 0;
  if (!cap) return null;
  var rem = (q && q.remaining != null) ? q.remaining : cap;
  var frac = Math.max(0, Math.min(1, rem / cap));
  var reset = (q && q.resetUnix) ? chatFmtTime(q.resetUnix) : '';
  var detail = (q && q.remaining != null)
    ? chatT('chat_quota').replace('{n}', q.remaining).replace('{t}', reset)
    : chatT('chat_quota_cap').replace('{n}', cap);
  return { svg: chatQuotaSvg(frac), detail: detail, low: frac <= 0.2 };
}

// chatQuotaCircleHTML renders the compose-row quota ring: a circle showing the
// remaining-sends count with a progress ring. Tap shows the full detail.
function chatQuotaCircleHTML() {
  var q = chatCurQuota();
  var lim = chatCurLimits();
  var cap = (lim && lim.sendPerHour) || 0;
  if (!cap) return '';
  var rem = (q && q.remaining != null) ? q.remaining : cap;
  var frac = Math.max(0, Math.min(1, rem / cap));
  var low = frac <= 0.2;
  var r = 15.5, c = 2 * Math.PI * r, off = c * (1 - frac);
  var col = low ? 'var(--error)' : 'var(--accent)';
  var ring = '<svg class="chat-quota-ring" viewBox="0 0 38 38" aria-hidden="true">'
    + '<circle cx="19" cy="19" r="' + r + '" fill="none" stroke="var(--border)" stroke-width="2.5"/>'
    + '<circle cx="19" cy="19" r="' + r + '" fill="none" stroke="' + col + '" stroke-width="2.5" stroke-linecap="round" stroke-dasharray="' + c.toFixed(1) + '" stroke-dashoffset="' + off.toFixed(1) + '" transform="rotate(-90 19 19)"/></svg>';
  return '<button type="button" class="chat-quota-btn' + (low ? ' low' : '') + '" title="' + escAttr((chatQuotaIcon() || {}).detail || '') + '" onclick="chatShowQuota(event)" aria-label="' + escAttr(chatT('chat_quota_label')) + '">'
    + ring + '<span class="chat-quota-num">' + rem + '</span></button>';
}

// chatShowQuota surfaces the full "N left · resets HH:MM" line on demand.
function chatShowQuota(e) {
  if (e) e.stopPropagation();
  var qi = chatQuotaIcon();
  if (qi) showToast(qi.detail);
}

function chatShowSendError(msg) {
  chatState.sendError = msg;
  var el = document.getElementById('chatSendError');
  if (el) { el.textContent = msg; el.style.display = msg ? 'block' : 'none'; }
}

function chatClearSendError() {
  chatState.sendError = '';
  var el = document.getElementById('chatSendError');
  if (el) { el.textContent = ''; el.style.display = 'none'; }
}

// ---- thread list view ----

// chatRenderIntro is the one-time opt-in screen: no keys exist yet and no
// server has been probed. Enabling creates the local identity.
function chatRenderIntro() {
  var html = '';
  html += '<div class="chat-topbar">';
  html += '<button class="chat-back chat-back-close" onclick="closeMessenger()" aria-label="close">' + icon('arrowLeft') + '</button>';
  html += '<div class="chat-topbar-title"><div class="chat-peer-name">' + esc(chatT('chat_title')) + '</div></div>';
  html += '<button class="chat-icon-btn" title="' + escAttr(chatT('chat_log')) + '" onclick="chatOpenLog()">' + icon('log') + '</button>';
  html += '</div>';
  html += '<div class="chat-body" id="chatBody">';
  html += '<div class="chat-card chat-intro">' +
    '<div class="chat-intro-title">' + esc(chatT('chat_enable_title')) + '</div>' +
    '<div class="chat-myaddr-hint">' + esc(chatT('chat_enable_desc')) + '</div>' +
    '<div class="chat-banner-actions"><button class="chat-btn-primary" onclick="chatEnableMessenger()">' +
    esc(chatT('chat_enable_btn')) + '</button></div></div>';
  html += '</div>';
  document.getElementById('chatSidebar').innerHTML = html;
}

// chatOverlayOpen reports whether any chat popup (action sheet, rename prompt,
// log) is on screen. They're all appended into #chatModal and share the
// .chat-sheet-overlay class. The periodic poll / status refresh must NOT
// rebuild the list/thread innerHTML while one is open — that would wipe the
// popup and steal keyboard focus. Such a render is deferred (renderPending)
// and flushed when the popup closes.
//
// Also covers the expanded recovery-code panel (#chatRecoveryBox): it lives
// inside the rendered list, so a re-render triggered by an incoming message
// would otherwise erase the recovery code while the user is reading it.
function chatOverlayOpen() {
  if (document.querySelector('#chatModal .chat-sheet-overlay')) return true;
  var rec = document.getElementById('chatRecoveryBox');
  if (rec && rec.style.display === 'block') return true;
  return false;
}

// chatFlushPendingRender re-renders the current view if a render was deferred
// while a popup was open. Deferred a tick so a close-then-reopen (e.g. a sheet
// refreshing itself) doesn't trigger a wasted render.
function chatFlushPendingRender() {
  setTimeout(function () {
    if (!chatState.renderPending || !chatState.open || chatOverlayOpen()) return;
    chatState.renderPending = false;
    if (chatState.view === 'thread') chatRenderThread();
    else chatRenderList();
  }, 0);
}

// chatRenderList renders the conversation list into #chatSidebar. keepView=true
// refreshes the sidebar WITHOUT switching away from an open thread — needed in
// the desktop two-pane shell where the sidebar and the thread are visible at
// once (e.g. a new conversation must appear in the list while you're in it).
function chatRenderList(keepView) {
  if (chatOverlayOpen()) { chatState.renderPending = true; return; }
  chatState.renderPending = false;
  if (!keepView) {
    chatState.view = 'list';
    document.documentElement.classList.remove('chat-thread');
  }
  var info = chatState.info || {};
  var avail = chatState.avail; // null = still checking

  // Not opted in yet → consent screen (no keys created, no servers probed).
  if (info.exists === false) { chatRenderIntro(); return; }

  var html = '';
  html += '<div class="chat-topbar">';
  html += '<button class="chat-back chat-back-close" onclick="closeMessenger()" aria-label="close">' + icon('arrowLeft') + '</button>';
  // The title doubles as the server picker: tap it (or the antenna button) to
  // see and toggle which servers the messenger is on for.
  // Subtitle: the live probe's usable count once it lands; before that, if a
  // server is already set up, the disk-backed enabled count (instant, from
  // /api/chat/info) so a known-good setup never shows "checking…" on every open.
  // Clean title — just "Chat". The server picker (and the live "N servers
  // active / checking…" status) moved into the ⋮ menu.
  html += '<div class="chat-topbar-title">' +
    '<div class="chat-peer-name">' + esc(chatT('chat_title')) + '</div></div>';
  if (chatAvailable()) html += chatTimerBtnHTML();
  if (chatAvailable()) {
    html += '<button id="chatRefreshBtn" class="chat-icon-btn' + (chatState.polling ? ' spinning' : '') + '" ' +
      (chatState.polling ? 'disabled ' : '') + 'title="' + escAttr(chatT('chat_refresh')) + '"' + CHAT_KEEP_FOCUS + ' onclick="chatPollNow()">' + icon('refresh') + '</button>';
  }
  // ⋮ shows a red dot until the recovery code has been backed up (the breadcrumb
  // leads the user: ⋮ → Recovery → "I saved it").
  html += '<button class="chat-icon-btn' + (info.address && info.backedUp === false ? ' chat-dot' : '') + '" title="' + escAttr(chatT('chat_menu')) + '" aria-label="' + escAttr(chatT('chat_menu')) + '" onclick="chatListMenu()">' + icon('more') + '</button>';
  html += '</div>';

  html += '<div class="chat-body" id="chatBody">';

  // Status banner: distinct states (checking / pick-a-server / need-key /
  // disabled / ok).
  // Rows still being probed are not a verdict: until one lands, this is the
  // "checking" state, not "no messenger here".
  var stillProbing = !!(avail && !avail.available &&
    (avail.servers || []).some(function (s) { return s.checking; }));
  if (avail === null || stillProbing) {
    // Only show "checking…" on a fresh setup. If a server is already enabled the
    // messenger is functional (threads + address render from local state), so
    // skip the banner and let the background probe quietly refresh — no flash on
    // every app open.
    if (!info.anyEnabled) {
      html += '<div class="chat-banner chat-banner-info">' + esc(chatT('chat_checking')) + '</div>';
    }
  } else if (!avail.available) {
    if (chatAvailServers().length > 0) {
      // Reachable servers exist but none is enabled — guide the opt-in (warn
      // colour so it reads as "action needed", not a neutral hint).
      html += '<div class="chat-banner chat-banner-warn">' + esc(chatT('chat_no_enabled_servers')) +
        ' <div class="chat-banner-actions"><button class="chat-btn-soft" onclick="chatServerSheet()">' +
        esc(chatT('chat_manage_servers')) + '</button></div></div>';
    } else {
      var reason = avail.reason || 'chat_err_disabled';
      html += '<div class="chat-banner chat-banner-warn">' + esc(chatT(reason));
      if (reason === 'chat_err_need_key' || reason === 'chat_err_no_key') {
        html += ' <div class="chat-banner-actions"><button class="chat-btn-soft" onclick="closeMessenger();openProfiles&&openProfiles()">' +
          esc(chatT('chat_edit_config')) + '</button></div>';
      } else if (reason === 'chat_err_unreachable') {
        html += ' <div class="chat-banner-actions"><button class="chat-btn-soft" onclick="chatRetryAvailability()">' +
          esc(chatT('chat_retry')) + '</button></div>';
      }
      html += '</div>';
    }
  }

  // My address — slim single row: label + FULL address (wraps on narrow
  // screens, never truncated) + eye + copy. The whole row copies; the eye
  // masks the address against shoulder-surfing (persisted; copy keeps
  // working while masked).
  if (info.address) {
    var addrMasked = false;
    try { addrMasked = localStorage.getItem('thefeed_chat_addr_mask') === '1'; } catch (e) { }
    html += '<div class="chat-addr-row" onclick="chatCopy(\'' + jsStr(info.address) + '\')">' +
      '<span class="chat-addr-label">' + esc(chatT('chat_my_address')) + '</span>' +
      '<button type="button" class="chat-icon-btn chat-addr-info" title="' + escAttr(chatT('chat_addr_info_btn')) + '" aria-label="' + escAttr(chatT('chat_addr_info_btn')) +
      '" onclick="event.stopPropagation();showInfoDialog(chatT(\'chat_addr_info\'))">' + icon('info') + '</button>' +
      (addrMasked
        ? '<span class="chat-addr-mask">••••</span>'
        : '<code class="chat-addr" dir="ltr">' + esc(info.address) + '</code>') +
      '<button type="button" class="chat-icon-btn chat-addr-eye" title="' + escAttr(chatT('chat_addr_mask')) + '" aria-label="' + escAttr(chatT('chat_addr_mask')) +
      '" onclick="event.stopPropagation();chatToggleAddrMask()">' + icon(addrMasked ? 'eyeOff' : 'views') + '</button>' +
      '<button type="button" class="chat-icon-btn chat-addr-copy" title="' + escAttr(chatT('chat_copy')) + '" aria-label="' + escAttr(chatT('chat_copy')) +
      '" onclick="event.stopPropagation();chatCopy(\'' + jsStr(info.address) + '\')">' + icon('copy') + '</button>' +
      '</div>';
  }

  html += '<div id="chatRecoveryBox" style="display:none"></div>';

  var chatComposeReady = chatAvailable() || chatEnabledServers().length;
  if (!chatState.threads.length) {
    if (chatAvailable()) html += '<div class="chat-empty">' + esc(chatT('chat_no_threads')) + '</div>';
    // First use: a visible labeled button — the FAB alone is not
    // discoverable enough for an empty list.
    if (chatComposeReady) {
      html += '<div class="chat-empty-cta"><button type="button" class="chat-btn-primary" onclick="chatOpenNewChatSheet()">' +
        icon('pencil') + ' ' + esc(chatT('chat_new_chat')) + '</button></div>';
    }
  } else {
    html += '<div class="chat-threads">';
    chatState.threads.forEach(function (th) {
      var nm = th.name || chatName(th.addr);
      var pinMark = th.pinned ? ('<span class="chat-pin-mark">' + icon('pinned') + '</span> ') : '';
      html += '<div class="chat-thread-item" onclick="openChatThread(\'' + jsStr(th.addr) + '\')">' +
        chatAvatarHTML(nm, 44) + // match the feed (.ch-avatar) and mirror list avatars (44px)
        '<div class="chat-thread-main"><div class="chat-thread-name">' + pinMark + esc(nm) + '</div>' +
        '<div class="chat-thread-last">' + esc(th.lastText || '') + '</div></div>' +
        (th.unread ? '<span class="chat-unread-badge">' + th.unread + '</span>' : '') +
        '<span class="chat-thread-time">' + chatFmtTime(th.lastTs) + '</span>' +
        '<button class="chat-icon-btn chat-row-menu" title="" onclick="event.stopPropagation();chatRowMenu(\'' + jsStr(th.addr) + '\',' + (th.pinned ? 'true' : 'false') + ')">' + icon('more') + '</button>' +
        '</div>';
    });
    html += '</div>';
  }
  html += '<div class="chat-progress-wrap"><div class="chat-progress" id="chatPollProgress"><div class="chat-progress-fill" id="chatPollProgressFill"></div></div><span class="chat-progress-label" id="chatPollProgressLabel"></span></div>';
  html += '</div>';

  // New chat: pencil FAB above the floating nav — replaces the old inline
  // add-contact card (the thread list now starts right below the address row).
  if (chatComposeReady) {
    html += '<button type="button" class="chat-fab" onclick="chatOpenNewChatSheet()" aria-label="' +
      escAttr(chatT('chat_new_chat')) + '" title="' + escAttr(chatT('chat_new_chat')) + '">' + icon('pencil') + '</button>';
  }

  document.getElementById('chatSidebar').innerHTML = html;
  chatBindPullRefresh(document.getElementById('chatBody'));
  chatUpdateCountdown();
}

function chatCopy(v) {
  var ok = function () { showToast(chatT('chat_copied')); };
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(v).then(ok, function () { if (chatCopyFallback(v)) ok(); });
    return;
  }
  // WebViews (and non-secure contexts) can lack the async clipboard API.
  if (chatCopyFallback(v)) ok();
}
function chatCopyFallback(v) {
  try {
    var ta = document.createElement('textarea');
    ta.value = v;
    ta.setAttribute('readonly', '');
    ta.style.position = 'fixed';
    ta.style.top = '-1000px';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    var done = document.execCommand('copy');
    document.body.removeChild(ta);
    return done;
  } catch (e) { return false; }
}

// chatCopyMsg copies a bubble's text, read from the DOM (never inlined into the
// handler) so message content with quotes/newlines can't break or inject.
function chatCopyMsg(btn) {
  var msg = btn.closest ? btn.closest('.chat-msg') : null;
  if (!msg) return;
  var txt = msg.querySelector('.chat-msg-text');
  if (txt) chatCopy(txt.textContent);
}

// chatForwardToSaved snapshots a chat bubble's (decrypted) text into Saved
// Messages as a kind:"chat" item, tagged with the current contact name. Text is
// read from the DOM, never inlined into the handler.
async function chatForwardToSaved(btn) {
  var msg = btn.closest ? btn.closest('.chat-msg') : null;
  if (!msg) return;
  var txtEl = msg.querySelector('.chat-msg-text');
  var text = txtEl ? txtEl.textContent : '';
  if (!text.trim()) return;
  var nameEl = document.querySelector('.chat-peer-name');
  var contactName = nameEl ? nameEl.textContent.trim() : '';
  try {
    var r = await fetch('/api/saved/from-chat', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ text: text, contactName: contactName })
    });
    if (r.ok) {
      var data = await r.json();
      var wasRemoved = data && data.toggled === 'removed';
      if (typeof showToast === 'function') {
        showToast(wasRemoved ? (t('unsaved_toast') || 'Removed from Saved') : (t('saved_toast') || 'Forwarded to Saved'));
      }
      if (typeof updateSavedBadge === 'function') updateSavedBadge();
      if (typeof getAllSaved === 'function') getAllSaved().then(function(items) { if (typeof savedMessages !== 'undefined') savedMessages = items; });
    } else {
      if (typeof showToast === 'function') showToast(t('saved_save_failed') || 'Could not save');
    }
  } catch (e) {
    if (typeof showToast === 'function') showToast(t('saved_save_failed') || 'Could not save');
  }
}
window.chatForwardToSaved = chatForwardToSaved;

async function chatShowRecovery() {
  var box = document.getElementById('chatRecoveryBox');
  if (!box) return;
  // Closing it lifts the render guard — flush any list update that arrived
  // (and was deferred) while the recovery code was on screen.
  if (box.style.display === 'block') { box.style.display = 'none'; chatFlushPendingRender(); return; }
  try {
    var r = await fetch('/api/chat/seed');
    var d = await r.json();
    box.style.display = 'block';
    box.innerHTML = '<div class="chat-card chat-recovery"><b>' + esc(chatT('chat_recovery_title')) + '</b>' +
      '<div class="chat-myaddr-hint">' + esc(chatT('chat_recovery_hint')) + '</div>' +
      '<div class="chat-recovery-code">' + esc(d.recovery) + '</div>' +
      '<div class="chat-banner-actions">' +
      '<button class="chat-btn-soft" onclick="chatCopy(\'' + jsStr(d.recovery) + '\')">' + esc(chatT('chat_copy')) + '</button>' +
      '<button class="chat-btn-soft' + ((chatState.info && chatState.info.backedUp === false) ? ' chat-dot' : '') + '" onclick="chatMarkBackedUp()">' + esc(chatT('chat_backup_saved')) + '</button>' +
      '<button class="chat-btn-soft" onclick="chatImportPrompt()">' + esc(chatT('chat_import')) + '</button>' +
      '</div></div>';
  } catch (e) { }
}

async function chatMarkBackedUp() {
  await fetch('/api/chat/seed', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ action: 'backedup' }) });
  if (chatState.info) chatState.info.backedUp = true;
  // Collapse the recovery panel first so the list re-render isn't blocked by the
  // open-overlay guard (chatOverlayOpen treats the visible box as an overlay).
  var box = document.getElementById('chatRecoveryBox');
  if (box) box.style.display = 'none';
  chatRenderList();
}

function chatImportPrompt() {
  chatPrompt(chatT('chat_import'), '', chatT('chat_import_ph'), async function (code) {
    if (!code || !code.trim()) return;
    var r = await fetch('/api/chat/seed', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ action: 'import', recovery: code.trim() }) });
    if (r.ok) {
      showToast(chatT('chat_imported'));
      chatState.avail = null;
      chatLoadInfo();
    } else {
      showToast(chatT('chat_import_invalid'));
    }
  });
}

// chatAvailServers returns the chat-capable servers (from the availability
// probe) that are reachable right now (whether or not the user enabled them).
function chatAvailServers() {
  var list = (chatState.avail && chatState.avail.servers) || [];
  return list.filter(function (s) { return s.available });
}

// chatUsableServers returns servers that are both reachable AND turned on by
// the user — the ones you can actually send through.
function chatUsableServers() {
  return chatAvailServers().filter(function (s) { return s.enabled });
}

// chatEnabledServers: servers the user has turned on. Prefers the live probe
// (reachable + enabled); before it lands, falls back to the disk-backed
// /api/chat/info list so the compose field and "new chat" work on first open
// without waiting for the slow availability probe.
function chatEnabledServers() {
  var usable = chatUsableServers();
  if (usable.length) return usable;
  var info = chatState.info || {};
  return (info.servers || []).filter(function (s) { return s.enabled });
}

// chatServerSheet lists every chat-capable server with its state and an on/off
// toggle, so the user controls exactly which servers learn their address.
function chatServerSheet() {
  chatCloseMenu();
  // Prefer the live probe; before it lands, show the disk-backed server list
  // (from /api/chat/info) with each row marked "checking" so the user sees ALL
  // their servers — active and inactive — instead of a blank "checking…" screen.
  var probed = (chatState.avail && chatState.avail.servers) || [];
  var servers = probed;
  if (!probed.length) {
    servers = ((chatState.info && chatState.info.servers) || []).map(function (s) {
      return { key: s.key, name: s.name, domain: s.domain, enabled: s.enabled, checking: true };
    });
  }
  var sheet = document.createElement('div');
  sheet.className = 'chat-sheet-overlay chat-server-sheet';
  sheet.id = 'chatSheet';
  sheet.onclick = function (e) { if (e.target === sheet) chatCloseMenu(); };
  var items = '<div class="chat-sheet-title">' + esc(chatT('chat_servers_title')) + '</div>' +
    '<div class="chat-sheet-hint">' + esc(chatT('chat_servers_hint')) + '</div>';
  if (!servers.length) {
    items += '<div class="chat-sheet-hint">' + esc(chatT('chat_checking')) + '</div>';
  }
  servers.forEach(function (sv) {
    var label = sv.name ? (sv.name + ' — ' + sv.domain) : sv.domain;
    var state = sv.checking ? chatT('chat_checking')
      : !sv.available ? chatT('chat_server_unreachable')
      : (sv.enabled ? chatT('chat_server_on') : chatT('chat_server_off'));
    var action = sv.enabled ? chatT('chat_server_turn_off') : chatT('chat_server_turn_on');
    // No button for a chatless server; busy state while a toggle is in flight;
    // "Check again" for a transiently-unreachable one.
    var btn = '';
    if (sv.toggling || sv.checking) {
      btn = '<button class="chat-btn-soft chat-btn-busy" disabled>' + esc(chatT('chat_server_connecting')) + '</button>';
    } else if (sv.available || sv.enabled) {
      // Accent pill for "Turn on", plain for "Turn off".
      var cls = sv.enabled ? 'chat-btn-soft' : 'chat-btn-soft chat-btn-enable';
      btn = '<button class="' + cls + '" onclick="chatToggleServer(\'' + jsStr(sv.key) + '\',' +
        (sv.enabled ? 'false' : 'true') + ')">' + esc(action) + '</button>';
    } else if (sv.reason !== 'chat_err_disabled') {
      btn = '<button class="chat-btn-soft" onclick="chatRecheckServer(\'' + jsStr(sv.key) + '\')">' +
        esc(chatT('chat_server_recheck')) + '</button>';
    }
    items += '<div class="chat-server-row">' +
      '<div class="chat-server-info"><div class="chat-server-name">' + esc(label) + '</div>' +
      '<div class="chat-server-state' + (sv.enabled ? ' on' : '') + (sv.available ? '' : ' off') + '">' + esc(state) + '</div></div>' +
      btn + '</div>';
  });
  items += '<button class="chat-sheet-item chat-sheet-cancel" onclick="chatCloseMenu()">' + esc(chatT('cancel')) + '</button>';
  sheet.innerHTML = '<div class="chat-sheet">' + items + '</div>';
  document.getElementById('chatModal').appendChild(sheet);
}

// chatToggleServer turns the messenger on/off for one server. Turning it on
// publishes the user's address to that server (and only that server).
async function chatToggleServer(key, on) {
  if (!chatState.avail) chatState.avail = { available: false, servers: [] };
  var list = chatState.avail.servers || (chatState.avail.servers = []);
  var row = null;
  list.forEach(function (s) { if (s.key === key) row = s; });
  if (!row) { row = { key: key }; list.push(row); }
  // Enabling registers synchronously; ignore repeat taps while in flight and
  // show the button busy so stacked taps don't fire racing registers.
  if (row.toggling) return;
  row.toggling = true;
  chatServerSheet();
  var data = null, ok = false;
  try {
    var r = await fetch('/api/chat/enable', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ action: 'server', server: key, on: on })
    });
    ok = r.ok;
    if (ok) data = await r.json();
  } catch (e) { ok = false; }
  row.toggling = false;
  if (!ok) { showToast(chatT('chat_err_generic')); chatServerSheet(); return; }
  // Trust the enable response's verdict directly; a fresh probe could transiently
  // fail and flip the just-enabled server back to unavailable.
  row.enabled = on;
  if (on && data) { row.available = !!data.available; if (data.reason) row.reason = data.reason; }
  chatState.avail.available = chatUsableServers().length > 0;
  chatServerSheet();
  if (chatState.view === 'list') chatRenderList();
  // Surface a real registration failure rather than leaving the server
  // toggled-on-but-unusable with no explanation.
  if (on && data && data.available === false) {
    showToast(chatT(data.checking ? 'chat_registering' : (data.reason || 'chat_err_unreachable')));
  }
}

// chatRecheckServer force-probes one server, ignoring cache/backoff.
async function chatRecheckServer(key) {
  if (!chatState.avail) chatState.avail = { available: false, servers: [] };
  var list = chatState.avail.servers || (chatState.avail.servers = []);
  var row = null;
  list.forEach(function (s) { if (s.key === key) row = s; });
  if (!row) { row = { key: key }; list.push(row); }
  if (row.checking || row.toggling) return;
  row.checking = true;
  chatServerSheet();
  var data = null, ok = false;
  try {
    // Generous timeout: the probe retries hard on a flaky net (server-side).
    var r = await chatFetchTimeout('/api/chat/probe?retry=1&server=' + encodeURIComponent(key), 55000);
    ok = r.ok;
    if (ok) data = await r.json();
  } catch (e) { ok = false; }
  row.checking = false;
  if (ok && data && data.server) {
    row.available = !!data.server.available;
    row.reason = data.server.reason || '';
  } else if (!ok) {
    showToast(chatT('chat_err_generic'));
  }
  chatState.avail.available = chatUsableServers().length > 0;
  chatServerSheet();
  if (chatState.view === 'list') chatRenderList();
}
window.chatRecheckServer = chatRecheckServer;

// chatToggleAddrMask flips the shoulder-surfing mask on the my-address row.
function chatToggleAddrMask() {
  try {
    var k = 'thefeed_chat_addr_mask';
    localStorage.setItem(k, localStorage.getItem(k) === '1' ? '0' : '1');
  } catch (e) { }
  chatRenderList();
}

// chatOpenNewChatSheet is the new-conversation bottom sheet (opened by the
// pencil FAB): a large address field with a paste helper — easier than
// long-press paste — and one primary start action. Uses .chat-sheet-overlay
// so the render guard defers list re-renders while it's open.
function chatOpenNewChatSheet() {
  chatCloseMenu();
  var ov = document.createElement('div');
  ov.className = 'chat-sheet-overlay';
  ov.id = 'chatSheet';
  ov.onclick = function (e) { if (e.target === ov) chatCloseMenu(); };
  ov.innerHTML = '<div class="chat-sheet" dir="' + chatDir() + '">' +
    '<div class="chat-sheet-title">' + esc(chatT('chat_new_chat')) + '</div>' +
    '<div class="chat-newchat-row">' +
    '<input id="chatAddAddr" placeholder="' + escAttr(chatT('chat_add_contact_ph')) +
    '" autocapitalize="off" autocomplete="off" spellcheck="false" onkeydown="if(event.key===\'Enter\')chatStartNew()">' +
    '<button type="button" class="chat-btn-soft" onclick="chatPasteAddr()">' + esc(chatT('chat_paste')) + '</button></div>' +
    '<button type="button" class="chat-btn-primary chat-newchat-start" onclick="chatStartNew()">' + esc(chatT('chat_start_chat')) + '</button>' +
    '<button type="button" class="chat-sheet-item chat-sheet-cancel" onclick="chatCloseMenu()">' + esc(chatT('cancel')) + '</button>' +
    '</div>';
  document.getElementById('chatModal').appendChild(ov);
  setTimeout(function () {
    var i = document.getElementById('chatAddAddr');
    if (i) try { i.focus(); } catch (e) { }
  }, 60);
}

// chatPasteAddr fills the new-chat field from the clipboard; WebViews and
// non-secure contexts may lack readText — then just focus so the user
// pastes manually.
function chatPasteAddr() {
  var inp = document.getElementById('chatAddAddr');
  if (!inp) return;
  if (navigator.clipboard && navigator.clipboard.readText) {
    navigator.clipboard.readText().then(function (v) {
      if (v) inp.value = v.trim();
      inp.focus();
    }, function () { inp.focus(); });
    return;
  }
  inp.focus();
}

function chatStartNew() {
  var inp = document.getElementById('chatAddAddr');
  var addr = chatCanonAddr(inp.value);
  if (!addr) { showToast(chatT('chat_bad_address')); return }
  // A valid address that's your own — say so, don't call it "invalid".
  if (addr === (chatState.info && chatState.info.address)) { showToast(chatT('chat_self_address')); return }
  // Enabled servers, falling back to the disk-backed list so starting a chat
  // works on first open before the availability probe lands.
  var servers = chatEnabledServers();
  if (!servers.length) { chatServerSheet(); return; } // turn on a server first
  if (servers.length === 1) {
    chatState.pendingServer[addr] = servers[0].key;
    openChatThread(addr);
    return;
  }
  // Multiple chat servers: ask which one to start the conversation on.
  chatServerPicker(addr, servers);
}

// chatServerPicker shows an action sheet of chat servers for a new chat.
function chatServerPicker(addr, servers) {
  chatCloseMenu();
  var sheet = document.createElement('div');
  sheet.className = 'chat-sheet-overlay';
  sheet.id = 'chatSheet';
  sheet.onclick = function (e) { if (e.target === sheet) chatCloseMenu(); };
  var items = '<div class="chat-sheet-title">' + esc(chatT('chat_pick_server')) + '</div>';
  servers.forEach(function (sv) {
    var label = (sv.name ? sv.name + ' — ' : '') + sv.domain;
    items += '<button class="chat-sheet-item" onclick="chatPickServer(\'' + jsStr(addr) + '\',\'' + jsStr(sv.key) + '\')">' +
      esc(label) + '</button>';
  });
  items += '<button class="chat-sheet-item chat-sheet-cancel" onclick="chatCloseMenu()">' + esc(chatT('cancel')) + '</button>';
  sheet.innerHTML = '<div class="chat-sheet">' + items + '</div>';
  document.getElementById('chatModal').appendChild(sheet);
}

function chatPickServer(addr, key) {
  chatCloseMenu();
  chatState.pendingServer[addr] = key;
  openChatThread(addr);
}

// ---- conversation view ----

async function openChatThread(addr) {
  // Opening a conversation is explicit navigation: tear down EVERY overlay
  // (server picker, the first-run "checking servers" spinner, a stray prompt) so
  // the render guard can't defer it and a full-screen overlay can't eat the tap.
  document.querySelectorAll('#chatModal .chat-sheet-overlay').forEach(function (o) { o.remove(); });
  // The expanded recovery-code panel also trips chatOverlayOpen(), which would
  // defer BOTH the list and thread render and leave a blank "select a
  // conversation" pane. Collapse it too.
  var _rec = document.getElementById('chatRecoveryBox'); if (_rec) _rec.style.display = 'none';
  chatState.guideAfterAvail = false; // don't re-pop the guide over the thread
  chatState.renderPending = false;
  chatState.view = 'thread';
  chatState.peer = addr;
  chatState.draft = ''; // each conversation starts with an empty compose box
  chatState.safetyOpen = false; // collapse the safety explainer per conversation
  // Clear this conversation's unread badge immediately. The messages fetch in
  // chatRenderThread uses markRead=1 (clears it server-side); reflect it locally
  // now so the sidebar list row + global badge update without waiting for the
  // next thread reload. In the desktop two-pane the list stays visible next to
  // the open thread, so a stale "1" would otherwise linger by the name.
  (chatState.threads || []).forEach(function (th) { if (th.addr === addr) th.unread = 0; });
  chatUpdateBadge();
  if (chatState.open) chatRenderList(true); // keepView: don't leave the thread
  if (!chatThreadPushed) {
    try { history.pushState({ view: 'chatThread' }, ''); chatThreadPushed = true; } catch (e) { }
  }
  // Mobile: swap to the content pane (reuse the feed's pane-swap).
  if (typeof mobileQuery !== 'undefined' && mobileQuery.matches) {
    var app = document.getElementById('app'); if (app) app.classList.add('chat-open');
  }
  await chatRenderThread();
  // ✓✓ + safety emojis arrive async (it's a DNS round trip). Later refreshes
  // ride the backend's poll cadence — see chatOnSSE's 'nextpoll' handler.
  chatFetchPeerStatus(addr);
}

// chatThreadRefresh is the manual ⟳ in the conversation header: poll the
// inbox now and re-check delivery status.
async function chatThreadRefresh() {
  var peer = chatState.peer;
  if (!peer) return;
  chatFetchPeerStatus(peer);
  await chatPollNow();
}

// chatToggleSafety expands/collapses the safety-code explainer (state-backed so
// a re-render keeps it open).
function chatToggleSafety(el) {
  chatState.safetyOpen = !chatState.safetyOpen;
  if (el) el.classList.toggle('open', chatState.safetyOpen);
}

// chatMsgMenu is the per-message action sheet (copy / save-to-Saved) —
// opened by tapping a bubble or its ⋮. Reply joins this menu when the
// reply feature lands.
function chatMsgMenu(msgEl) {
  if (!msgEl || !msgEl.classList || !msgEl.classList.contains('chat-msg')) return;
  // Don't eat a click that ends a text selection.
  try { if (window.getSelection && String(window.getSelection())) return; } catch (e) { }
  chatCloseMenu();
  var sheet = document.createElement('div');
  sheet.className = 'chat-sheet-overlay';
  sheet.id = 'chatSheet';
  sheet.onclick = function (e) { if (e.target === sheet) chatCloseMenu(); };
  sheet.innerHTML = '<div class="chat-sheet" dir="' + chatDir() + '">' +
    '<button class="chat-sheet-item" id="chatMsgMenuCopy">' + icon('copy') + ' ' + esc(chatT('chat_copy')) + '</button>' +
    '<button class="chat-sheet-item" id="chatMsgMenuSave">' + icon('bookmark') + ' ' + esc(t('forward_to_saved')) + '</button>' +
    '<button class="chat-sheet-item chat-sheet-cancel" onclick="chatCloseMenu()">' + esc(chatT('cancel')) + '</button>' +
    '</div>';
  document.getElementById('chatModal').appendChild(sheet);
  // closest('.chat-msg') matches the element itself, so the existing
  // button-based helpers work unchanged with the bubble as the anchor.
  sheet.querySelector('#chatMsgMenuCopy').onclick = function () { chatCloseMenu(); chatCopyMsg(msgEl); };
  sheet.querySelector('#chatMsgMenuSave').onclick = function () { chatCloseMenu(); chatForwardToSaved(msgEl); };
}

// chatRowMenu shows an action sheet (pin/unpin + delete) for a list row.
function chatRowMenu(addr, pinned) {
  chatCloseMenu();
  var nm = chatState.contacts[addr] || chatName(addr);
  var sheet = document.createElement('div');
  sheet.className = 'chat-sheet-overlay';
  sheet.id = 'chatSheet';
  sheet.onclick = function (e) { if (e.target === sheet) chatCloseMenu(); };
  sheet.innerHTML =
    '<div class="chat-sheet">' +
    '<div class="chat-sheet-title">' + esc(nm) + '</div>' +
    '<button class="chat-sheet-item" onclick="chatPin(\'' + jsStr(addr) + '\',' + (pinned ? 'false' : 'true') + ')">' +
    icon('pinned') + ' ' + esc(chatT(pinned ? 'chat_unpin' : 'chat_pin')) + '</button>' +
    '<button class="chat-sheet-item chat-sheet-danger" onclick="chatDelete(\'' + jsStr(addr) + '\')">' +
    icon('delete') + ' ' + esc(chatT('chat_delete')) + '</button>' +
    '<button class="chat-sheet-item chat-sheet-cancel" onclick="chatCloseMenu()">' + esc(chatT('cancel')) + '</button>' +
    '</div>';
  document.getElementById('chatModal').appendChild(sheet);
}

function chatCloseMenu() {
  var s = document.getElementById('chatSheet');
  if (s) s.remove();
  chatFlushPendingRender();
}

// chatPrompt is an in-app replacement for the browser's prompt() (which looks
// out of place and shows the page URL). cb receives the entered string, or is
// not called on cancel.
function chatPrompt(title, value, placeholder, cb) {
  chatPromptClose();
  var ov = document.createElement('div');
  ov.className = 'chat-sheet-overlay chat-prompt-overlay';
  ov.id = 'chatPromptOverlay';
  ov.onclick = function (e) { if (e.target === ov) chatPromptClose(); };
  ov.innerHTML = '<div class="chat-prompt">' +
    '<div class="chat-prompt-title">' + esc(title) + '</div>' +
    '<input id="chatPromptInput" class="chat-prompt-input" dir="auto" value="' + escAttr(value || '') + '" placeholder="' + escAttr(placeholder || '') + '">' +
    '<div class="chat-prompt-actions">' +
    '<button class="chat-btn-soft" onclick="chatPromptClose()">' + esc(chatT('cancel')) + '</button>' +
    '<button class="chat-btn-primary" onclick="chatPromptOK()">' + esc(chatT('ok')) + '</button>' +
    '</div></div>';
  document.getElementById('chatModal').appendChild(ov);
  chatState.promptCb = cb;
  var inp = document.getElementById('chatPromptInput');
  if (inp) {
    try { inp.focus(); inp.select(); } catch (e) { }
    inp.onkeydown = function (e) {
      if (e.key === 'Enter') { e.preventDefault(); chatPromptOK(); }
      else if (e.key === 'Escape') { chatPromptClose(); }
    };
  }
}

function chatPromptOK() {
  var inp = document.getElementById('chatPromptInput');
  var v = inp ? inp.value : '';
  var cb = chatState.promptCb;
  chatPromptClose();
  if (cb) cb(v);
}

function chatPromptClose() {
  chatState.promptCb = null;
  var o = document.getElementById('chatPromptOverlay');
  if (o) o.remove();
  chatFlushPendingRender();
}

async function chatPin(addr, pin) {
  chatCloseMenu();
  await fetch('/api/chat/thread', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ peer: addr, action: pin ? 'pin' : 'unpin' }) });
  chatState.pinned[addr] = pin;
  chatLoadThreads();
}

async function chatDelete(addr) {
  chatCloseMenu();
  if (!(await showConfirmDialog(chatT('chat_delete_confirm')))) return;
  await fetch('/api/chat/thread', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ peer: addr, action: 'delete' }) });
  showToast(chatT('chat_deleted'));
  chatBackToList();
}

// chatClearMessages wipes the message history of a conversation but keeps the
// conversation itself (contact, server, seq counters, ✓/✓✓). Local-only.
async function chatClearMessages(addr) {
  chatCloseMenu();
  if (!(await showConfirmDialog(chatT('chat_clear_confirm')))) return;
  await fetch('/api/chat/thread', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ peer: addr, action: 'clear' }) });
  showToast(chatT('chat_cleared'));
  if (chatState.view === 'thread' && chatState.peer === addr) chatRenderThread();
  chatLoadThreads();
}

// (chatThreadMenu removed — the thread ⋮ now opens the merged chatContactInfo
// panel, which includes Clear messages / Delete.)

// chatListMenu is the ⋮ overflow on the chat list: sound toggle and log.
function chatListMenu() {
  chatCloseMenu();
  var sheet = document.createElement('div');
  sheet.className = 'chat-sheet-overlay';
  sheet.id = 'chatSheet';
  sheet.onclick = function (e) { if (e.target === sheet) chatCloseMenu(); };
  // Server picker + live status, moved out of the (cluttered) topbar.
  var info = chatState.info || {};
  var srvCount = chatState.avail ? chatUsableServers().length : (info.anyEnabled ? (info.enabledCount || 0) : null);
  var srvLabel = (srvCount === null)
    ? chatT('chat_checking')
    : chatT('chat_servers_active').replace('{n}', srvCount);
  // Recovery code lives here now (out of the address card). Red dot until the
  // user backs it up — and chatShowRecovery puts the dot on its "I saved it".
  var recoveryDot = (info.address && info.backedUp === false) ? ' chat-dot' : '';
  sheet.innerHTML =
    '<div class="chat-sheet">' +
    '<button class="chat-sheet-item" onclick="chatCloseMenu();chatServerSheet()">' +
    icon('antenna') + ' ' + esc(srvLabel) + '</button>' +
    '<button class="chat-sheet-item' + recoveryDot + '" onclick="chatCloseMenu();chatShowRecovery()">' +
    icon('bookmark') + ' ' + esc(chatT('chat_recovery_title')) + '</button>' +
    '<button class="chat-sheet-item" onclick="chatToggleSound();chatListMenu()">' +
    icon('music') + ' ' + esc(chatT(chatSoundOff() ? 'chat_sound_off' : 'chat_sound_on')) + '</button>' +
    '<button class="chat-sheet-item" onclick="chatCloseMenu();chatOpenLog()">' +
    icon('log') + ' ' + esc(chatT('chat_log')) + '</button>' +
    '<button class="chat-sheet-item" onclick="chatCloseMenu();chatCellSizeSheet()">' +
    icon('stats') + ' ' + esc(chatT('chat_query_size')) + '</button>' +
    '<button class="chat-sheet-item chat-sheet-cancel" onclick="chatCloseMenu()">' + esc(chatT('cancel')) + '</button>' +
    '</div>';
  document.getElementById('chatModal').appendChild(sheet);
}

// chatCellSizeSheet lets the user trade query size against query count (RFC
// §8.2): Compact blends chat into the feed's query-length cloud (more, smaller
// queries); Wide is fewer, bigger queries.
function chatCellSizeSheet() {
  fetch('/api/chat/settings').then(function (r) { return r.json(); }).then(function (cur) {
    var presets = cur.presets || { compact: 8, standard: 15, wide: 21 };
    var byBudget = chatBudgetScores(cur.scores); // budget -> {queries, errors, used}
    var opts = [
      { key: 'auto', label: chatT('chat_size_auto'), hint: chatT('chat_size_auto_hint') },
      { key: 'compact', label: chatT('chat_size_compact'), hint: chatT('chat_size_compact_hint') },
      { key: 'standard', label: chatT('chat_size_standard'), hint: chatT('chat_size_standard_hint') },
      { key: 'wide', label: chatT('chat_size_wide'), hint: chatT('chat_size_wide_hint') }
    ];
    var sheet = document.createElement('div');
    sheet.className = 'chat-sheet-overlay';
    sheet.id = 'chatSheet';
    sheet.onclick = function (e) { if (e.target === sheet) chatCloseMenu(); };
    var items = '<div class="chat-sheet-title">' + esc(chatT('chat_query_size')) + '</div>' +
      '<div class="chat-sheet-hint">' + esc(chatT('chat_query_size_hint')) + '</div>';
    opts.forEach(function (p) {
      var on = cur && cur.mode === p.key;
      var score = '';
      if (cur.mode === 'auto' && p.key !== 'auto') {
        var sc = byBudget[presets[p.key]];
        if (sc && sc.used > 0) {
          var label = Math.round(sc.queries) + chatT('chat_size_q');
          if (sc.errors >= 0.5) label += ' · ' + Math.round(sc.errors) + chatT('chat_size_err');
          var rate = sc.queries > 0 ? (1 - sc.errors / sc.queries) : 1;
          label += ' · ' + chatT('chat_size_score') + (rate * 10).toFixed(1);
          // The chat thread is laid out LTR (.chat-modal), so without an explicit
          // dir this mixed number+label badge renders LTR and the query count is
          // flung to the wrong side in Persian. Pin it to the language direction.
          score = '<span class="chat-score" dir="' + chatDir() + '">' + esc(label) + '</span>';
        }
      }
      items += '<button class="chat-sheet-item' + (on ? ' chat-sheet-on' : '') +
        '" onclick="chatSetCellSize(\'' + p.key + '\')">' + (on ? icon('tickSingle') + ' ' : '') +
        '<span><b>' + esc(p.label) + '</b>' + score + '<br><span class="chat-sheet-hint">' + esc(p.hint) + '</span></span></button>';
    });
    items += '<button class="chat-sheet-item chat-sheet-cancel" onclick="chatCloseMenu()">' + esc(chatT('cancel')) + '</button>';
    sheet.innerHTML = '<div class="chat-sheet">' + items + '</div>';
    document.getElementById('chatModal').appendChild(sheet);
  }).catch(function () { showToast(chatT('chat_err_generic')); });
}

// chatBudgetScores averages the per-server auto stats into one budget ->
// {queries, errors, cost, used} map for display.
function chatBudgetScores(scores) {
  var acc = {}; // budget -> {q, e, c, n, used}
  Object.keys(scores || {}).forEach(function (k) {
    (scores[k] || []).forEach(function (a) {
      if (!a.used) return;
      var x = acc[a.budget] || (acc[a.budget] = { q: 0, e: 0, c: 0, n: 0, used: 0 });
      x.q += a.queries; x.e += a.errors; x.c += (a.cost || 0); x.n++; x.used += a.used;
    });
  });
  var out = {};
  Object.keys(acc).forEach(function (b) {
    out[b] = { queries: acc[b].q / acc[b].n, errors: acc[b].e / acc[b].n, cost: acc[b].c / acc[b].n, used: acc[b].used };
  });
  return out;
}

function chatSetCellSize(mode) {
  fetch('/api/chat/settings', { method: 'POST', body: JSON.stringify({ mode: mode }) })
    .then(function (r) { return r.json(); })
    .then(function () {
      showToast(chatT('chat_query_size_saved'));
      // Re-open so the new selection (and, for auto, the scores) show.
      chatCloseMenu();
      chatCellSizeSheet();
    })
    .catch(function () { showToast(chatT('chat_err_generic')); });
}

async function chatRenderThread() {
  var addr = chatState.peer;
  if (!addr) return;
  document.documentElement.classList.add('chat-thread');
  // A popup is open (rename prompt, menu, …): don't rebuild the thread — defer.
  if (chatOverlayOpen()) { chatState.renderPending = true; return; }
  chatState.renderPending = false;
  // Preserve the compose draft, caret and focus across re-renders: the 15s
  // foreground poll and the ✓✓ status refresh both re-render the thread, and
  // rebuilding innerHTML would otherwise wipe whatever the user is typing.
  var prevInput = document.getElementById('chatInput');
  // Draft is read from chatState (single source of truth), NOT the DOM: a
  // background render (poll / status) that started before a send and resolves
  // after it would otherwise restore the just-sent text, leaving it stuck in
  // the box. chatState.draft is cleared synchronously on send, so any
  // out-of-order render now restores the correct (empty) value.
  var draft = chatState.draft || '';
  var hadFocus = !!prevInput && document.activeElement === prevInput;
  var selStart = prevInput ? prevInput.selectionStart : null;
  var selEnd = prevInput ? prevInput.selectionEnd : null;
  // Don't yank the user to the bottom on a background refresh if they've
  // scrolled up to read history.
  var prevBody = document.getElementById('chatMsgsBody');
  var keepScroll = prevBody && (prevBody.scrollHeight - prevBody.scrollTop - prevBody.clientHeight > 40);
  var prevScroll = prevBody ? prevBody.scrollTop : 0;
  var firstRender = prevInput == null;
  var data = null;
  try {
    var r = await fetch('/api/chat/messages?peer=' + encodeURIComponent(addr) + '&markRead=1');
    if (r.ok) data = await r.json();
  } catch (e) { }
  // A failed/non-OK fetch (a transient local-server hiccup — e.g. right after a
  // config switch or app resume) must NOT blank an already-open conversation:
  // rendering an empty list here made the user's messages appear to vanish. Keep
  // the current view and let the next tick retry; only the very first render has
  // nothing to preserve and may fall through to the empty state.
  if (data == null) {
    if (!firstRender) return;
    data = { msgs: [] };
  }
  var msgs = data.msgs || []; // server may omit / null for an empty thread
  // Per-server ✓/✓✓ high-water seq (seq numbering is per server). Merge the
  // persisted thread counters (fresh: a just-committed send is bumped here)
  // with the live probe, taking the max per server. Merging — not "probe ||
  // persisted" — is what keeps a sent message's ✓ from regressing to 🕓 when a
  // stale probe (e.g. taken on another server mid-switch) lacks that seq.
  var st = chatMergeStatus({ accepted: data.accepted, delivered: data.delivered, emojis: data.emojis },
    chatState.peerStatus[addr]);
  var nm = data.name || chatName(addr);
  var threadServer = data.server || chatState.pendingServer[addr] || '';
  chatState.curServer = threadServer; // server this conversation currently sends via
  // hasOut: have we replied yet? (gates the stranger warning)
  var hasOut = false;
  for (var mi0 = 0; mi0 < msgs.length; mi0++) {
    if (msgs[mi0].dir === 'out') { hasOut = true; break; }
  }

  var html = '';
  html += '<div class="chat-topbar">';
  html += '<button class="chat-back" onclick="chatBackToList()" aria-label="back">' + icon('arrowLeft') + '</button>';
  // Avatar + tappable name → contact panel. Single line so it centers with the avatar.
  html += '<div class="chat-topbar-title" onclick="chatContactInfo()" style="cursor:pointer">' +
    chatAvatarHTML(nm, 36) +
    '<div class="chat-topbar-name-wrap"><div class="chat-peer-name">' + esc(nm) + '</div></div></div>';
  // Circular next-poll timer lives in the topbar; the rate-limit/quota moved
  // into the contact panel (⋮) so the topbar isn't cramped next to the name.
  if (chatAvailable()) html += chatTimerBtnHTML();
  html += '<button id="chatRefreshBtn" class="chat-icon-btn' + (chatState.polling ? ' spinning' : '') + '" ' +
    (chatState.polling ? 'disabled ' : '') + 'title="' + escAttr(chatT('chat_refresh')) + '"' + CHAT_KEEP_FOCUS + ' onclick="chatThreadRefresh()">' + icon('refresh') + '</button>';
  // ⋮ opens the SAME merged contact panel as tapping the name.
  html += '<button class="chat-icon-btn" title="' + escAttr(chatT('chat_menu')) +
    '" aria-label="' + escAttr(chatT('chat_menu')) + '" onclick="chatContactInfo()">' + icon('more') + '</button>';
  html += '</div>';
  // Thin receive-progress strip under the floating topbar (countdown moved to
  // the circular timer above; this only fills during a receive).
  html += '<div class="chat-nextpoll-bar">' +
    '<div class="chat-progress chat-progress-inline" id="chatRecvProgress"><div class="chat-progress-fill" id="chatRecvProgressFill"></div></div>' +
    '<span class="chat-progress-label" id="chatRecvProgressLabel"></span></div>';

  html += '<div class="chat-body-wrap">';
  html += '<div class="chat-body" id="chatMsgsBody">';
  // E2E safety code at the top; tap to expand the explainer.
  if (st.emojis) {
    html += '<div class="chat-sysmsg chat-safety-sep' + (chatState.safetyOpen ? ' open' : '') +
      '" onclick="chatToggleSafety(this)">' +
      '<span dir="auto">' + esc(st.emojis.join(' ')) + ' · ' + esc(chatT('chat_safety_code')) +
      '<span class="chat-safety-i" aria-hidden="true">' + icon('info') + '</span></span>' +
      '<div class="chat-safety-explain" dir="auto">' + esc(chatT('chat_safety_explain')) + '</div></div>';
  }
  if (!msgs.length) {
    html += '<div class="chat-empty">' + esc(chatT('chat_no_messages')) + '</div>';
  }
  // Stranger warning: this thread was started by the peer and the user has
  // neither replied nor named the contact — anyone holding the address can
  // write, so warn before the user trusts links or shares anything private.
  // Replying or naming the contact clears it (the user has decided who it is).
  if (msgs.length && !hasOut && !data.name && !chatState.contacts[addr]) {
    html += '<div class="chat-banner chat-banner-warn" dir="auto">' + esc(chatT('chat_stranger_warn')) + '</div>';
  }
  // peerServer: the latest server the peer messaged from (≠ threadServer ⇒ moved).
  var peerServer = '', peerTS = 0;
  for (var pj = msgs.length - 1; pj >= 0; pj--) {
    if (msgs[pj].dir === 'in' && msgs[pj].server) { peerServer = msgs[pj].server; peerTS = msgs[pj].ts || 0; break; }
  }
  var lastDate = '', lastServer = '', pending = {};
  msgs.forEach(function (m) {
    // Day separator: insert a centered date chip whenever the day changes, so a
    // multi-day conversation reads correctly (Jalali for fa, Gregorian for en).
    var dateStr = chatFmtDate(m.ts);
    if (dateStr && dateStr !== lastDate) {
      html += '<div class="chat-date-sep"><span dir="auto">' + esc(dateStr) + '</span></div>';
      lastDate = dateStr;
    }
    // Separator where the conversation's server changed.
    if (m.server && lastServer && m.server !== lastServer) {
      html += '<div class="chat-sysmsg"><span dir="auto">' +
        chatFmtBidi(chatT('chat_server_changed'), { s: chatServerLabel(m.server) }) + '</span></div>';
    }
    if (m.server) lastServer = m.server;
    var ticks = '';
    if (m.dir === 'out') {
      var sk = m.server || threadServer || '';
      var acc = (st.accepted && st.accepted[sk]) || 0;
      var del = (st.delivered && st.delivered[sk]) || 0;
      // ticks is HTML (inline SVG icons), inserted unescaped.
      if (del >= m.seq) ticks = ' ' + icon('tickDouble');
      else if (acc >= m.seq) ticks = ' ' + icon('tickSingle');
      else ticks = ' ' + icon('clock');
      // Track servers with a sent-but-undelivered message so the status refresh
      // probes them too — otherwise ✓✓ never lands after switching servers.
      if (sk && del < m.seq) pending[sk] = true;
    }
    // Bubble actions live in a sheet (chatMsgMenu) — opened by tapping the
    // bubble or the ⋮ pinned at the block's bottom edge, outside the box.
    html += '<div class="chat-msg ' + (m.dir === 'in' ? 'in' : 'out') + '" dir="auto" onclick="chatMsgMenu(this)">' +
      '<span class="chat-msg-text">' + esc(m.text).replace(/🇮🇷/g, '<img src="/static/lion-sun.svg" alt="\u{1F981}☀️" style="height:1.1em;vertical-align:middle">') + '</span>' +
      '<span class="chat-msg-meta">' +
      '<span class="chat-msg-time">' + chatFmtTime(m.ts) + ticks + '</span></span>' +
      '<button type="button" class="chat-msg-more" title="" aria-label="' + escAttr(chatT('chat_msg_actions')) +
      '" onclick="event.stopPropagation();chatMsgMenu(this.parentNode)">' + icon('more') + '</button></div>' +
      '<div class="chat-clearfix"></div>';
  });
  chatState.pendingServers[addr] = Object.keys(pending);
  // Peer moved to a server we don't send through, after our own last switch →
  // offer to follow (server-verified).
  if (peerServer && threadServer && peerServer !== threadServer && peerTS > (data.serverSetAt || 0)) {
    var plabel = chatServerLabel(peerServer);
    html += '<div class="chat-switch-banner" dir="' + chatDir() + '">' +
      '<div class="chat-switch-text">' +
      chatFmtBidi(chatT('chat_peer_switched'), { n: nm, s: plabel }) +
      '</div><button class="chat-btn-primary chat-switch-btn" onclick="chatSwitchServer(\'' +
      jsStr(peerServer) + '\')">' +
      chatFmtBidi(chatT('chat_switch_reply'), { s: plabel }) + '</button></div>';
  }
  html += '</div>'; // .chat-body
  html += '<button id="chatScrollDown" class="chat-scroll-down hidden" aria-label="' +
    escAttr(chatT('chat_scroll_bottom')) + '" title="' + escAttr(chatT('chat_scroll_bottom')) +
    '" onclick="chatScrollToBottom()">' + icon('arrowDown') +
    '<span class="chat-scroll-down-badge" id="chatScrollDownBadge"></span></button>';
  html += '</div>'; // .chat-body-wrap

  html += '<div class="chat-footer">';
  html += '<div class="chat-send-error" id="chatSendError"></div>';
  html += '<div class="chat-progress-wrap"><div class="chat-progress" id="chatSendProgress"><div class="chat-progress-fill" id="chatSendProgressFill"></div></div><span class="chat-progress-label" id="chatSendProgressLabel"></span></div>';
  // Compose row: three SEPARATE pills — quota ring (start) · text pill · send
  // circle. The quota ring shows remaining sends; tap it for the full detail.
  html += '<div class="chat-input-row">' +
    chatQuotaCircleHTML() +
    '<textarea id="chatInput" dir="auto" placeholder="' + escAttr(chatT('chat_send_ph')) + '" rows="1" oninput="chatInputResize()" onkeydown="chatInputKey(event)"></textarea>' +
    '<button class="chat-send-btn" id="chatSendBtn"' + CHAT_KEEP_FOCUS + ' onclick="sendChatMessage()" aria-label="' + escAttr(chatT('chat_send')) + '">' + icon('send') + '</button></div>';
  html += '<div class="chat-charcount" id="chatCharCount"></div>';
  html += '</div>';

  // A popup may have opened during the await above — don't wipe it.
  if (chatOverlayOpen()) { chatState.renderPending = true; return; }
  var modal = document.getElementById('chatPane'); // thread renders into the content pane
  var liveFooter = hadFocus && !firstRender ? modal.querySelector('.chat-footer') : null;
  if (liveFooter) {
    var tmp = document.createElement('div');
    tmp.innerHTML = html;
    var newTopbar = tmp.querySelector('.chat-topbar');
    var newPollBar = tmp.querySelector('.chat-nextpoll-bar');
    var newBodyWrap = tmp.querySelector('.chat-body-wrap');
    var oldTopbar = modal.querySelector('.chat-topbar');
    var oldPollBar = modal.querySelector('.chat-nextpoll-bar');
    var oldBodyWrap = modal.querySelector('.chat-body-wrap');
    if (oldTopbar && newTopbar) modal.replaceChild(newTopbar, oldTopbar);
    if (oldPollBar && newPollBar) modal.replaceChild(newPollBar, oldPollBar);
    if (oldBodyWrap && newBodyWrap) modal.replaceChild(newBodyWrap, oldBodyWrap);
    // The quota ring lives in the kept footer but is not the text box, so it
    // can follow the count without disturbing what is being typed.
    var newRing = tmp.querySelector('.chat-quota-btn');
    var oldRing = modal.querySelector('.chat-quota-btn');
    var inputRow = liveFooter.querySelector('.chat-input-row');
    if (oldRing && newRing) oldRing.replaceWith(newRing);
    else if (oldRing) oldRing.remove();
    else if (newRing && inputRow) inputRow.insertBefore(newRing, inputRow.firstChild);
  } else {
    modal.innerHTML = html;
  }
  var chatWrap = document.querySelector('.chat-body-wrap');
  if (chatWrap) {
    if (typeof getBgPattern === 'function') {
      var pat = getBgPattern();
      if (pat) chatWrap.setAttribute('data-bg-pattern', pat);
    }
    var ca = document.querySelector('.chat-area');
    if (ca && ca.style.backgroundImage) {
      chatWrap.style.backgroundImage = ca.style.backgroundImage;
      chatWrap.style.backgroundSize = 'cover';
      chatWrap.style.backgroundPosition = 'center';
      chatWrap.style.backgroundRepeat = 'no-repeat';
    }
  }
  var body = document.getElementById('chatMsgsBody');
  if (body) body.scrollTop = keepScroll ? prevScroll : body.scrollHeight;
  // ↓ button + unread-while-scrolled badge. Reset the seen-count when the open
  // peer changes; if we're at the bottom now, everything is seen.
  if (chatState.scrollPeer !== addr) { chatState.scrollPeer = addr; chatState.seenCount = msgs.length; }
  chatState.msgCount = msgs.length;
  if (!keepScroll) chatState.seenCount = msgs.length;
  if (body) body.onscroll = chatUpdateScrollBtn;
  chatUpdateScrollBtn();
  // Restore the in-progress draft + caret before resizing the input.
  var inp = document.getElementById('chatInput');
  if (inp && !liveFooter) {
    inp.value = draft;
    if (draft && selStart != null) { try { inp.setSelectionRange(selStart, selEnd); } catch (e) { } }
  }
  // A send in flight survives this re-render: the compose row was just rebuilt,
  // so re-disable the input (chatInputResize already disables the button while
  // sending) and repaint the live send-progress bar this render destroyed.
  if (chatState.sending) {
    if (chatState.sendProg && chatState.sendProg.done >= 1) {
      chatShowProgress('chatSendProgress', chatState.sendProg.done, chatState.sendProg.total, '↑');
    } else if (chatState.connecting) {
      chatShowProgress('chatSendProgress', 0, 1, '↑'); // keep the "connecting…" bar
    }
  }
  if (chatState.sendError) {
    var errEl = document.getElementById('chatSendError');
    if (errEl) { errEl.textContent = chatState.sendError; errEl.style.display = 'block'; }
  }
  chatInputResize();
  chatUpdateCountdown();
  // Auto-focus the compose box on first open only on DESKTOP. On mobile that
  // popped the keyboard immediately, which shrank the viewport and pushed the
  // newest messages (just scrolled into view) back under the keyboard. Like
  // Telegram, mobile waits for the user to tap the box. Focus is still preserved
  // across the periodic re-renders (hadFocus) so typing isn't interrupted.
  var isMobile = (typeof mobileQuery !== 'undefined' && mobileQuery.matches);
  var allowFirstFocus = firstRender && !isMobile;
  if (inp && !liveFooter && (allowFirstFocus || (hadFocus && !keepScroll))) {
    try { inp.focus({ preventScroll: true }); } catch (e) { inp.focus(); }
  }
  if (inp && !liveFooter) inp.addEventListener('blur', chatFlushPendingRender);
}

// chatUpdateScrollBtn shows the floating ↓ button while the user is scrolled up,
// with a badge counting messages that arrived since they were last at the
// bottom. Runs on every body scroll and after each thread render.
function chatUpdateScrollBtn() {
  var body = document.getElementById('chatMsgsBody');
  var btn = document.getElementById('chatScrollDown');
  if (!body || !btn) return;
  var atBottom = body.scrollHeight - body.scrollTop - body.clientHeight <= 40;
  if (atBottom) {
    chatState.seenCount = chatState.msgCount;
    if (chatState.renderPending) chatFlushPendingRender();
  }
  btn.classList.toggle('hidden', atBottom);
  var unseen = Math.max(0, chatState.msgCount - chatState.seenCount);
  var badge = document.getElementById('chatScrollDownBadge');
  if (badge) {
    badge.textContent = unseen > 0 ? (unseen > 99 ? '99+' : unseen) : '';
    badge.classList.toggle('on', unseen > 0);
  }
}

// chatScrollToBottom jumps to the latest message (the ↓ button and post-send).
function chatScrollToBottom() {
  var body = document.getElementById('chatMsgsBody');
  if (!body) return;
  body.scrollTop = body.scrollHeight;
  chatState.seenCount = chatState.msgCount;
  chatUpdateScrollBtn();
}

// chatPullRefresh runs the same poll as the header ↻ button, throttled to one
// per CHAT_REFRESH_MIN_MS. The view's own refresh (chatPollNow/chatThreadRefresh)
// keeps its in-flight guard + spinner, so this only adds the time throttle.
// Returns whether a refresh actually started (so the gesture can snap back).
function chatPullRefresh() {
  if (Date.now() - (chatState.lastRefreshAt || 0) < CHAT_REFRESH_MIN_MS) return false;
  if (chatState.polling) return false;
  chatState.lastRefreshAt = Date.now();
  if (chatState.view === 'thread' && chatState.peer) chatThreadRefresh();
  else chatPollNow();
  return true;
}

// chatPullIndicator is the spinner that follows the finger during a pull. It
// lives on #chatModal (re-created after each render, which replaces innerHTML).
function chatPullIndicator() {
  var ind = document.getElementById('chatPullRefresh');
  if (!ind) {
    var modal = document.getElementById('chatModal');
    if (!modal) return null;
    ind = document.createElement('div');
    ind.id = 'chatPullRefresh';
    ind.className = 'chat-pull-refresh';
    ind.innerHTML = icon('refresh');
    modal.appendChild(ind);
  }
  return ind;
}

function chatPullSet(ind, pull) {
  if (!ind) return;
  ind.style.opacity = String(Math.min(1, pull / CHAT_PULL_TRIGGER));
  ind.style.transform = 'translateX(-50%) translateY(' + (pull - CHAT_PULL_TRIGGER) +
    'px) rotate(' + Math.round(pull * 2.5) + 'deg)';
  ind.classList.toggle('ready', pull >= CHAT_PULL_TRIGGER);
}

function chatPullReset(ind) {
  if (!ind) return;
  ind.classList.remove('ready', 'spinning');
  ind.style.opacity = '0';
  ind.style.transform = 'translateX(-50%) translateY(-' + CHAT_PULL_TRIGGER + 'px)';
}

// chatBindPullRefresh wires the touch drag-down gesture on a scroll container.
// It only engages when the list is already at the top, so a normal upward
// scroll is never hijacked. Passive listeners keep scrolling smooth — we never
// preventDefault (at scrollTop 0 there's nothing to scroll up into anyway).
function chatBindPullRefresh(el) {
  if (!el || el.dataset.pullBound) return;
  el.dataset.pullBound = '1';
  var startY = 0, dist = 0, pulling = false;
  el.addEventListener('touchstart', function (e) {
    pulling = el.scrollTop <= 0 && e.touches.length === 1;
    if (pulling) { startY = e.touches[0].clientY; dist = 0; }
  }, { passive: true });
  el.addEventListener('touchmove', function (e) {
    if (!pulling) return;
    dist = e.touches[0].clientY - startY;
    if (dist <= 0 || el.scrollTop > 0) { pulling = false; chatPullReset(chatPullIndicator()); return; }
    chatPullSet(chatPullIndicator(), Math.min(dist, CHAT_PULL_TRIGGER * 1.6));
  }, { passive: true });
  var end = function () {
    if (!pulling) return;
    pulling = false;
    var ind = chatPullIndicator();
    if (dist >= CHAT_PULL_TRIGGER && chatPullRefresh()) {
      if (ind) { ind.classList.add('spinning'); ind.style.opacity = '1'; ind.style.transform = 'translateX(-50%) translateY(6px)'; }
      setTimeout(function () { chatPullReset(ind); }, 600);
    } else {
      chatPullReset(ind);
    }
  };
  el.addEventListener('touchend', end, { passive: true });
  el.addEventListener('touchcancel', end, { passive: true });
}

// chatMaxBytes is the server's per-message byte cap (from ChatInfo), or a safe
// default until availability is known.
function chatMaxBytes() {
  var lim = chatCurLimits();
  return (lim && lim.maxMsgBytes) ? lim.maxMsgBytes : 500;
}

function chatByteLen(s) {
  try { return new TextEncoder().encode(s).length; } catch (e) { return s.length; }
}

// chatInputResize grows the textarea and updates the byte counter, disabling
// send when the message exceeds the server's limit (Persian chars are 2 bytes,
// so we count bytes, not characters).
function chatInputResize() {
  var inp = document.getElementById('chatInput');
  var cc = document.getElementById('chatCharCount');
  var btn = document.getElementById('chatSendBtn');
  if (!inp) return;
  chatState.draft = inp.value; // keep the source-of-truth draft in sync as the user types
  inp.style.height = 'auto';
  inp.style.height = Math.min(inp.scrollHeight, 120) + 'px';
  var max = chatMaxBytes();
  var used = chatByteLen(inp.value || '');
  var remaining = max - used;
  var over = remaining < 0;
  if (cc) {
    // Twitter-style remaining counter: appears as you approach the limit and
    // shows how many more bytes fit (negative + red when over).
    if (used >= max * 0.6) {
      cc.textContent = remaining + '';
      cc.className = 'chat-charcount show' + (over ? ' over' : (remaining <= max * 0.1 ? ' near' : ''));
    } else {
      cc.className = 'chat-charcount';
      cc.textContent = '';
    }
  }
  if (btn) btn.disabled = over || used === 0 || chatState.sending;
}

function chatInputKey(e) {
  if (e.key === 'Enter' && !e.shiftKey && !chatIsMobile()) {
    e.preventDefault();
    sendChatMessage();
  }
}

// chatShowList switches the thread view back to the conversation list. It does
// NOT touch history — chatBackToList and the popstate handler manage that.
function chatShowList() {
  chatState.view = 'list';
  chatState.peer = null;
  // Mobile: swap back to the sidebar (content pane shows the placeholder).
  var app = document.getElementById('app'); if (app) app.classList.remove('chat-open');
  chatRenderContentEmpty();
  chatRenderList();
  chatLoadThreads();
}

// chatBackToList is the in-thread back arrow. Route through history so the
// button and the hardware back key take the same path (popstate → chatShowList).
function chatBackToList() {
  if (chatThreadPushed) { try { history.back(); } catch (e) { } return; }
  chatShowList();
}

// chatContactInfo is the contact panel: rename, copy account ID, change server.
function chatContactInfo() {
  var addr = chatState.peer;
  if (!addr) return;
  chatCloseMenu();
  var nm = chatState.contacts[addr] || chatName(addr);
  var st = chatState.peerStatus[addr] || {};
  var sheet = document.createElement('div');
  sheet.className = 'chat-sheet-overlay';
  sheet.id = 'chatSheet';
  sheet.onclick = function (e) { if (e.target === sheet) chatCloseMenu(); };
  var h = '<div class="chat-sheet">' +
    '<div class="chat-info-head">' + chatAvatarHTML(nm, 56) +
    '<div class="chat-sheet-title">' + esc(nm) + '</div></div>';
  h += '<div class="chat-info-field"><div class="chat-info-label">' + esc(chatT('chat_account_id')) + '</div>' +
    '<div class="chat-info-value chat-info-id" dir="ltr">' + esc(addr) + '</div>' +
    '<button class="chat-btn-soft" onclick="chatCopy(\'' + jsStr(addr) + '\')">' + esc(chatT('chat_copy')) + '</button></div>';
  // Safety code, next to the address — tap to expand the explainer.
  if (st.emojis) {
    h += '<div class="chat-info-field chat-info-safety-wrap" onclick="this.classList.toggle(\'open\')">' +
      '<div class="chat-info-label">' + esc(chatT('chat_safety_code')) +
      '<span class="chat-safety-i" aria-hidden="true">' + icon('info') + '</span></div>' +
      '<div class="chat-info-value chat-info-safety">' + esc(st.emojis.join(' ')) + '</div>' +
      '<div class="chat-safety-explain" dir="auto">' + esc(chatT('chat_safety_explain')) + '</div></div>';
  }
  h += '<div class="chat-info-field"><div class="chat-info-label">' + esc(chatT('chat_info_server')) + '</div>' +
    '<div class="chat-info-value" dir="auto"><bdi>' + esc(chatServerLabel(chatState.curServer)) + '</bdi></div>' +
    '<button class="chat-btn-soft" onclick="chatCloseMenu();chatSwitchServerSheet()">' + esc(chatT('chat_change_server')) + '</button></div>';
  // Rate-limit / quota — moved here out of the cramped topbar.
  var qi = chatQuotaIcon();
  if (qi) {
    h += '<div class="chat-info-field"><div class="chat-info-label">' + esc(chatT('chat_quota_label')) + '</div>' +
      '<div class="chat-info-value' + (qi.low ? ' chat-info-quota-low' : '') + '" dir="auto">' + esc(qi.detail) + '</div></div>';
  }
  h += '<button class="chat-sheet-item" onclick="chatContactRename()">' + icon('edit') + ' ' + esc(chatT('chat_rename')) + '</button>';
  // Merged in from the old thread ⋮ menu: clear / delete the conversation.
  h += '<button class="chat-sheet-item" onclick="chatClearMessages(\'' + jsStr(addr) + '\')">' + icon('eraser') + ' ' + esc(chatT('chat_clear')) + '</button>';
  h += '<button class="chat-sheet-item chat-sheet-danger" onclick="chatDelete(\'' + jsStr(addr) + '\')">' + icon('delete') + ' ' + esc(chatT('chat_delete')) + '</button>';
  h += '<button class="chat-sheet-item chat-sheet-cancel" onclick="chatCloseMenu()">' + esc(chatT('close')) + '</button></div>';
  sheet.innerHTML = h;
  document.getElementById('chatModal').appendChild(sheet);
}

function chatContactRename() {
  chatCloseMenu();
  chatRenamePeer();
}

// chatSwitchServerSheet moves the conversation to another server (verified
// server-side). reasonHtml, if given, is shown as a top warning (bidi-safe HTML).
function chatSwitchServerSheet(reasonHtml) {
  var addr = chatState.peer;
  if (!addr) return;
  chatCloseMenu();
  var servers = chatUsableServers();
  var sheet = document.createElement('div');
  sheet.className = 'chat-sheet-overlay';
  sheet.id = 'chatSheet';
  sheet.onclick = function (e) { if (e.target === sheet) chatCloseMenu(); };
  var items = '<div class="chat-sheet-title">' + esc(chatT('chat_switch_server_title')) + '</div>' +
    (reasonHtml
      ? '<div class="chat-sheet-warn" dir="' + chatDir() + '">' + reasonHtml + '</div>'
      : '<div class="chat-sheet-hint">' + esc(chatT('chat_switch_server_hint')) + '</div>');
  if (!servers.length) {
    items += '<div class="chat-sheet-hint">' + esc(chatT('chat_checking')) + '</div>';
  }
  servers.forEach(function (sv) {
    var label = (sv.name ? sv.name + ' — ' : '') + sv.domain;
    var cur = sv.key === chatState.curServer;
    items += '<button class="chat-sheet-item"' + (cur ? ' disabled' : '') +
      ' onclick="chatSwitchServer(\'' + jsStr(sv.key) + '\')">' + esc(label) +
      (cur ? ' · <span class="chat-server-current">' + esc(chatT('chat_server_current')) + '</span>' : '') + '</button>';
  });
  items += '<button class="chat-sheet-item chat-sheet-cancel" onclick="chatCloseMenu()">' + esc(chatT('cancel')) + '</button>';
  sheet.innerHTML = '<div class="chat-sheet">' + items + '</div>';
  document.getElementById('chatModal').appendChild(sheet);
}

// chatSwitchServer rebinds the conversation to serverKey (backend verifies the
// peer is registered there).
async function chatSwitchServer(serverKey) {
  var addr = chatState.peer;
  if (!addr) return;
  chatCloseMenu();
  showToast(chatT('chat_switch_checking'));
  try {
    var r = await fetch('/api/chat/setserver', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ peer: addr, server: serverKey })
    });
    var d = await r.json();
    if (d.ok) {
      chatState.curServer = d.server;
      delete chatState.pendingServer[addr];
      showToast(chatT('chat_switched_server'));
      if (chatState.view === 'thread' && chatState.peer === addr) chatRenderThread();
    } else {
      var emsg = chatT(d.error || 'chat_err_generic');
      showToast(emsg);
      // Peer not on the chosen server → reopen the picker with the warning.
      if (d.error === 'chat_err_peer_not_on_server') chatSwitchServerSheet(esc(emsg));
    }
  } catch (e) {
    showToast(chatT('chat_err_generic'));
  }
}

function chatRenamePeer() {
  var addr = chatState.peer;
  if (!addr) return;
  chatPrompt(chatT('chat_contact_name'), chatState.contacts[addr] || '', '', function (name) {
    name = (name || '').trim();
    fetch('/api/chat/contacts', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ addr: addr, name: name }) })
      .then(function () {
        if (name) chatState.contacts[addr] = name; else delete chatState.contacts[addr];
        chatRenderThread();
      });
  });
}

async function chatFetchPeerStatus(addr, server) {
  try {
    var url = '/api/chat/peer-status?peer=' + encodeURIComponent(addr);
    if (server) url += '&server=' + encodeURIComponent(server);
    var r = await fetch(url);
    var d = await r.json();
    if (d.ok) {
      // The response carries the full per-server map; merge (max) so probing
      // one server never drops another's already-known counters.
      var prev = chatState.peerStatus[addr];
      d.accepted = chatMaxMap(prev && prev.accepted, d.accepted);
      d.delivered = chatMaxMap(prev && prev.delivered, d.delivered);
      chatState.peerStatus[addr] = d;
      // live "N sends left this hour", for the server that was asked
      if (d.quota) chatState.quotaByServer[d.quota.server || server || chatLimitsKey()] = d.quota;
      if (chatState.view === 'thread' && chatState.peer === addr) {
        // Safety emojis arrived for the first time: inject directly into the
        // DOM so the user sees them immediately without a full re-render
        // (which the focus/scroll guard might defer).
        if (d.emojis && !(prev && prev.emojis) && !document.querySelector('.chat-safety-sep')) {
          var mb0 = document.getElementById('chatMsgsBody');
          if (mb0) {
            var sep = document.createElement('div');
            sep.className = 'chat-sysmsg chat-safety-sep';
            sep.onclick = function () { chatToggleSafety(sep); };
            sep.innerHTML = '<span dir="auto">' + esc(d.emojis.join(' ')) + ' · ' + esc(chatT('chat_safety_code')) +
              '<span class="chat-safety-i" aria-hidden="true">' + icon('info') + '</span></span>' +
              '<div class="chat-safety-explain" dir="auto">' + esc(chatT('chat_safety_explain')) + '</div>';
            mb0.insertBefore(sep, mb0.firstChild);
          }
        }
        // Safe unconditionally: chatRenderThread reuses the live footer while
        // the input has focus and keeps a scrolled-up position.
        chatRenderThread();
      }
    }
  } catch (e) { }
}

// chatRefreshStatus probes the conversation's bound server AND every other
// server it still has an undelivered message on, so ✓✓ lands even after the
// user switched the conversation to a different server mid-thread.
async function chatRefreshStatus(addr) {
  if (!addr) return;
  await chatFetchPeerStatus(addr, '');
  var pend = chatState.pendingServers[addr] || [];
  for (var i = 0; i < pend.length; i++) {
    if (pend[i] && pend[i] !== chatState.curServer) await chatFetchPeerStatus(addr, pend[i]);
  }
}

async function sendChatMessage() {
  if (chatState.sending) return;
  var inp = document.getElementById('chatInput');
  var btn = document.getElementById('chatSendBtn');
  var text = (inp.value || '').trim();
  if (!text || !chatState.peer) return;
  if (chatByteLen(text) > chatMaxBytes()) {
    showToast(chatT('chat_too_long').replace('{n}', chatMaxBytes()));
    return;
  }

  chatClearSendError();
  chatState.sending = true;
  chatState.sendProg = { done: 0, total: 1 };
  chatState.connecting = false;
  btn.disabled = true;
  clearTimeout(chatState.sendConnTimer);
  chatState.sendConnTimer = setTimeout(function () {
    if (chatState.sending && !chatState.sendProg) {
      chatState.connecting = true;
      chatShowHandshakeProgress(0);
    }
  }, 1500);

  var peerAddr = chatState.peer;
  try {
    var r = await fetch('/api/chat/send', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ peer: peerAddr, text: text, server: chatState.pendingServer[peerAddr] || '' })
    });
    var d = await r.json();
    if (d.ok) {
      chatState.sending = false;
      chatState.sendProg = null;
      if (d.remaining != null) {
        chatState.quotaByServer[d.server || chatState.pendingServer[peerAddr] || chatLimitsKey()] =
          { remaining: d.remaining, resetUnix: d.resetUnix };
      }
      delete chatState.pendingServer[peerAddr];
      chatBumpActivity();
      // First send to a NEW peer creates the thread server-side: reload so it
      // appears in the sidebar list.
      chatLoadThreads();
      // A send proves the server is reachable — probe availability if we don't
      // have it yet so the timer + quota (which need the server's limits) populate
      // without a manual page refresh.
      if (!chatAvailable()) chatCheckAvailability();
      var stillViewing = chatState.view === 'thread' && chatState.peer === peerAddr;
      if (stillViewing) {
        chatState.draft = '';
        var liveInp = document.getElementById('chatInput');
        if (liveInp) liveInp.value = '';
        await chatRenderThread();
        chatScrollToBottom();
        chatRefreshStatus(peerAddr);
        setTimeout(function () { if (chatState.peer === peerAddr) chatRefreshStatus(peerAddr); }, 3000);
        setTimeout(function () { if (chatState.peer === peerAddr) chatRefreshStatus(peerAddr); }, 8000);
      }
    } else {
      var msg = chatT(d.error || 'chat_err_generic');
      if (d.error === 'chat_err_rate_limited' && d.resetUnix) {
        msg += ' (' + chatFmtTime(d.resetUnix) + ')';
      }
      chatShowSendError(msg);
      if (d.error === 'chat_err_unknown_recipient' && chatUsableServers().length > 1) {
        chatSwitchServerSheet(chatFmtBidi(chatT('chat_switch_because_absent'),
          { n: chatState.contacts[peerAddr] || chatName(peerAddr), s: chatServerLabel(chatState.curServer) }));
      }
    }
  } catch (e) {
    chatShowSendError(chatT('chat_err_generic'));
  } finally {
    chatState.sending = false;
    chatState.sendProg = null;
    clearTimeout(chatState.sendConnTimer);
    chatState.connecting = false;
    chatInputResize(); // recompute the send button's enabled state
    chatHideProgress('chatSendProgress');
  }
}

// chatPollNow checks every server for new mail. Rapid taps are ignored while a
// poll is already running (the button shows a spinner + goes disabled), so the
// process never stacks.
async function chatPollNow() {
  if (chatState.polling) return;
  chatState.polling = true;
  chatSetRefreshBusy(true);
  chatShowPollProgress(0, 1);
  try {
    var r = await fetch('/api/chat/poll', { method: 'POST' });
    var d = await r.json();
    if (!d.ok) showToast(chatT(d.error || 'chat_err_generic'));
    else if (d.got > 0) showToast(chatT('chat_new_messages').replace('{n}', d.got));
  } catch (e) {
  } finally {
    // ALWAYS clear the spinner, even on a thrown error — otherwise the ⟳ button
    // spins forever. Drop polling BEFORE the re-render so the rebuilt button
    // isn't recreated with the 'spinning' class.
    chatState.polling = false;
    chatSetRefreshBusy(false);
    chatHidePollProgress();
    chatLoadThreads().then(function () {
      if (chatState.view === 'thread') chatRenderThread();
    });
  }
}

// chatSetRefreshBusy toggles the refresh button's spinner/disabled state (it
// has the same id in both the list and thread headers).
function chatSetRefreshBusy(busy) {
  var b = document.getElementById('chatRefreshBtn');
  if (!b) return;
  b.disabled = busy;
  b.classList.toggle('spinning', busy);
}

// ---- in-chat log viewer ----
// The sidebar #logPanel always receives the server's log lines over SSE, but
// it's hidden behind the full-screen chat modal. So show a log overlay inside
// the modal: seed it from #logPanel's history, then append new lines live.

function chatOpenLog() {
  chatCloseMenu();
  var ov = document.createElement('div');
  ov.className = 'chat-sheet-overlay chat-log-overlay';
  ov.id = 'chatLogOverlay';
  ov.onclick = function (e) { if (e.target === ov) chatCloseLog(); };
  ov.innerHTML = '<div class="chat-log-sheet">' +
    '<div class="chat-log-head"><span>' + esc(chatT('chat_log')) + '</span>' +
    '<button class="chat-icon-btn" aria-label="close" onclick="chatCloseLog()">' + icon('close') + '</button></div>' +
    '<div class="chat-log-body" id="chatLogBody"></div></div>';
  document.getElementById('chatModal').appendChild(ov);
  var body = document.getElementById('chatLogBody');
  var src = document.getElementById('logPanel');
  if (src && body) {
    Array.prototype.forEach.call(src.children, function (c) { body.appendChild(c.cloneNode(true)); });
    body.scrollTop = body.scrollHeight;
  }
}

function chatCloseLog() {
  var o = document.getElementById('chatLogOverlay');
  if (o) o.remove();
  chatFlushPendingRender();
}

// chatLogLive is called by addLogLine (log.js) for each new line so the in-chat
// log stays live while open.
function chatLogLive(line, cls) {
  var body = document.getElementById('chatLogBody');
  if (!body) return;
  var div = document.createElement('div');
  div.className = 'log-line ' + (cls || 'inf');
  div.textContent = line;
  body.appendChild(div);
  body.scrollTop = body.scrollHeight;
  while (body.children.length > 200) body.removeChild(body.firstChild);
}

// ---- progress + SSE glue ----
// arrow: '↑' for uploads (send), '↓' for downloads (receive). The label shows
// the block count so the user sees how many data blocks go up / come down.

function chatShowProgress(id, done, total, arrow) {
  var bar = document.getElementById(id);
  if (!bar) return;
  // Nothing to show, or already complete → hide instead of leaving a full bar
  // stuck on screen. An empty-inbox poll reports done==total==1, which would
  // otherwise pin "↓ 1/1" to the thread list.
  if (!(total > 0) || done >= total) { chatHideProgress(id); return; }
  bar.classList.add('active');
  var fill = document.getElementById(id + 'Fill');
  var label = document.getElementById(id + 'Label');
  bar.classList.remove('indeterminate');
  if (fill) fill.style.width = (Math.round(done * 100 / total) + '%');
  if (label) label.textContent = (arrow || '') + ' ' + done + '/' + total + ' ' + chatT('chat_blocks');
}

function chatHideProgress(id) {
  var bar = document.getElementById(id);
  if (!bar) return;
  bar.classList.remove('active');
  bar.classList.remove('indeterminate');
  var fill = document.getElementById(id + 'Fill');
  if (fill) fill.style.width = '0';
  var label = document.getElementById(id + 'Label');
  if (label) label.textContent = '';
}

function chatShowHandshakeProgress(done) {
  var bar = document.getElementById('chatSendProgress');
  if (!bar) return;
  bar.classList.add('active');
  bar.classList.add('indeterminate');
  var fill = document.getElementById('chatSendProgressFill');
  if (fill) fill.style.width = '100%';
  var label = document.getElementById('chatSendProgressLabel');
  // Isolate the count+arrow in an LTR bidi run so it stays a unit at the START
  // of the line; as bare text inside RTL (Persian) the number was reordered to
  // the end. chat_connecting is trusted i18n text (no HTML), done is a number.
  if (label) label.innerHTML = '<bdi dir="ltr">' + (done | 0) + '↑</bdi> ' + chatT('chat_connecting');
}

// Receive/poll progress can be shown in either view (list has chatPollProgress,
// thread has chatRecvProgress); update whichever is present.
function chatShowPollProgress(done, total) {
  chatShowProgress('chatPollProgress', done, total, '↓');
  chatShowProgress('chatRecvProgress', done, total, '↓');
}
function chatHidePollProgress() {
  chatHideProgress('chatPollProgress');
  chatHideProgress('chatRecvProgress');
}

// chatMergeServerResult folds one streamed availability verdict into the list
// and repaints whatever is on screen.
function chatMergeServerResult(sv, limits) {
  if (!chatState.avail) chatState.avail = { available: false, servers: [] };
  var list = chatState.avail.servers || (chatState.avail.servers = []);
  var row = null;
  list.forEach(function (s) { if (s.key === sv.key) row = s; });
  if (!row) { row = { key: sv.key, enabled: !!sv.enabled }; list.push(row); }
  // A row the user is toggling right now owns its own state; don't fight it.
  if (row.toggling) return;
  row.name = sv.name || row.name;
  row.domain = sv.domain || row.domain;
  // enabled is NOT taken from the event: it was read when the probe STARTED,
  // so a verdict landing after the user flipped the toggle would undo it.
  row.checking = !!sv.checking;
  if (sv.checking) return; // no verdict yet — leave the last one standing
  row.available = !!sv.available;
  row.reason = sv.reason || '';
  if (sv.limits || limits) row.limits = sv.limits || limits;
  chatState.avail.available = chatUsableServers().length > 0;
  if (!chatState.open) return;
  if (document.querySelector('.chat-server-sheet')) chatServerSheet();
  if (chatState.view === 'list') chatRenderList();
}

// Called by sse.js on "chat" events.
function chatOnSSE(data) {
  if (!data || typeof data !== 'object') return;
  if (data.type === 'nextpoll') {
    // Backend announced when its next poll lands — drive the countdown ring from
    // its real schedule (handled even when chat is closed so the value is fresh
    // on open).
    if (data.ms > 0) {
      chatState.pollIntervalMs = data.ms;
      chatState.nextPollAt = Date.now() + data.ms;
      if (chatState.open) chatUpdateCountdown();
    }
    // ✓✓ needs its own round trip, and the peer fetches our message on one of
    // ITS poll cycles — later than the post-send probes (0/3/8s). Ride the
    // backend's cadence rather than add a timer, and only while something is
    // actually outstanding, so an idle thread costs no query.
    if (chatState.open && chatState.view === 'thread' && chatState.peer &&
      (chatState.pendingServers[chatState.peer] || []).length > 0) {
      chatRefreshStatus(chatState.peer);
    }
    return;
  }
  if (data.type === 'progress') {
    if (data.op === 'handshake') {
      clearTimeout(chatState.sendConnTimer);
      chatState.connecting = false;
      chatShowHandshakeProgress(data.done);
    }
    else if (data.op === 'send') {
      chatState.sendProg = { done: data.done, total: data.total };
      clearTimeout(chatState.sendConnTimer);
      chatState.connecting = false;
      chatShowProgress('chatSendProgress', data.done, data.total, '↑');
    }
    else if (data.op === 'poll') chatShowPollProgress(data.done, data.total);
    return;
  }
  if (data.type === 'availserver' && data.server && data.server.key) {
    // The backend answers the list before every config has been probed, then
    // pushes each verdict as it lands — so a config that replies in a second
    // shows up at once and a slow one is still never abandoned.
    chatMergeServerResult(data.server, data.limits);
    return;
  }
  if (data.type === 'inboxstored') {
    // Messages are stored but not yet acked — render them now so the UI doesn't
    // wait on the ack round trip. Notification is left to the 'inbox' event so
    // it isn't doubled.
    chatBumpActivity();
    chatLoadThreads().then(function () {
      if (chatState.open && chatState.view === 'thread') chatRenderThread();
    });
    return;
  }
  if (data.type === 'inbox') {
    if (data.got > 0) { chatBumpActivity(); chatNotifyNew(data.got); }
    chatLoadThreads().then(function () {
      if (chatState.open && chatState.view === 'thread') chatRenderThread();
    });
  }
}
