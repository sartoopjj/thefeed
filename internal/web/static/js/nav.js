// ===== NAV =====
// Telegram-2026 redesign navigation layer. It does NOT replace any screen —
// it drives the existing, already-wired screens (Feed list, Messenger, Scanner,
// Settings) and the Mirror (telemirror) folder. Loaded last so every open*/
// close* function it wraps already exists.

// Scanner + Bank are merged into one 'resolver' section (bank-centric, with the
// scanner reachable from inside it).
var NAV_TABS = ['feed', 'mirror', 'chat', 'tel', 'resolver', 'settings'];
var NAV_TAB_ID = {
  feed: 'navTabFeed', mirror: 'navTabMirror', chat: 'navTabChat',
  tel: 'navTabTel', resolver: 'navTabResolver', settings: 'navTabSettings'
};

function setActiveTab(tab) {
  NAV_TABS.forEach(function (k) {
    var el = document.getElementById(NAV_TAB_ID[k]);
    if (!el) return;
    var on = (k === tab);
    el.classList.toggle('active', on);
    if (on) el.setAttribute('aria-current', 'page');
    else el.removeAttribute('aria-current');
  });
}

// closeOverlays dismisses whichever full-screen view / modal is currently open
// so the Feed tab can return the user to the channel list.
// Mirror is reparented into the shell (no modal) — close it via its html.tm-open
// state flag rather than a modal .active class.
function closeMirror() {
  if (document.documentElement.classList.contains('tm-open') && typeof closeTelemirror === 'function') {
    try { closeTelemirror(); } catch (e) { }
  }
}

// Chat is reparented into the shell (no modal) — close it via its
// html.chat-section state flag, like Mirror.
function closeChat() {
  if (document.documentElement.classList.contains('chat-section') && typeof closeMessenger === 'function') {
    try { closeMessenger(); } catch (e) { }
  }
}

// Telegram is reparented into the shell too — close via its html.tel-section
// state flag, like Mirror/Chat.
function closeTelSection() {
  if (document.documentElement.classList.contains('tel-section') && typeof closeTel === 'function') {
    try { closeTel(); } catch (e) { }
  }
}

// Resolver (Scanner+Bank) is reparented into the shell — close via its
// html.resolver-section state flag, like Mirror/Chat.
function closeResolver() {
  if (document.documentElement.classList.contains('resolver-section') && typeof closeResolverSection === 'function') {
    try { closeResolverSection(); } catch (e) { }
  }
}

function closeModalOverlays() {
  var map = [
    ['settingsModal', 'closeSettings'],
    ['scannerModal', 'closeScanner'],
    ['resolversModal', 'closeResolversModal'],
    ['profilesModal', 'closeProfiles'],
  ];
  map.forEach(function (pair) {
    var el = document.getElementById(pair[0]);
    if (el && el.classList.contains('active') && typeof window[pair[1]] === 'function') {
      try { window[pair[1]](); } catch (e) { }
    }
  });
}

function closeOverlays() {
  closeMirror();
  closeChat();
  closeTelSection();
  closeResolver();
  closeSettingsSec();
  closeModalOverlays();
}

// Settings is reparented into the shell — close via its html.settings-section
// state flag, like the other sections.
function closeSettingsSec() {
  if (document.documentElement.classList.contains('settings-section') && typeof closeSettingsSection === 'function') {
    try { closeSettingsSection(); } catch (e) { }
  }
}

function navTo(tab) {
  // Every section tab: tear the others down without their history unwind
  // (which would cross-talk), then open this one, adopting their entry.
  var SECTIONS = {
    mirror: { flag: 'tm-open', open: 'openTelemirror', off: 'tmCloseForNav' },
    chat: { flag: 'chat-section', open: 'openMessenger', off: 'chatCloseForNav' },
    tel: { flag: 'tel-section', open: 'openTel', off: 'telCloseForNav' },
    resolver: { flag: 'resolver-section', open: 'openResolverSection', off: 'resolverCloseForNav' },
    settings: { flag: 'settings-section', open: 'openSettings', off: 'settingsCloseForNav' }
  };
  if (SECTIONS[tab]) {
    var me = SECTIONS[tab];
    var adopt = false;
    Object.keys(SECTIONS).forEach(function (k) {
      if (k === tab) return;
      var fn = window[SECTIONS[k].off];
      if (typeof fn === 'function' && fn()) adopt = true;
    });
    closeModalOverlays();
    var openFn = window[me.open];
    if (!document.documentElement.classList.contains(me.flag) && typeof openFn === 'function') {
      openFn(adopt);
    }
    setActiveTab(tab);
    return;
  }
  if (tab === 'feed') {
    closeOverlays();
    // Mobile: if we're inside a conversation, the feed tab acts as "back".
    if (typeof mobileQuery !== 'undefined' && mobileQuery.matches &&
      document.getElementById('app').classList.contains('chat-open') &&
      typeof feedBack === 'function') {
      feedBack();
    }
    setActiveTab('feed');
    return;
  }
  // Fallback: close whatever's open.
  closeOverlays();
}

