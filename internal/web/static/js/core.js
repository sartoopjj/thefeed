// ===== STATE =====
var selectedChannel = 0, channels = [], eventSource = null, autoRefreshTimer = null, telegramLoggedIn = false, logVisible = false;
var _selectGen = 0;
// previousMsgIDs is kept for the "no_new_messages" toast on refresh.
// previousContentHashes drives the channel-list NEW badge — robust across
// both Telegram (monotonic IDs) and X accounts (CRC32-hashed snowflake
// IDs that aren't ordered). Both maps are persisted to localStorage so
// they survive page reload and the user keeps seeing badges.
var serverNextFetch = 0, nextFetchInterval = null, previousMsgIDs = loadSeenMap('thefeed_seen_ids'), previousContentHashes = loadSeenMap('thefeed_seen_hashes'), currentMsgTexts = [];
function loadSeenMap(storageKey) {
  try {
    var raw = localStorage.getItem(storageKey);
    if (!raw) return {};
    var parsed = JSON.parse(raw);
    return (parsed && typeof parsed === 'object') ? parsed : {};
  } catch (e) { return {}; }
}
function saveSeenMap(storageKey, m) {
  try { localStorage.setItem(storageKey, JSON.stringify(m)); } catch (e) { }
}
function rememberSeen(name, lastID, contentHash) {
  if (!name) return;
  previousMsgIDs[name] = lastID || 0;
  previousContentHashes[name] = contentHash || 0;
  saveSeenMap('thefeed_seen_ids', previousMsgIDs);
  saveSeenMap('thefeed_seen_hashes', previousContentHashes);
  pushSeen(name, lastID || 0, contentHash || 0);
}
// ----- server-side seen-state -----
// The seen maps used to live only in localStorage, which the WebView wipes
// whenever the loopback port changes between launches (Android/iOS), so the
// unread counts reset to nonsense. We mirror them to disk via /api/seen.
var seenServerSynced = false;
// seenLocalOnly is set when the backend reports shared/multi-user mode
// (--shared). Then seen-state stays in this browser's localStorage and is
// never read from or written to the server, so users sharing one backend
// don't clear each other's unread counts.
var seenLocalOnly = false;
// syncSeenFromServer pulls the disk copy once at startup and lets it win
// over localStorage (the server copy survives the loopback port changing).
async function syncSeenFromServer() {
  if (seenServerSynced) return;
  seenServerSynced = true;
  var d;
  try {
    var r = await fetch('/api/seen');
    if (!r.ok) return;
    d = await r.json();
  } catch (e) { return; }
  if (d && d.shared) {
    // Shared/multi-user backend: keep per-browser localStorage as-is — don't
    // wipe it, don't merge server state, don't push (pushSeen* also bail).
    seenLocalOnly = true;
    return;
  }
  // One-time cleanup: older builds kept seen-state only in localStorage. We
  // deliberately do NOT migrate that to the server (it's origin-scoped and
  // can be stale) — we just drop it so it can't seed the server with wrong
  // counts. The server is the source of truth from here on; with nothing
  // stored, channels re-baseline to "all read" on next load, like Telegram.
  // TODO(v1): remove this one-time localStorage cleanup once every client
  // has launched at least once on a build that persists seen-state to disk.
  try {
    if (!localStorage.getItem('thefeed_seen_cleared')) {
      localStorage.removeItem('thefeed_seen_ids');
      localStorage.removeItem('thefeed_seen_hashes');
      localStorage.setItem('thefeed_seen_cleared', '1');
      previousMsgIDs = {};
      previousContentHashes = {};
    }
  } catch (e) { }
  var sIds = (d && d.seenIds) || {}, sH = (d && d.seenHashes) || {};
  for (var k in sIds) previousMsgIDs[k] = sIds[k];
  for (var k2 in sH) previousContentHashes[k2] = sH[k2];
  saveSeenMap('thefeed_seen_ids', previousMsgIDs);
  saveSeenMap('thefeed_seen_hashes', previousContentHashes);
}
// pushSeen persists a single channel's read marker (fire-and-forget).
function pushSeen(name, id, hash) {
  if (!name || seenLocalOnly) return;
  try {
    fetch('/api/seen', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: name, id: id || 0, hash: hash || 0 }), keepalive: true
    }).catch(function () { });
  } catch (e) { }
}
// pushSeenBulk seeds new channel baselines server-side. The server only
// fills entries it lacks, so a real read marker is never overwritten by a
// baseline.
function pushSeenBulk(ids, hashes) {
  if (seenLocalOnly) return;
  try {
    fetch('/api/seen', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ seenIds: ids, seenHashes: hashes })
    }).catch(function () { });
  } catch (e) { }
}
var appVersion = '', latestVersion = '';
// Set from /api/settings at startup; store builds must not self-update.
var storeBuild = false;
// Removes the manual "check on GitHub" control.
function hideUpdateEntryPoints() {
  var btn = document.getElementById('checkGitHubBtn');
  if (btn) btn.style.display = 'none';
}
var profiles = null, activeProfileId = '', editingProfileId = null, resolverScanHint = '', resolverScanHealthy = 0, resolverScanDone = 0, resolverScanTotal = 0;
var currentMaxMsgID = 0;
var currentMaxTimestamp = 0;
var newMsgScrollDone = false;
// Per-channel state for the "new messages" separator. The separator is
// kept visible across re-renders for NEW_MSG_STICKY_MS by deferring the
// lastSeen-timestamp commit, so users actually have time to notice the
// new content before it gets marked seen.
var NEW_MSG_STICKY_MS = 10000; // how long the "new messages" tag stays
var newMsgSepLastSeen = {};    // ch name → lastSeenTs the current sep represents
var newMsgSepCommitTimer = {}; // ch name → setTimeout handle for deferred commit
var refreshingChannels = {};