// ----- chat unread badge mirrored into the nav -----
function syncChatBadge() {
  var dst = document.getElementById('navChatBadge');
  if (!dst) return;
  var src = document.getElementById('chatSidebarBadge');
  var n = src ? (parseInt((src.textContent || '').replace(/\D/g, ''), 10) || 0) : 0;
  if (n > 0) { dst.textContent = n > 99 ? '99+' : String(n); dst.hidden = false; }
  else { dst.hidden = true; }
}

function watchChatBadge() {
  var src = document.getElementById('chatSidebarBadge');
  if (!src || typeof MutationObserver === 'undefined') return;
  try {
    new MutationObserver(syncChatBadge).observe(src, {
      childList: true, characterData: true, subtree: true, attributes: true
    });
  } catch (e) { }
}

// renderNavBar is called from applyLang() — labels are data-i18n, so we only
// need to re-sync the dynamic badge here.
function renderNavBar() { syncChatBadge(); }

// ----- Mirror note dismiss ("don't show again") -----
function tmDismissNote() {
  var w = document.getElementById('tmDisclaimerWrap');
  if (w) w.style.display = 'none';
  try { localStorage.setItem('tm_note_off', '1'); } catch (e) { }
  // Persist server-side too: localStorage is per-origin, so a client that comes
  // up on a new port would otherwise re-show the note (see ScanPromptOff).
  try {
    fetch('/api/settings', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ mirrorNoteOff: true })
    }).catch(function () { });
  } catch (e) { }
}

function applyTmNoteState() {
  try {
    if (localStorage.getItem('tm_note_off') === '1') {
      var w = document.getElementById('tmDisclaimerWrap');
      if (w) w.style.display = 'none';
    }
  } catch (e) { }
}

// The mobile panes are positioned with transform; .app must never scroll. But a
// descendant's scrollIntoView()/focus() (e.g. the auto-scroll-to-newest after
// loading a channel) scrolls .app horizontally into the off-screen pane —
// overflow:hidden/clip still permits programmatic scroll on some engines. Pin it
// back to 0 so the sidebar/content swap stays correct.
function initAppScrollGuard() {
  var app = document.getElementById('app');
  if (!app) return;
  app.addEventListener('scroll', function () {
    if (app.scrollLeft !== 0) app.scrollLeft = 0;
    if (app.scrollTop !== 0) app.scrollTop = 0;
  }, { passive: true });
}

// ----- resizable desktop sidebar -----
function initSidebarResize() {
  var handle = document.getElementById('sidebarResize');
  var sidebar = document.getElementById('sidebar');
  if (!handle || !sidebar) return;
  var W_MIN = 240, W_MAX = 520, lastW = 0, dragging = false;
  function setW(w) {
    if (w < W_MIN) w = W_MIN;
    if (w > W_MAX) w = W_MAX;
    lastW = w;
    document.documentElement.style.setProperty('--sidebar-w', w + 'px');
  }
  try { var s = parseInt(localStorage.getItem('thefeed_sidebar_w'), 10); if (s) setW(s); } catch (e) { }
  function move(x) { setW(x - sidebar.getBoundingClientRect().left); }
  function down(e) {
    if (typeof mobileQuery !== 'undefined' && mobileQuery.matches) return; // desktop only
    dragging = true; handle.classList.add('dragging');
    document.body.style.userSelect = 'none';
    e.preventDefault();
  }
  function mm(e) { if (dragging) { move(e.clientX); e.preventDefault(); } }
  function up() {
    if (!dragging) return;
    dragging = false; handle.classList.remove('dragging');
    document.body.style.userSelect = '';
    try { if (lastW) localStorage.setItem('thefeed_sidebar_w', String(lastW)); } catch (e) { }
  }
  handle.addEventListener('mousedown', down);
  document.addEventListener('mousemove', mm);
  document.addEventListener('mouseup', up);
}