// ===== MOBILE NAV =====
var mobileQuery = window.matchMedia('(max-width: 768px)');
var chatIsOpen = false;
// Desktop: chat is always laid out alongside the sidebar.
// Mobile: chat is only visible when .chat-open is set.
function _chatPanelVisible() {
  return !mobileQuery.matches || document.getElementById('app').classList.contains('chat-open');
}

function openChat() {
  chatIsOpen = true;
  if (mobileQuery.matches) {
    document.getElementById('app').classList.add('chat-open');
    history.pushState({ view: 'chat' }, '');
  }
}
function openSidebar() {
  chatIsOpen = false;
  _selectGen++;
  document.getElementById('app').classList.remove('chat-open');
}
// feedBack: the content pane's back button. Remove chat-open directly — the feed
// shares window.history with the messenger and telemirror (each with their own
// push/popstate handling), so routing back through history.back() can land on a
// foreign state and never clear chat-open, leaving the user stuck in a channel.
// Direct removal always returns to the list.
function feedBack() {
  if (viewingSaved) closeSavedMessages();
  else openSidebar();
}
window.feedBack = feedBack;
window.addEventListener('popstate', function () {
  // This handles ONLY the feed's channel view. The Mirror/Chat/Resolver/Settings
  // sections also set #app.chat-open for their channel/thread/pane views and run
  // their OWN popstate handlers — so skip here, otherwise an unrelated history
  // pop (e.g. closing a Mirror image lightbox) would call openSidebar() and
  // collapse the section's view, leaving a broken state with the nav hidden.
  var de = document.documentElement;
  if (de.classList.contains('tm-open') || de.classList.contains('chat-section') ||
      de.classList.contains('resolver-section') || de.classList.contains('settings-section')) {
    return;
  }
  if (mobileQuery.matches && document.getElementById('app').classList.contains('chat-open')) {
    openSidebar();
  }
});
document.addEventListener('visibilitychange', function () {
  if (!document.hidden && mobileQuery.matches && chatIsOpen) {
    document.getElementById('app').classList.add('chat-open');
  }
});
function filterChannels() {
  var q = document.getElementById('channelSearch').value.toLowerCase();
  // In the Mirror folder the same search box filters the telemirror list.
  if (document.documentElement.classList.contains('tm-open')) {
    document.querySelectorAll('#tmChannelsList .tm-channel-item').forEach(function (el) {
      if (el.classList.contains('tm-saved-shortcut')) return; // keep Saved shortcut visible
      var hay = ((el.getAttribute('data-u') || '') + ' ' + (el.textContent || '')).toLowerCase();
      el.style.display = hay.includes(q) ? 'flex' : 'none';
    });
    return;
  }
  document.querySelectorAll('.ch-item').forEach(function (el) {
    var hay = (el.dataset.name + ' ' + (el.dataset.label || '')).toLowerCase();
    el.style.display = hay.includes(q) ? 'flex' : 'none';
  });
}