// ----- pull-to-refresh on the channel list -----
// Only the small indicator moves; the scrolling list is never transformed, so
// there's no transform-transition for the Android WebView compositor to freeze.
function initPullToRefresh() {
  var list = document.getElementById('channelList');
  var sidebar = document.getElementById('sidebar');
  if (!list || !sidebar) return;
  var ind = document.createElement('div');
  ind.className = 'ptr-ind';
  ind.innerHTML = '<span class="ptr-spin"></span>';
  sidebar.appendChild(ind);
  var startY = 0, dist = 0, active = false, refreshing = false;
  var THRESH = 62, MAX = 90;

  function place() {
    // Start the spinner just below the floating header, not at the very top
    // (where the transparent header overlaps it).
    var hh = parseFloat(getComputedStyle(document.documentElement).getPropertyValue('--feed-header-h')) || 0;
    ind.style.top = (list.offsetTop + hh) + 'px';
  }
  function paint() {
    ind.style.opacity = String(Math.min(1, dist / THRESH));
    ind.style.transform = 'translateX(-50%) translateY(' + (dist - 26) + 'px) rotate(' + (dist * 4) + 'deg)';
    ind.classList.toggle('ready', dist >= THRESH);
  }
  function hide() {
    ind.style.opacity = '0';
    ind.style.transform = 'translateX(-50%) translateY(-26px)';
    ind.classList.remove('ready');
  }
  function trigger() {
    refreshing = true;
    ind.style.opacity = '1';
    ind.style.transform = 'translateX(-50%) translateY(10px)';
    ind.classList.remove('ready');
    ind.classList.add('spinning');
    var done = function () { refreshing = false; ind.classList.remove('spinning'); hide(); };
    try {
      var p = (typeof doRefresh === 'function') ? doRefresh() : null;
      if (p && typeof p.then === 'function') p.then(done, done); else setTimeout(done, 1300);
    } catch (e) { setTimeout(done, 1300); }
  }

  list.addEventListener('touchstart', function (e) {
    if (refreshing || e.touches.length !== 1 || list.scrollTop > 0) { active = false; return; }
    startY = e.touches[0].clientY; active = true; dist = 0; place();
  }, { passive: true });
  list.addEventListener('touchmove', function (e) {
    if (!active || refreshing) return;
    var dy = e.touches[0].clientY - startY;
    if (dy <= 0 || list.scrollTop > 0) { active = false; hide(); return; }
    dist = Math.min(MAX, dy * 0.5);
    e.preventDefault(); // suppress native overscroll while pulling
    paint();
  }, { passive: false });
  list.addEventListener('touchend', function () {
    if (!active) return;
    active = false;
    if (dist >= THRESH) trigger(); else hide();
  }, { passive: true });
  list.addEventListener('touchcancel', function () { active = false; hide(); }, { passive: true });
  hide();
}

// ----- horizontal swipe navigation between sections -----
// Sequence the user steps through with a horizontal swipe. Swipe right→left
// (finger leftward) advances; left→right goes back.
var NAV_SEQUENCE = ['feed', 'mirror', 'chat', 'tel', 'resolver', 'settings'];

function _isOpen(id) {
  var el = document.getElementById(id);
  if (!el) return false;
  if (el.classList.contains('active')) return true;
  return getComputedStyle(el).display !== 'none';
}

function currentScreen() {
  if (document.documentElement.classList.contains('tm-open')) return 'mirror';
  if (document.documentElement.classList.contains('chat-section')) return 'chat';
  if (document.documentElement.classList.contains('tel-section')) return 'tel';
  if (document.documentElement.classList.contains('resolver-section')) return 'resolver';
  if (_isOpen('scannerModal') || _isOpen('resolversModal')) return 'resolver';
  if (document.documentElement.classList.contains('settings-section')) return 'settings';
  if (_isOpen('settingsModal')) return 'settings';
  return 'feed';
}

function goToScreen(name) {
  // navTo handles every section (mirror/chat panes + scanner/bank/settings
  // modals + feed) with the right close-others logic.
  navTo(name);
}

function navSwipe(dir) { // dir: +1 forward (swipe-left), -1 back (swipe-right)
  var i = NAV_SEQUENCE.indexOf(currentScreen());
  if (i < 0) i = 0;
  var ni = i + dir;
  if (ni < 0 || ni >= NAV_SEQUENCE.length) return; // clamp at the ends
  goToScreen(NAV_SEQUENCE[ni]);
}

function _swipeBlocked(target) {
  if (!target || !target.closest) return false;
  // Surfaces that own horizontal scrolling / panning / typing.
  return !!target.closest(
    '.folder-pills, .resolver-tabs, .settings-tabs, input, textarea, select, ' +
    '.media-lightbox, .tm-lightbox, .sidebar-resize, [data-no-swipe]'
  );
}