// Reveal/hide the channel search box on demand (it's hidden by default — a
// header toggle button drives it, like the log button). Shared by Feed and
// Mirror; closing clears the filter so nothing stale lingers across modes.
function toggleChannelSearch() {
  var header = document.querySelector('.sidebar-header');
  if (!header) return;
  var open = header.classList.toggle('search-open');
  var input = document.getElementById('channelSearch');
  document.querySelectorAll('.channel-search-toggle').forEach(function (b) {
    b.classList.toggle('active', open);
  });
  if (open) {
    if (input) setTimeout(function () { input.focus(); }, 0);
  } else if (input) {
    input.value = '';
    filterChannels();
  }
}
// Close + clear the search when the user leaves the current list (e.g. switching
// Feed↔Mirror) so a leftover query doesn't silently filter the other list.
function closeChannelSearch() {
  var header = document.querySelector('.sidebar-header');
  if (!header || !header.classList.contains('search-open')) return;
  toggleChannelSearch();
}

// +/- stepper for numeric fields: big tap targets replacing the tiny native
// spin arrows. Honors the input's min/max/step and fires its change handler so
// the value persists (autoSaveSettings etc.).
function stepNum(id, dir) {
  var el = document.getElementById(id);
  if (!el) return;
  var step = parseFloat(el.getAttribute('step')) || 1;
  var minA = el.getAttribute('min'), maxA = el.getAttribute('max');
  var min = (minA !== null && minA !== '') ? parseFloat(minA) : -Infinity;
  var max = (maxA !== null && maxA !== '') ? parseFloat(maxA) : Infinity;
  var v = (parseFloat(el.value) || 0) + dir * step;
  if (v < min) v = min;
  if (v > max) v = max;
  el.value = String(v);
  el.dispatchEvent(new Event('change', { bubbles: true }));
}

// ===== INIT =====
async function init() {
  loadTheme();
  applyLang();
  await loadFontSize();
  loadBgImage();
  connectSSE();
  refreshResolversBadge();
  // Seed the messenger unread badge from persisted threads so a cold start
  // (e.g. opening the app by tapping a new-message notification) shows the
  // count immediately, without having to open the messenger first.
  if (typeof chatLoadThreads === 'function') chatLoadThreads();
  // Populate profilePicCache so the channel list renders avatars
  // without a per-item probe.
  loadProfilePicState().catch(function () { });
  // Quietly ask GitHub for the latest published client version. Runs in
  // the background so a slow github.com response can't delay startup —
  // if there's an update, the dialog shows up a few seconds later.
  // Skipped on iOS: App Store / TestFlight handles updates there.
  // Must precede the update check: migration 1 repairs skip flags that the
  // check reads back, and racing it costs another launch.
  var migrated = fetch('/api/settings')
    .then(function (r) { return r.ok ? r.json() : null; })
    .then(function (s) {
      storeBuild = !!(s && s.storeBuild);
      return runPendingMigrations(s);
    })
    .catch(function () { });
  if (typeof IOS === 'undefined') {
    migrated.then(function () {
      if (storeBuild) { hideUpdateEntryPoints(); return; }
      return checkGitHubUpdate(false);
    }).catch(function () { });
  } else {
    hideUpdateEntryPoints();
  }
  try {
    var r = await fetch('/api/status'); var st = await r.json();
    await loadProfiles();
    if (!st.configured) { openProfiles(); return }
    checkAndShowSavedResolversPrompt(st);
    telegramLoggedIn = !!st.telegramLoggedIn;
    serverNextFetch = st.nextFetch || 0;
    latestVersion = st.latestVersion || '';
    renderLatestVersion();
    updateNextFetchDisplay();
    await loadChannels();
    // Land on the channel list; don't auto-open the first channel.
    if (!channels || channels.length === 0) {
      showInitProgress(); await doRefresh();
    } else {
      // Mobile: make sure the sidebar is visible so the user can pick.
      openSidebar();
      document.getElementById('messages').innerHTML = '<div class="empty-state"><p>' + (t('select_channel_hint') || '') + '</p></div>';
    }
    startAutoRefresh();
  } catch (e) { }
}

// ===== FONT SIZE =====
async function loadFontSize() {
  try {
    var r = await fetch('/api/settings'); var s = await r.json();
    if (s.fontSize >= 11 && s.fontSize <= 22) {
      document.documentElement.style.setProperty('--font-size', s.fontSize + 'px');
      document.getElementById('fontSizeSlider').value = s.fontSize;
      document.getElementById('fontSizeVal').textContent = s.fontSize;
    }
    if (s.debug) document.getElementById('cfgDebug').checked = true;
    if (s.theme && (s.theme === 'dark' || s.theme === 'light' || s.theme === 'system')) {
      localStorage.setItem('thefeed_theme', s.theme);
      applyResolvedTheme();
      applyThemeButtons();
    }
    if (s.lang && (s.lang === 'fa' || s.lang === 'en')) {
      lang = s.lang;
      localStorage.setItem('thefeed_lang', s.lang);
      applyLang();
    }
    if (s.version) { appVersion = s.version; renderAppVersion(s.version, s.commit); }
    // Sync the server-persisted "don't show scan prompt" flag
    // into localStorage. Android picks a new 127.0.0.1 port on
    // each launch, so localStorage alone wouldn't survive
    // restart — the server-side flag is the source of truth.
    if (s.scanPromptOff === true) {
      localStorage.setItem('thefeed_scan_prompt_off', '1');
    } else if (s.scanPromptOff === false) {
      localStorage.removeItem('thefeed_scan_prompt_off');
    }
    // Mirror-note dismissal is server-persisted too (survives a new client
    // port). Mirror it into localStorage + hide the note if already dismissed.
    if (s.mirrorNoteOff === true) {
      try { localStorage.setItem('tm_note_off', '1'); } catch (e) { }
      if (typeof applyTmNoteState === 'function') applyTmNoteState();
    } else if (s.mirrorNoteOff === false) {
      try { localStorage.removeItem('tm_note_off'); } catch (e) { }
    }
    if (s.telIntroSeen === true && typeof telIntroRead === 'function') telIntroRead();
    // Populate pinned channels from the server response (per-profile).
    pinnedChannels = new Set();
    if (Array.isArray(s.pinnedChannels)) {
      for (var pi = 0; pi < s.pinnedChannels.length; pi++) {
        var pn = String(s.pinnedChannels[pi] || '').replace(/^@/, '').trim();
        if (pn) pinnedChannels.add(pn);
      }
    }
    renderLatestVersion();
  } catch (e) { }
}

function renderAppVersion(v, commit) {
  var vEl = document.getElementById('appVersionEl');
  if (!vEl) return;
  if (!v) { vEl.textContent = '-'; return; }
  vEl.textContent = v + (commit && commit !== 'unknown' ? ' (' + commit.slice(0, 7) + ')' : '');
}

function renderLatestVersion() {
  var vEl = document.getElementById('latestVersionEl');
  if (vEl) vEl.textContent = latestVersion || '-';
}

function normalizeVersion(v) {
  if (!v) return '';
  v = String(v).trim().replace(/^v/i, '');
  return v;
}

function compareSemver(a, b) {
  a = normalizeVersion(a); b = normalizeVersion(b);
  if (!a || !b || a === 'dev' || b === 'dev') return 0;
  var as = a.split('.'); var bs = b.split('.');
  var n = Math.max(as.length, bs.length);
  for (var i = 0; i < n; i++) {
    var ai = parseInt(as[i] || '0', 10); if (isNaN(ai)) ai = 0;
    var bi = parseInt(bs[i] || '0', 10); if (isNaN(bi)) bi = 0;
    if (ai > bi) return 1;
    if (ai < bi) return -1;
  }
  return 0;
}

function maybeWarnNewVersion() {
  if (!latestVersion || !appVersion) return;
  if (compareSemver(latestVersion, appVersion) <= 0) return;
  var seenKey = 'thefeed_seen_update_' + normalizeVersion(latestVersion);
  if (localStorage.getItem(seenKey) === '1') return;
  localStorage.setItem(seenKey, '1');
  showToast(t('update_available').replace('{v}', latestVersion));
  addLogLine('Warning: ' + t('update_available').replace('{v}', latestVersion));
}
function previewFontSize(v) { document.documentElement.style.setProperty('--font-size', v + 'px'); document.getElementById('fontSizeVal').textContent = v }