function initSwipeNav() {
  var sx = 0, sy = 0, tracking = false, conv = false;
  document.addEventListener('touchstart', function (e) {
    tracking = false;
    if (e.touches.length !== 1) return;
    if (!(typeof mobileQuery !== 'undefined' && mobileQuery.matches)) return; // mobile only
    if (_swipeBlocked(e.target)) return;
    sx = e.touches[0].clientX; sy = e.touches[0].clientY; tracking = true;
    conv = document.getElementById('app').classList.contains('chat-open');
  }, { passive: true });
  document.addEventListener('touchend', function (e) {
    if (!tracking) return; tracking = false;
    var t = e.changedTouches && e.changedTouches[0];
    if (!t) return;
    var dx = t.clientX - sx, dy = t.clientY - sy;
    if (Math.abs(dx) < 70 || Math.abs(dx) < Math.abs(dy) * 2) return; // need a clear horizontal swipe
    var dir = dx < 0 ? 1 : -1;
    // Inside a channel conversation, a back-swipe (right) returns to the list;
    // forward-swipe is ignored so reading isn't hijacked.
    if (conv) {
      if (dir === -1) {
        // Back-swipe returns to the relevant list: chat thread → chat list,
        // mirror channel → mirror list, otherwise the feed.
        if (document.documentElement.classList.contains('chat-thread') && typeof chatBackToList === 'function') chatBackToList();
        else if (document.documentElement.classList.contains('tel-thread') && typeof telBackToList === 'function') telBackToList();
        else if (document.documentElement.classList.contains('tm-channel') && typeof toggleTmSidebar === 'function') toggleTmSidebar();
        else if (typeof feedBack === 'function') feedBack();
      }
      return;
    }
    navSwipe(dir);
  }, { passive: true });
}

// ----- active-tab/folder tracking via open/close wrappers -----
function _wrapAfter(name, after) {
  var orig = window[name];
  if (typeof orig !== 'function' || orig.__navWrapped) return;
  var wrapped = function () { var r = orig.apply(this, arguments); try { after(); } catch (e) { } return r; };
  wrapped.__navWrapped = true;
  window[name] = wrapped;
}

function wrapNav() {
  _wrapAfter('openMessenger', function () { setActiveTab('chat'); });
  _wrapAfter('closeMessenger', function () { setActiveTab('feed'); });
  _wrapAfter('openScanner', function () { setActiveTab('resolver'); });
  _wrapAfter('openResolversModal', function () { setActiveTab('resolver'); });
  _wrapAfter('closeResolversModal', function () { if (typeof currentScreen === 'function' && currentScreen() === 'feed') setActiveTab('feed'); });
  _wrapAfter('openSettings', function () {
    setActiveTab('settings');
    // Show the active profile name under the Profiles row (empty when none).
    var sub = document.getElementById('settingsProfilesSub');
    if (sub) {
      var hasProfile = (typeof activeProfileId !== 'undefined' && activeProfileId);
      var nameEl = document.getElementById('profileBtnName');
      sub.textContent = hasProfile && nameEl ? (nameEl.textContent || '').trim() : '';
    }
  });
  _wrapAfter('closeSettings', function () { setActiveTab('feed'); });
  _wrapAfter('openTel', function () { setActiveTab('tel'); });
  _wrapAfter('closeTel', function () { setActiveTab('feed'); });
  _wrapAfter('openTelemirror', function () { setActiveTab('mirror'); });
  _wrapAfter('closeTelemirror', function () { setActiveTab('feed'); });
}

// The Feed/Mirror header floats over the channel list on mobile (see nav.css);
// publish its live height as --feed-header-h so the list's padding-top tracks it
// — including when the search row collapses on scroll and when Mirror swaps the
// profile row for the add-channel row.
function initFloatHeaderMetric() {
  var hdr = document.querySelector('.sidebar-header');
  if (!hdr) return;
  var set = function () {
    document.documentElement.style.setProperty('--feed-header-h', hdr.offsetHeight + 'px');
  };
  set();
  if (typeof ResizeObserver === 'function') {
    try { new ResizeObserver(set).observe(hdr); } catch (e) { }
  }
  window.addEventListener('resize', set);
}

(function navInit() {
  function go() {
    wrapNav();
    initAppScrollGuard();
    initSidebarResize();
    initPullToRefresh();
    initSwipeNav();
    initFloatHeaderMetric();
    watchChatBadge();
    syncChatBadge();
    setActiveTab('feed');
    applyTmNoteState();
  }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', go, { once: true });
  else go();
})();

// Keep the nav highlight honest after a hardware/browser back. Each section
// owns a popstate handler that tears itself down (→ shows the feed) or returns
// to its sidebar; none of them touch the nav tab. nav.js loads LAST, so this
// listener runs AFTER those handlers have updated the html state classes —
// re-sync the active tab to whatever is actually on screen.
window.addEventListener('popstate', function () {
  if (typeof setActiveTab === 'function' && typeof currentScreen === 'function') {
    setActiveTab(currentScreen());
  }
});

if (typeof window !== 'undefined') {
  window.navTo = navTo;
  window.renderNavBar = renderNavBar;
  window.tmDismissNote = tmDismissNote;
  window.setActiveTab = setActiveTab;
  window.navSwipe = navSwipe;
}