// ===== THEME =====
// Preference is 'system' (default) | 'dark' | 'light'. 'system' follows the
// device's prefers-color-scheme and tracks live OS changes. The data-theme
// attribute the CSS reads is always the RESOLVED 'dark'/'light'.
function themePref() { return localStorage.getItem('thefeed_theme') || 'system'; }
function resolveTheme(pref) {
  if (pref === 'dark' || pref === 'light') return pref;
  return (window.matchMedia && window.matchMedia('(prefers-color-scheme: light)').matches) ? 'light' : 'dark';
}
function applyResolvedTheme() {
  var resolved = resolveTheme(themePref());
  document.documentElement.setAttribute('data-theme', resolved);
  syncNativeSystemBars(resolved);
}
// Recolor the native status + gesture/home-indicator bars to match the theme
// (Android via Android.setSystemBars, iOS via IOS.setSystemBars). The web
// <meta theme-color> only affects browser chrome, not a native WebView's
// system bars, so the shell has to do it. No-op in a plain browser.
function syncNativeSystemBars(resolved) {
  try {
    var bg = (getComputedStyle(document.documentElement).getPropertyValue('--bg2') || '').trim()
      || (resolved === 'light' ? '#f0f2f5' : '#0e1621');
    var dark = resolved === 'dark';
    if (typeof Android !== 'undefined' && Android.setSystemBars) Android.setSystemBars(bg, dark);
    else if (typeof IOS !== 'undefined' && IOS.setSystemBars) IOS.setSystemBars(bg, dark);
  } catch (e) { }
}
var _themeMqlBound = false;
function loadTheme() {
  applyResolvedTheme();
  // While the preference is 'system', re-apply when the OS theme flips.
  if (!_themeMqlBound && window.matchMedia) {
    _themeMqlBound = true;
    var mql = window.matchMedia('(prefers-color-scheme: light)');
    var onChange = function () { if (themePref() === 'system') applyResolvedTheme(); };
    if (mql.addEventListener) mql.addEventListener('change', onChange);
    else if (mql.addListener) mql.addListener(onChange); // older WebView/Safari
  }
}
function setTheme(t) {
  localStorage.setItem('thefeed_theme', t);
  applyResolvedTheme();
  applyThemeButtons();
  fetch('/api/settings', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ theme: t }) }).catch(function () { });
}
function applyThemeButtons() {
  var cur = themePref();
  var s = document.getElementById('themeSystem');
  var d = document.getElementById('themeDark');
  var l = document.getElementById('themeLight');
  if (s) s.classList.toggle('active-theme', cur === 'system');
  if (d) d.classList.toggle('active-theme', cur === 'dark');
  if (l) l.classList.toggle('active-theme', cur === 'light');
}

// ===== LAST SEEN MESSAGES =====
function channelName(num) {
  var ch = channels[num - 1];
  return (ch && (ch.Name || ch.name)) || '';
}
function getLastSeenTimestamp(name) {
  if (!name) return 0;
  try { return parseInt(localStorage.getItem('thefeed_seen_ts_' + name)) || 0 } catch (e) { return 0 }
}
function setLastSeenTimestamp(name, ts) {
  if (!name) return;
  try { localStorage.setItem('thefeed_seen_ts_' + name, ts) } catch (e) { }
}

// ===== RESCAN PROMPT =====
// Resolves true → skip rescan. Honors scanPromptOff.
// Single-instance: if a prompt is already open (e.g. the user tapped a profile
// several times), return the SAME pending promise instead of stacking a new
// overlay each time.
var _rescanPromptPromise = null;
function askRescan(count) {
  if (localStorage.getItem('thefeed_scan_prompt_off') === '1') return Promise.resolve(true);
  if (!count || count <= 0) return Promise.resolve(true);
  if (_rescanPromptPromise) return _rescanPromptPromise;
  _rescanPromptPromise = new Promise(function (resolve) {
    var msg = t('rescan_prompt_msg').replace('{n}', count);
    var overlay = document.createElement('div');
    overlay.className = 'modal-overlay active';
    // The floating nav sits at z-index 9300; the base .modal-overlay (100) would
    // render this startup prompt underneath it. Lift it above the nav.
    overlay.style.zIndex = '9600';
    overlay.innerHTML =
      '<div class="modal" style="max-width:380px">'
      + '<h2 style="margin-top:0">' + esc(t('rescan_prompt_title')) + '</h2>'
      + '<p style="font-size:13px;color:var(--text-dim);margin-bottom:16px;line-height:1.6">' + esc(msg) + '</p>'
      + '<div style="display:flex;justify-content:flex-end;margin-bottom:10px">'
      + '<button class="btn btn-flat" id="rescanPromptNever" style="font-size:11px;padding:4px 10px;color:var(--text-dim)">' + esc(t('dont_show_again')) + '</button>'
      + '</div>'
      + '<div class="modal-actions">'
      + '<button class="btn btn-outline" id="rescanPromptYes">' + esc(t('rescan_prompt_yes')) + '</button>'
      + '<button class="btn btn-primary" id="rescanPromptSkip">' + esc(t('rescan_prompt_skip')) + '</button>'
      + '</div></div>';
    document.body.appendChild(overlay);
    var done = function (skip) {
      if (overlay.parentNode) overlay.parentNode.removeChild(overlay);
      _rescanPromptPromise = null;
      resolve(skip);
    };
    document.getElementById('rescanPromptSkip').onclick = function () { done(true) };
    document.getElementById('rescanPromptYes').onclick = function () { done(false) };
    document.getElementById('rescanPromptNever').onclick = function () {
      localStorage.setItem('thefeed_scan_prompt_off', '1');
      try {
        fetch('/api/settings', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ scanPromptOff: true })
        });
      } catch (e) { }
      done(true);
    };
  });
  // MUST return the promise: without this askRescan() returns undefined, so
  // `skipCheck = await askRescan()` becomes undefined → JSON.stringify drops the
  // key → the server defaults skipCheck to false → it rescans immediately,
  // before the user even touches the prompt. (Regression from the single-
  // instance refactor.)
  return _rescanPromptPromise;
}
function showRescanPrompt(count) { return askRescan(count); } // legacy alias
function showConfirmDialog(msg, yesText, noText) {
  return new Promise(function (resolve) {
    var overlay = document.createElement('div');
    overlay.className = 'modal-overlay active';
    // Float above any open full-screen view (chat modal z9100, etc.) so the
    // confirm lands on the SAME screen the user clicked from — not behind it.
    overlay.style.zIndex = '99990';
    overlay.innerHTML = '<div class="modal" style="max-width:380px"><p style="font-size:13px;color:var(--text);margin-bottom:16px;line-height:1.6">' + esc(msg) + '</p><div class="modal-actions"><button class="btn btn-flat" id="confirmNo">' + esc(noText || t('cancel')) + '</button><button class="btn btn-primary" id="confirmYes">' + esc(yesText || t('ok')) + '</button></div></div>';
    document.body.appendChild(overlay);
    // Look buttons up inside THIS overlay: when two dialogs are open at once
    // the ids repeat and getElementById would wire the hidden one.
    overlay.querySelector('#confirmNo').onclick = function () { overlay.remove(); resolve(false) };
    overlay.querySelector('#confirmYes').onclick = function () { overlay.remove(); resolve(true) };
  });
}

// showChoiceDialog is a themed replacement for a native <select> popup:
// one row per item, the current one ticked. Resolves the chosen value, or
// null when dismissed.
function showChoiceDialog(opts) {
  return new Promise(function (resolve) {
    var overlay = document.createElement('div');
    overlay.className = 'modal-overlay active';
    overlay.style.zIndex = '99990'; // above full-screen views (see showConfirmDialog)
    var h = '<div class="modal choice-modal">';
    if (opts.title) h += '<h2 class="choice-title">' + esc(opts.title) + '</h2>';
    h += '<div class="choice-list">';
    (opts.items || []).forEach(function (it, i) {
      var on = it.value === opts.value;
      h += '<button type="button" class="choice-row' + (on ? ' on' : '') + '" data-i="' + i + '">'
        + '<span class="choice-text"><span class="choice-label" dir="auto">' + esc(it.label) + '</span>'
        + (it.sub ? '<span class="choice-sub" dir="ltr">' + esc(it.sub) + '</span>' : '')
        + '</span>' + (on ? '<span class="choice-check">' + icon('check') + '</span>' : '') + '</button>';
    });
    h += '</div></div>';
    overlay.innerHTML = h;
    document.body.appendChild(overlay);
    function done(v) { overlay.remove(); resolve(v); }
    overlay.onclick = function (e) {
      var row = e.target.closest('.choice-row');
      if (row) done(opts.items[+row.dataset.i].value);
      else if (e.target === overlay) done(null);
    };
  });
}

// showInputDialog is a themed window.prompt replacement. Returns
// the trimmed input string on confirm, or null on cancel/escape.
// Uses the existing modal-overlay pattern so it inherits the
// app's theme — no more browser-native prompt with default
// chrome that ignores dark mode.
function showInputDialog(opts) {
  opts = opts || {};
  return new Promise(function (resolve) {
    var overlay = document.createElement('div');
    overlay.className = 'modal-overlay active';
    overlay.style.zIndex = '99990'; // above full-screen views (see showConfirmDialog)
    var titleHtml = opts.title ? '<h2 style="margin-top:0;margin-bottom:8px;font-size:16px">' + esc(opts.title) + '</h2>' : '';
    var msgHtml = opts.message ? '<p style="font-size:13px;color:var(--text-dim);margin:0 0 12px;line-height:1.6">' + esc(opts.message) + '</p>' : '';
    overlay.innerHTML =
      '<div class="modal" style="max-width:380px">'
      + titleHtml + msgHtml
      + '<input type="text" id="inputDialogField" maxlength="' + (opts.maxLength || 64) + '"'
      + ' value="' + esc(opts.value || '') + '"'
      + ' placeholder="' + esc(opts.placeholder || '') + '"'
      + ' autocomplete="off" spellcheck="false"'
      + ' style="width:100%;padding:9px 12px;background:var(--bg);color:var(--text);border:1px solid var(--border);border-radius:8px;font-size:13px;box-sizing:border-box;font-family:inherit">'
      + '<div class="modal-actions">'
      + '<button class="btn btn-flat" id="inputDialogCancel">' + esc(opts.cancelText || t('cancel') || 'Cancel') + '</button>'
      + '<button class="btn btn-primary" id="inputDialogOk">' + esc(opts.okText || t('ok') || 'OK') + '</button>'
      + '</div></div>';
    document.body.appendChild(overlay);
    var field = overlay.querySelector('#inputDialogField');
    var done = function (val) {
      document.removeEventListener('keydown', onKey);
      if (overlay.parentNode) overlay.parentNode.removeChild(overlay);
      resolve(val);
    };
    overlay.querySelector('#inputDialogCancel').onclick = function () { done(null); };
    overlay.querySelector('#inputDialogOk').onclick = function () {
      var v = (field.value || '').trim();
      done(v || null);
    };
    var onKey = function (e) {
      if (e.key === 'Escape') { done(null); }
      else if (e.key === 'Enter') {
        var v = (field.value || '').trim();
        done(v || null);
      }
    };
    document.addEventListener('keydown', onKey);
    // Focus & select after the modal is in the DOM so iOS WebView
    // also gets the soft keyboard up.
    setTimeout(function () { try { field.focus(); field.select(); } catch (e) { } }, 30);
  });
}

// triggerDownload saves a blob to the user's device. Resolves false only when
// a save definitely failed; iOS and the anchor path cannot report either way.
function triggerDownload(blob, filename) {
  // Ensure the filename has an extension — iOS share sheet and Android
  // bridge use it literally, unlike <a download> which infers from MIME.
  if (filename && filename.indexOf('.') === -1 && blob.type) {
    var ext = { 'image/jpeg': '.jpg', 'image/png': '.png', 'image/gif': '.gif',
      'image/webp': '.webp', 'video/mp4': '.mp4', 'audio/mpeg': '.mp3',
      'audio/ogg': '.ogg', 'application/pdf': '.pdf' }[blob.type];
    if (!ext) {
      var sub = blob.type.split('/')[1];
      // octet-stream names no real extension; ".octet-stream" would leave a
      // file no OS can open.
      if (sub && sub !== 'octet-stream') ext = '.' + sub.replace(/\+.*$/, '');
    }
    if (ext) filename += ext;
  }
  // Native bridge (Android AND iOS): a real save that needs no user gesture.
  // navigator.share is deliberately not used: it requires transient user
  // activation, which has expired by the time the blob's fetch resolves, so it
  // silently saved nothing — and when the iOS app runs on macOS its share
  // sheet has no "save" entry at all.
  var bridge = (typeof window !== 'undefined') ? (window.Android || window.IOS) : null;
  if (bridge && typeof bridge.saveMedia === 'function') {
    return new Promise(function (resolve) {
      var fail = function () { showToast(t('save_failed')); resolve(false); };
      var reader = new FileReader();
      reader.onload = function () {
        var b64 = (reader.result || '').split(',')[1] || '';
        // Android returns false on failure; iOS returns undefined either way.
        try { resolve(bridge.saveMedia(b64, blob.type || 'application/octet-stream', filename) !== false); }
        catch (e) { fail(); }
      };
      reader.onerror = fail;
      reader.readAsDataURL(blob);
    });
  }
  // Every desktop browser handles this natively.
  anchorDownload(blob, filename);
  return Promise.resolve(true);
}

function anchorDownload(blob, filename) {
  var a = document.createElement('a');
  a.href = URL.createObjectURL(blob);
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  setTimeout(function () { URL.revokeObjectURL(a.href); a.remove(); }, 60000);
}

// openExternal opens a URL in a new tab/window without leaking the opener.
// window.open returns null when 'noopener' is in the features EVEN ON
// SUCCESS, so pass no features and sever the opener on the handle instead —
// the old null-check fallback fired on every successful open and loaded the
// URL in the current tab too. A real null (popup blocked, or a WebView
// without a window factory) falls back to navigating the current view; the
// native shells intercept external URLs and hand them to the system browser.
function openExternal(url) {
  var w = null;
  try { w = window.open(url, '_blank'); } catch (e) { }
  if (w) { try { w.opener = null; } catch (e) { } }
  else { window.location.href = url; }
}

// showLinkSheet displays a bottom-sheet with the full URL, copy and
// open-in-browser buttons. The optional extra {label, action} adds a
// full-width in-app button (the feed uses it for open-channel/go-to-post).
function showLinkSheet(url, extra) {
  var old = document.getElementById('linkSheetOverlay');
  if (old) old.remove();
  var overlay = document.createElement('div');
  overlay.id = 'linkSheetOverlay';
  overlay.className = 'link-overlay';
  overlay.innerHTML = '<div class="link-sheet">'
    + '<button class="link-sheet-close" type="button" aria-label="' + escAttr(t('close') || 'Close') + '">' + icon('x') + '</button>'
    + '<div class="link-title">' + esc(t('telemirror_open_this_link') || 'Open this link?') + '</div>'
    + '<div class="link-url" dir="ltr">' + esc(url) + '</div>'
    + '<div class="link-actions">'
    + '<button class="link-btn link-copy">' + esc(t('telemirror_copy_link') || 'Copy link') + '</button>'
    + '<button class="link-btn link-open">' + esc(t('telemirror_open_link') || 'Open in browser') + '</button>'
    + '</div>'
    + (extra ? '<button class="link-btn link-goto">' + esc(extra.label) + '</button>' : '')
    + '</div>';
  overlay.addEventListener('click', function (e) {
    if (e.target === overlay) overlay.remove();
  });
  // Direct handler, NOT target-class matching on the overlay: a real click
  // lands on the svg INSIDE the button, so e.target never carries the
  // button's class (element.click() in tests masks this).
  overlay.querySelector('.link-sheet-close').onclick = function () { overlay.remove(); };
  if (extra) {
    overlay.querySelector('.link-goto').onclick = function () {
      overlay.remove();
      extra.action();
    };
  }
  overlay.querySelector('.link-copy').onclick = function () {
    try {
      if (navigator.clipboard) { navigator.clipboard.writeText(url).catch(function () {}); }
      else { var ta = document.createElement('textarea'); ta.value = url; document.body.appendChild(ta); ta.select(); document.execCommand('copy'); ta.remove(); }
    } catch (e) {}
    showToast(t('copied') || 'Copied');
    overlay.remove();
  };
  overlay.querySelector('.link-open').onclick = function () {
    overlay.remove();
    openExternal(url);
  };
  document.body.appendChild(overlay);
}

// showInfoDialog is the one-button cousin of showConfirmDialog: a small
// modal with a message and a single OK button. Used for explanatory
// bits like "this file is too large for the server cache".
function showInfoDialog(msg, okText) {
  return new Promise(function (resolve) {
    var overlay = document.createElement('div');
    overlay.className = 'modal-overlay active';
    overlay.style.zIndex = '99990'; // above the floating nav (9300) and section panes
    overlay.innerHTML = '<div class="modal" style="max-width:380px"><p style="font-size:13px;color:var(--text);margin-bottom:16px;line-height:1.6;white-space:pre-line">' + esc(msg) + '</p><div class="modal-actions"><button class="btn btn-primary" id="infoOk">' + esc(okText || t('ok') || 'OK') + '</button></div></div>';
    document.body.appendChild(overlay);
    function close() { if (overlay.parentNode) document.body.removeChild(overlay); resolve(true); }
    overlay.querySelector('#infoOk').onclick = close;
    // Tap outside the card (on the backdrop) also dismisses.
    overlay.onclick = function (e) { if (e.target === overlay) close(); };
  });
}

