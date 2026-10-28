// ===== SETTINGS =====
function openSettings(adopt) {
  renderLatestVersion();
  applyThemeButtons();
  renderPatternGrid();
  switchSettingsTab('display');
  openSettingsSection(adopt); // reparented into the shell (was: modal .active)
  try { initAndroidSettings(); } catch (e) { console.error('initAndroidSettings error:', e); }
  // Reflect current localStorage flag on every open — toggling it
  // away in the prompt itself should be picked up next time
  // settings is opened.
  var promptEl = document.getElementById('cfgShowScanPrompt');
  if (promptEl) promptEl.checked = localStorage.getItem('thefeed_scan_prompt_off') !== '1';
  // Load server-side settings: profile-pics toggle + the connection
  // fields (queryMode/rateLimit/scatter/timeout, formerly per-profile,
  // now global on ProfileList).
  fetch('/api/settings').then(function (r) { return r.ok ? r.json() : null; }).then(function (s) {
    if (!s) return;
    var pp = document.getElementById('cfgProfilePics');
    if (pp) pp.checked = !!s.profilePicsEnabled;
    var qm = document.getElementById('cfgQueryMode');
    if (qm) qm.value = s.queryMode || 'single';
    var rl = document.getElementById('cfgRateLimit');
    if (rl) rl.value = s.rateLimit || 10;
    var sc = document.getElementById('cfgScatter');
    if (sc) sc.value = s.scatter || 6;
    var to = document.getElementById('cfgTimeout');
    if (to) to.value = s.timeout || 10;
    var rcs = document.getElementById('cfgResolverCacheShare');
    if (rcs) rcs.checked = !!s.resolverCacheShare;
  }).catch(function () { });
}

function switchSettingsTab(tab) {
  var tabs = ['display', 'connection', 'storage', 'backup', 'about'];
  tabs.forEach(function (name) {
    var btn = document.getElementById('settingsTabBtn' + name.charAt(0).toUpperCase() + name.slice(1));
    var panel = document.getElementById('settingsPanel' + name.charAt(0).toUpperCase() + name.slice(1));
    if (btn) {
      btn.classList.toggle('active', name === tab);
      if (name === tab) btn.scrollIntoView({ behavior: 'smooth', block: 'nearest', inline: 'nearest' });
    }
    if (panel) panel.style.display = name === tab ? '' : 'none';
  });
  if (tab === 'storage') loadCacheStats();
  var actions = document.getElementById('settingsActions');
  if (actions) {
    var save = actions.querySelector('[onclick="saveSettings()"]');
    if (tab === 'backup' || tab === 'about' || tab === 'storage') {
      if (save) save.style.display = 'none';
    } else {
      if (save) save.style.display = '';
    }
  }
}

function toggleHelp(id) {
  var el = document.getElementById(id);
  if (!el) return;
  el.style.display = el.style.display === 'none' ? '' : 'none';
}

// ===== PROFILE PICTURES =====
// Cached lowercase usernames so renderChannels can decide whether
// to overlay an <img> over the initial-letter circle.
var profilePicCache = { enabled: false, users: {} };
var profilePicsPollTimer = null;
// SSE throttle state — server fires one event per stored avatar.
var profilePicsReloadTimer = null;
var profilePicsLastReloadAt = 0;

function loadProfilePicState() {
  // no-store so SSE reloads see fresh data, not a cached entry.
  return fetch('/api/profile-pics', { cache: 'no-store' }).then(function (r) { return r.ok ? r.json() : null; }).then(function (d) {
    if (!d) return;
    profilePicCache.enabled = !!d.enabled;
    profilePicCache.users = {};
    (d.users || []).forEach(function (u) { profilePicCache.users[u.toLowerCase()] = true; });
    try { renderChannels(); } catch (e) { }
  }).catch(function () { });
}

function setProfilePicsEnabled(enabled) {
  // Toggle only controls the DNS path; cached GitHub avatars keep
  // displaying either way.
  fetch('/api/settings', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ profilePicsEnabled: !!enabled })
  }).then(function () {
    profilePicCache.enabled = !!enabled;
    if (enabled) profilePicsRefresh();
  }).catch(function () { });
}

function profilePicsRefresh() {
  var prog = document.getElementById('profilePicsProgress');
  if (prog) prog.textContent = t('profile_pics_starting') || 'Starting…';
  fetch('/api/profile-pics/refresh', { method: 'POST' }).then(function (r) {
    if (!r.ok) {
      if (prog) prog.textContent = t('profile_pics_failed') || 'Failed';
      return;
    }
    // Poll progress until inactive.
    if (profilePicsPollTimer) clearInterval(profilePicsPollTimer);
    profilePicsPollTimer = setInterval(function () {
      fetch('/api/profile-pics/progress').then(function (r) { return r.ok ? r.json() : null; }).then(function (p) {
        if (!p) return;
        if (prog) {
          if (p.active) {
            var label = (t('profile_pics_progress') || 'Refreshing avatars: {n}/{m}').replace('{n}', p.done || 0).replace('{m}', p.total || 0);
            prog.textContent = label + (p.username ? '  (@' + p.username + ')' : '');
          } else {
            prog.textContent = (t('profile_pics_done') || 'Done').replace('{n}', p.done || 0);
            clearInterval(profilePicsPollTimer);
            profilePicsPollTimer = null;
            loadProfilePicState();
          }
        }
      });
    }, 800);
  }).catch(function () { });
}

// Toggle the "show startup scan prompt" preference. Persists
// both client-side (localStorage for fast boot) and server-side
// (so Android keeps it across launches even when the WebView
// origin port changes).
function setShowScanPrompt(enabled) {
  if (enabled) {
    localStorage.removeItem('thefeed_scan_prompt_off');
    // Clear the per-session "already shown" flag so the prompt
    // re-appears on the next reload — without this, re-enabling
    // does nothing until the browser tab is closed.
    sessionStorage.removeItem('thefeed_scan_prompt_shown');
  } else {
    localStorage.setItem('thefeed_scan_prompt_off', '1');
  }
  try {
    fetch('/api/settings', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ scanPromptOff: !enabled })
    });
  } catch (e) { }
}
function closeSettings() { closeSettingsSection(); }

// ===== SETTINGS SECTION (reparented into the unified shell) =====
// Same pattern as the Resolver section: each settings group is a sidebar item;
// the panels + actions live in #settingsPane. Panels are MOVED out of the old
// modal once so all existing settings logic keeps working unchanged.

var _settingsReparented = false;
var settingsSectionOpen = false;
var settingsHistoryPushed = false;
var settingsSuppressPopstate = 0;
var settingsPanePushed = false;     // mobile: a settings section is open over the sidebar
var settingsTab = 'display';
var SETTINGS_TABS = ['display', 'connection', 'storage', 'backup', 'about'];

function settingsReparentOnce() {
  if (_settingsReparented) return;
  var content = document.getElementById('settingsContent');
  if (!content) return;
  SETTINGS_TABS.forEach(function (name) {
    var p = document.getElementById('settingsPanel' + name.charAt(0).toUpperCase() + name.slice(1));
    if (p) content.appendChild(p);
  });
  var actions = document.getElementById('settingsActions');
  if (actions) content.appendChild(actions);
  _settingsReparented = true;
}

function renderSettingsSidebar() {
  var sb = document.getElementById('settingsSidebar');
  if (!sb) return;
  var nameEl = document.getElementById('profileBtnName');
  var hasProfile = (typeof activeProfileId !== 'undefined' && activeProfileId);
  var profileName = (hasProfile && nameEl) ? (nameEl.textContent || '').trim() : '';
  var chev = '<span class="rs-chevron" aria-hidden="true">' + icon('chevron') + '</span>';
  var h = '<div class="rs-head">' + esc(t('settings') || 'Settings') + '</div>';
  // Profiles row: avatar tile (profile initial) + "Profiles" + the active
  // profile name as a trailing value + chevron — one line, no "+" button.
  var initial = profileName ? profileName.trim().charAt(0).toUpperCase() : '';
  h += '<button class="rs-item rs-item-set rs-item-profiles" onclick="openProfiles()">'
    + '<span class="rs-ic rs-ic-tile rs-tile-profiles">' + (initial || icon('profileToggle')) + '</span>'
    + '<span class="rs-name">' + esc(t('profiles') || 'Configs') + '</span>'
    + (profileName ? '<span class="rs-value">' + esc(profileName) + '</span>' : '')
    + chev + '</button>';
  h += '<div class="rs-label">' + esc(t('settings') || 'Settings') + '</div>';
  var labels = {
    display: t('settings_tab_display') || 'Display',
    connection: t('settings_tab_connection') || 'Connection',
    storage: t('settings_tab_storage') || 'Storage',
    backup: t('settings_tab_backup') || 'Backup',
    about: t('settings_tab_about') || 'About'
  };
  var icons = { display: 'settings', connection: 'antenna', storage: 'stats', backup: 'bank', about: 'info' };
  SETTINGS_TABS.forEach(function (name) {
    // Highlight the section currently shown in the pane (desktop two-pane: both
    // the list and the open section are visible, so the active one must stand out).
    var on = (name === settingsTab);
    h += '<button class="rs-item rs-item-set' + (on ? ' rs-item-active' : '') + '" onclick="settingsShowSection(\'' + name + '\',true)">'
      + '<span class="rs-ic rs-ic-tile rs-tile-' + name + '">' + icon(icons[name]) + '</span>'
      + '<span class="rs-name">' + esc(labels[name]) + '</span>' + chev + '</button>';
  });
  sb.innerHTML = h;
}

function settingsShowSection(tab, swapMobile) {
  settingsTab = tab;
  switchSettingsTab(tab);
  var titleEl = document.getElementById('settingsPaneTitle');
  if (titleEl) {
    var lbl = { display: 'settings_tab_display', connection: 'settings_tab_connection', storage: 'settings_tab_storage', backup: 'settings_tab_backup', about: 'settings_tab_about' }[tab];
    titleEl.textContent = t(lbl) || (t('settings') || 'Settings');
  }
  renderSettingsSidebar();
  if (swapMobile) settingsEnterPane();
}

// settingsEnterPane: on mobile, slide to the content pane AND push a history
// entry so hardware-back returns to the sidebar (not to the feed). Keyed off the
// flag, not .chat-open (core.js's popstate clears the class before ours runs).
function settingsEnterPane() {
  if (typeof mobileQuery === 'undefined' || !mobileQuery.matches) return;
  var app = document.getElementById('app'); if (app) app.classList.add('chat-open');
  if (!settingsPanePushed) {
    try { history.pushState({ view: 'settingsPane' }, ''); settingsPanePushed = true; } catch (e) { }
  }
}

function settingsShowSidebar() {
  var app = document.getElementById('app'); if (app) app.classList.remove('chat-open');
  if (settingsPanePushed) { settingsPanePushed = false; settingsSuppressPopstate++; try { history.go(-1); } catch (e) { } }
}

function openSettingsSection(adopt) {
  settingsReparentOnce();
  document.documentElement.classList.add('settings-section');
  settingsSectionOpen = true;
  if (adopt) {
    settingsHistoryPushed = true;
  } else if (!settingsHistoryPushed) {
    try { history.pushState({ view: 'settingsSection' }, ''); settingsHistoryPushed = true; } catch (e) { }
  }
  var app = document.getElementById('app'); if (app) app.classList.remove('chat-open');
  renderSettingsSidebar();
  settingsShowSection(settingsTab || 'display', false);
}

function settingsTeardown() {
  settingsSectionOpen = false;
  settingsPanePushed = false;
  document.documentElement.classList.remove('settings-section');
  var app = document.getElementById('app'); if (app) app.classList.remove('chat-open');
}

function settingsCloseForNav() {
  var had = settingsHistoryPushed;
  settingsHistoryPushed = false;
  settingsTeardown();
  return had;
}

function closeSettingsSection() {
  var steps = settingsHistoryPushed ? 1 : 0;
  settingsHistoryPushed = false;
  settingsTeardown();
  if (steps > 0) { settingsSuppressPopstate += steps; try { history.go(-steps); } catch (e) { } }
}

window.addEventListener('popstate', function () {
  if (settingsSuppressPopstate > 0) { settingsSuppressPopstate--; return; }
  // Mobile: back from a section pane returns to the sidebar (consume the pane
  // entry). Key off our flag — core.js clears .chat-open before this runs.
  if (settingsPanePushed) {
    settingsPanePushed = false;
    var app = document.getElementById('app'); if (app) app.classList.remove('chat-open');
    return;
  }
  if (settingsHistoryPushed) { settingsHistoryPushed = false; settingsTeardown(); }
});
async function saveSettings() {
  var fs = parseInt(document.getElementById('fontSizeSlider').value) || 14;
  var dbg = document.getElementById('cfgDebug').checked;
  var qm = document.getElementById('cfgQueryMode').value;
  var rl = parseInt(document.getElementById('cfgRateLimit').value, 10);
  var sc = parseInt(document.getElementById('cfgScatter').value);
  var to = parseFloat(document.getElementById('cfgTimeout').value);
  var body = { fontSize: fs, debug: dbg, queryMode: qm };
  if (rl > 0) body.rateLimit = rl;
  if (sc > 0) body.scatter = sc;
  if (to > 0) body.timeout = to;
  body.resolverCacheShare = document.getElementById('cfgResolverCacheShare').checked;
  try { await fetch('/api/settings', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) }) } catch (e) { }
}
// Every settings field auto-saves on change, so this is the shared handler.
var autoSaveSettings = saveSettings;

// ===== ANDROID SETTINGS =====

function initAndroidSettings() {
  if (typeof Android === 'undefined') return;
  document.getElementById('androidPasswordSection').style.display = '';
  // Show correct password section
  try {
    var hasPw = Android.hasPassword();
    document.getElementById('passwordSetSection').style.display = hasPw ? 'none' : '';
    document.getElementById('passwordRemoveSection').style.display = hasPw ? '' : 'none';
    if (!hasPw) document.getElementById('passwordRemovePrompt').style.display = 'none';
  } catch (e) { }
}

function showPasswordRemovePrompt() {
  document.getElementById('passwordRemovePrompt').style.display = '';
  document.getElementById('appPasswordCurrent').value = '';
  document.getElementById('passwordRemoveError').style.display = 'none';
}

function hidePasswordRemovePrompt() {
  document.getElementById('passwordRemovePrompt').style.display = 'none';
}

function setAppPassword() {
  if (typeof Android === 'undefined') return;
  var pw = document.getElementById('appPasswordInput').value;
  var confirm = document.getElementById('appPasswordConfirm').value;
  var errEl = document.getElementById('passwordError');
  errEl.style.display = 'none';
  if (!pw) { errEl.textContent = t('password_empty'); errEl.style.display = ''; return }
  if (pw !== confirm) { errEl.textContent = t('password_mismatch'); errEl.style.display = ''; return }
  try {
    var ok = Android.setPassword(pw);
    if (ok) {
      showToast(t('password_set_ok'));
      document.getElementById('appPasswordInput').value = '';
      document.getElementById('appPasswordConfirm').value = '';
      document.getElementById('passwordSetSection').style.display = 'none';
      document.getElementById('passwordRemoveSection').style.display = '';
    }
  } catch (e) { }
}

async function removeAppPassword() {
  if (typeof Android === 'undefined') return;
  var pw = document.getElementById('appPasswordCurrent').value;
  var errEl = document.getElementById('passwordRemoveError');
  errEl.style.display = 'none';
  if (!pw) { errEl.textContent = t('password_empty'); errEl.style.display = ''; return }
  if (!(await showConfirmDialog(t('password_remove_confirm')))) return;
  try {
    var ok = Android.removePassword(pw);
    if (ok) {
      showToast(t('password_removed'));
      document.getElementById('appPasswordCurrent').value = '';
      document.getElementById('passwordSetSection').style.display = '';
      document.getElementById('passwordRemoveSection').style.display = 'none';
      document.getElementById('passwordRemovePrompt').style.display = 'none';
    } else {
      errEl.textContent = t('password_wrong');
      errEl.style.display = '';
    }
  } catch (e) { }
}

function changeAppPassword() {
  if (typeof Android === 'undefined') return;
  var pw = document.getElementById('appPasswordCurrent').value;
  var errEl = document.getElementById('passwordRemoveError');
  errEl.style.display = 'none';
  if (!pw) { errEl.textContent = t('password_empty'); errEl.style.display = ''; return }
  try {
    var ok = Android.checkPassword(pw);
    if (!ok) {
      errEl.textContent = t('password_wrong');
      errEl.style.display = '';
      return;
    }
    // Remove old and show set section for new password
    Android.removePassword(pw);
    document.getElementById('appPasswordCurrent').value = '';
    document.getElementById('passwordRemoveSection').style.display = 'none';
    document.getElementById('passwordRemovePrompt').style.display = 'none';
    document.getElementById('passwordSetSection').style.display = '';
    showToast(t('password_enter_new'));
  } catch (e) { }
}

// checkGitHubUpdate hits /api/update/github (which reads the latest
// GitHub Release tag for sartoopjj/thefeed) and prompts the user
// with a download link tailored to their platform.
// `manual=true` shows a toast on "no update", `manual=false` stays silent.
async function checkGitHubUpdate(manual) {
  if (storeBuild) return; // store builds update through the store
  try {
    var r = await fetch('/api/update/github');
    if (!r.ok) {
      if (manual) showToast(t('update_check_failed') || 'Update check failed');
      return;
    }
    var data = await r.json();
    if (!data || !data.latest) return;
    latestVersion = data.latest;
    renderLatestVersion();
    if (data.hasUpdate && data.downloadURL) {
      if (!manual) {
        // Server-stored state survives per-port localStorage wipes.
        var skipped = '', downloaded = '';
        try {
          var sx = new XMLHttpRequest();
          sx.open('GET', '/api/settings', false);
          sx.send();
          if (sx.status === 200) {
            var sv = JSON.parse(sx.responseText);
            skipped = sv.skipUpdateVersion || '';
            downloaded = sv.downloadedVersion || '';
          }
        } catch (e) { }
        if (!skipped) skipped = localStorage.getItem('thefeed_skip_gh_update_' + normalizeVersion(data.latest)) === '1' ? data.latest : '';
        if (!downloaded) downloaded = localStorage.getItem('thefeed_downloaded_gh_update_' + normalizeVersion(data.latest)) === '1' ? data.latest : '';
        if (normalizeVersion(skipped) === normalizeVersion(data.latest)) return;
        // Already saved but evidently not installed (we are still running the
        // old build): remind how to finish rather than offering the download
        // again. Shown every launch, like the normal prompt — "don't show
        // again" is the way out. A once-per-session guard lived here and was
        // wrong: sessionStorage outlives app restarts while the browser tab
        // stays open, so the reminder appeared once and never again.
        if (normalizeVersion(downloaded) === normalizeVersion(data.latest)) {
          showUpdateReminder(data.latest, data.downloadURL);
          return;
        }
      }
      showUpdateDialog(data.latest, data.downloadURL);
    } else if (manual) {
      showToast((t('version_up_to_date') || 'Up to date: {v}').replace('{v}', data.latest));
    }
  } catch (e) {
    if (manual) showToast(e.message || t('update_check_failed') || 'Update check failed');
  }
}

function showUpdateDialog(newVersion, url) {
  var msg = (t('update_available') || 'New version available: {v}').replace('{v}', newVersion);
  var hint = t('update_download_hint') || 'Download the new version below.';
  var dl = t('update_download_btn') || 'Download';
  var later = t('update_later_btn') || 'Later';
  var skip = t('update_skip_btn') || "Don't show again";
  var overlay = document.createElement('div');
  overlay.className = 'modal-overlay active';
  overlay.innerHTML = '<div class="modal" style="max-width:380px">'
    + '<h2 style="margin-top:0">' + esc(msg) + '</h2>'
    + '<p style="font-size:13px;color:var(--text-dim);margin-bottom:12px;line-height:1.6">' + esc(hint) + '</p>'
    + '<p style="font-size:11px;color:var(--text-dim);margin-bottom:16px;word-break:break-all"><code>' + esc(url) + '</code></p>'
    + '<div class="modal-actions" style="flex-wrap:wrap;gap:6px">'
    + '  <button class="btn btn-flat" id="updateSkip">' + esc(skip) + '</button>'
    + '  <button class="btn btn-flat" id="updateLater">' + esc(later) + '</button>'
    + '  <button class="btn btn-primary" id="updateDownload">' + esc(dl) + '</button>'
    + '</div></div>';
  document.body.appendChild(overlay);
  var dismiss = function () { if (overlay.parentNode) overlay.parentNode.removeChild(overlay); };
  // Scoped to this overlay: nothing stops a second dialog carrying the same ids.
  overlay.querySelector('#updateLater').onclick = dismiss;
  overlay.querySelector('#updateSkip').onclick = function () { persistUpdateSkip(newVersion); dismiss(); };
  overlay.querySelector('#updateDownload').onclick = function () {
    runUpdateDownload(newVersion, overlay);
  };
}

// ===== ONE-TIME DATA MIGRATIONS =====
// Append-only; index+1 is the migration number. Each entry mutates the
// settings patch POSTed once all pending steps have run.
var TF_MIGRATIONS = [
  // 1 — the old download path wrote skipUpdateVersion (the "don't show again"
  // field) to mean "downloaded", hiding that version's update for good. A real
  // "don't show again" is cleared too; the two are indistinguishable now.
  function (patch) {
    try {
      Object.keys(localStorage)
        .filter(function (k) { return k.indexOf('thefeed_skip_gh_update_') === 0; })
        .forEach(function (k) { localStorage.removeItem(k); });
    } catch (e) { }
    patch.skipUpdateVersion = '';
  }
];

function tfLocalMigrationLevel() {
  try { return parseInt(localStorage.getItem('thefeed_migration_level') || '0', 10) || 0; }
  catch (e) { return 0; }
}

// runPendingMigrations applies everything above this install's level, and
// resolves once done so callers reading the migrated values can wait.
// Level lives on the server for a single-user client (its loopback port
// changes and wipes localStorage), but per-browser on a shared backend, where
// one profiles.json serves everyone yet each browser has its own localStorage.
function runPendingMigrations(s) {
  if (!s) return Promise.resolve();
  var shared = !!s.shared;
  var applied = shared ? tfLocalMigrationLevel() : (s.migrationVersion || 0);
  if (applied >= TF_MIGRATIONS.length) return Promise.resolve();
  var patch = { migrationVersion: TF_MIGRATIONS.length };
  for (var i = applied; i < TF_MIGRATIONS.length; i++) {
    try { TF_MIGRATIONS[i](patch); } catch (e) { }
  }
  if (shared) {
    try { localStorage.setItem('thefeed_migration_level', String(TF_MIGRATIONS.length)); } catch (e) { }
    return Promise.resolve();
  }
  return fetch('/api/settings', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(patch)
  }).catch(function () { });
}

// pruneVersionKeys drops every <prefix><version> entry except `keep` — only
// the newest is ever read, so the rest would accumulate one per release.
function pruneVersionKeys(prefix, keep) {
  try {
    Object.keys(localStorage).forEach(function (k) {
      if (k.indexOf(prefix) === 0 && k !== keep) localStorage.removeItem(k);
    });
  } catch (e) { }
}

// persistUpdateSkip records an explicit "don't show again" — permanent for
// that version. Local first, so it sticks even if the API call fails.
function persistUpdateSkip(newVersion) {
  var key = 'thefeed_skip_gh_update_' + normalizeVersion(newVersion);
  pruneVersionKeys('thefeed_skip_gh_update_', key);
  try { localStorage.setItem(key, '1'); } catch (e) { }
  fetch('/api/settings', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ skipUpdateVersion: newVersion })
  }).catch(function () { });
}

// persistUpdateDownloaded records that the asset was saved — deliberately not
// the skip flag, so a download the user never installs is still remindable.
function persistUpdateDownloaded(newVersion) {
  var key = 'thefeed_downloaded_gh_update_' + normalizeVersion(newVersion);
  pruneVersionKeys('thefeed_downloaded_gh_update_', key);
  try { localStorage.setItem(key, '1'); } catch (e) { }
  fetch('/api/settings', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ downloadedVersion: newVersion })
  }).catch(function () { });
}

// updateSaveHint describes where this shell puts the file: Android streams it
// to Downloads/, iOS asks the user to pick, desktop uses the browser folder.
function updateSaveHint() {
  return androidBridge ? t('update_done_android')
    : (typeof window !== 'undefined' && window.IOS) ? t('update_done_ios')
      : t('update_done_desktop');
}

// showUpdateReminder runs when this version was already downloaded but the
// app is still the old build — so the file was never installed.
function showUpdateReminder(newVersion, url) {
  var overlay = document.createElement('div');
  overlay.className = 'modal-overlay active';
  overlay.innerHTML = '<div class="modal" style="max-width:380px">'
    + '<h2 style="margin-top:0">' + esc(t('update_ready_title').replace('{v}', newVersion)) + '</h2>'
    + '<p style="font-size:13px;color:var(--text-dim);margin-bottom:16px;line-height:1.6">' + esc(updateSaveHint()) + '</p>'
    + '<div class="modal-actions" style="flex-wrap:wrap;gap:6px">'
    + '  <button class="btn btn-flat" id="updateSkip">' + esc(t('update_skip_btn')) + '</button>'
    + '  <button class="btn btn-flat" id="updateLater">' + esc(t('update_later_btn')) + '</button>'
    + '  <button class="btn btn-primary" id="updateRedownload">' + esc(t('update_download_again')) + '</button>'
    + '</div></div>';
  document.body.appendChild(overlay);
  var dismiss = function () { if (overlay.parentNode) overlay.parentNode.removeChild(overlay); };
  overlay.querySelector('#updateLater').onclick = dismiss;
  overlay.querySelector('#updateSkip').onclick = function () { persistUpdateSkip(newVersion); dismiss(); };
  overlay.querySelector('#updateRedownload').onclick = function () {
    runUpdateDownload(newVersion, overlay);
  };
}

// showUpdateDone replaces the progress panel with a completion notice, kept
// open until dismissed because installing the file is a manual step.
function showUpdateDone(overlay, savedName) {
  var modal = overlay && overlay.querySelector('.modal');
  if (!modal) return;
  var hint = updateSaveHint();
  modal.innerHTML = ''
    + '<h2 style="margin-top:0">' + esc(t('update_done_title')) + '</h2>'
    + '<p style="color:var(--text-dim);font-size:13px;margin:0 0 10px;line-height:1.6">' + esc(hint) + '</p>'
    + (savedName
      ? '<div style="font-family:monospace;font-size:12px;word-break:break-all;background:var(--bg-soft,#222);padding:8px 10px;border-radius:6px">'
        + esc(savedName) + '</div>'
      : '')
    + '<div class="modal-actions" style="margin-top:14px;justify-content:flex-end">'
    + '  <button class="btn btn-primary" id="updateDoneClose">' + esc(t('close')) + '</button>'
    + '</div>';
  // Scoped to this modal: a second update dialog would carry the same id, and
  // document.getElementById would hand back the older one's button.
  modal.querySelector('#updateDoneClose').onclick = function () {
    if (overlay.parentNode) overlay.parentNode.removeChild(overlay);
  };
}

// runUpdateDownload swaps the update dialog's body for a progress
// bar, streams the asset through /api/update/download, and on
// completion shows the install notice.
async function runUpdateDownload(newVersion, overlay) {
  var modal = overlay && overlay.querySelector('.modal');
  var progressHTML = ''
    + '<h2 style="margin-top:0">' + esc(t('update_downloading').replace('{v}', newVersion)) + '</h2>'
    + '<div style="background:var(--bg-soft,#222);height:10px;border-radius:5px;overflow:hidden;margin-bottom:8px">'
    + '  <div id="updateProgressBar" style="background:var(--accent,#4caf50);height:100%;width:0%;transition:width .2s"></div>'
    + '</div>'
    + '<div id="updateProgressText" style="font-size:12px;color:var(--text-dim);text-align:center">0 / ?</div>'
    + '<div class="modal-actions" style="margin-top:14px;justify-content:flex-end">'
    + '  <button class="btn btn-flat" id="updateCancel">' + esc(t('cancel')) + '</button>'
    + '</div>';
  if (!modal) return;
  modal.innerHTML = progressHTML;

  // Scoped to this modal: a second update dialog carries the same ids.
  var cancelBtn = modal.querySelector('#updateCancel');
  var bar = modal.querySelector('#updateProgressBar');
  var txt = modal.querySelector('#updateProgressText');

  var controller = new AbortController();
  cancelBtn.onclick = function () {
    controller.abort();
    if (overlay.parentNode) overlay.parentNode.removeChild(overlay);
  };

  var showFailed = function (detail) {
    if (!txt) return;
    txt.style.color = 'var(--danger,#e53935)';
    txt.textContent = t('update_download_failed') + (detail ? ': ' + detail : '');
  };

  // Android: let the native side stream the APK straight to Downloads.
  // Buffering it in JS and handing it over as base64 peaked at ~8x the file
  // size (~210 MB for the 25 MB universal APK), which could freeze low-RAM
  // devices. The native path holds one 64 KB buffer; we just poll progress.
  if (androidBridge && androidBridge.startDownload && androidBridge.downloadStatus) {
    var updURL = location.origin + '/api/update/download?version=' + encodeURIComponent(newVersion);
    var updName = 'thefeed-' + newVersion + '.apk';
    if (!androidBridge.startDownload(updURL, updName)) {
      showFailed('');
      return;
    }
    var pollFails = 0;
    var poll = setInterval(function () {
      var st;
      try { st = JSON.parse(androidBridge.downloadStatus() || '{}'); }
      catch (e) {
        // A bridge that keeps throwing would otherwise poll for the whole
        // session; give it a few seconds, then report the failure.
        if (++pollFails < 20) return;
        st = { state: 'error', detail: e.message || String(e) };
      }
      pollFails = 0;
      if (st.state === 'running') {
        if (st.total > 0) {
          var p = Math.min(100, (st.received / st.total) * 100);
          if (bar) bar.style.width = p.toFixed(1) + '%';
          if (txt) txt.textContent = fmtBytes(st.received) + ' / ' + fmtBytes(st.total) + ' (' + p.toFixed(0) + '%)';
        } else if (txt) {
          txt.textContent = fmtBytes(st.received || 0);
        }
        return;
      }
      clearInterval(poll);
      if (st.state === 'done') {
        if (bar) bar.style.width = '100%';
        persistUpdateDownloaded(newVersion);
        // st.detail is the name the native side actually wrote, which may
        // differ from updName when the server supplies an ABI-tagged one.
        showUpdateDone(overlay, st.detail || updName);
      } else if (st.state === 'canceled') {
        // User-initiated; the dialog is normally already gone.
        if (txt) txt.textContent = t('update_canceled');
      } else {
        // Anything that is not running or done — including an unrecognised
        // state — must report, or the bar freezes with no explanation.
        showFailed(st.detail || '');
      }
    }, 300);
    cancelBtn.onclick = function () {
      clearInterval(poll);
      // Stop the native transfer too — without this it ran to completion and
      // toasted "Saved to Downloads/…" after the dialog had gone.
      try { if (androidBridge.cancelDownload) androidBridge.cancelDownload(); } catch (e) { }
      if (overlay.parentNode) overlay.parentNode.removeChild(overlay);
    };
    return;
  }

  try {
    var resp = await fetch('/api/update/download?version=' + encodeURIComponent(newVersion), {
      signal: controller.signal,
    });
    if (!resp.ok) {
      var errText = await resp.text();
      throw new Error(errText || ('HTTP ' + resp.status));
    }
    var total = parseInt(resp.headers.get('Content-Length') || '0', 10);
    var filename = resp.headers.get('X-Download-Filename') || ('thefeed-' + newVersion);

    var blob;
    if (resp.body && resp.body.getReader) {
      var reader = resp.body.getReader();
      var chunks = [];
      var received = 0;
      while (true) {
        var step = await reader.read();
        if (step.done) break;
        chunks.push(step.value);
        received += step.value.length;
        if (total > 0) {
          var pct = Math.min(100, (received / total) * 100);
          if (bar) bar.style.width = pct.toFixed(1) + '%';
          if (txt) txt.textContent = fmtBytes(received) + ' / ' + fmtBytes(total)
            + ' (' + pct.toFixed(0) + '%)';
        } else {
          if (txt) txt.textContent = fmtBytes(received);
        }
      }
      blob = new Blob(chunks, { type: 'application/octet-stream' });
      chunks.length = 0; // the Blob owns a copy; drop the second one
    } else {
      // Fallback for old WebViews without ReadableStream. No
      // progress — just wait for the whole response, then save.
      if (txt) txt.textContent = t('update_downloading_no_progress');
      if (bar) bar.style.width = '100%';
      blob = await resp.blob();
    }

    // Both native WebViews ignore <a download> for blob URLs; triggerDownload
    // picks the bridge and reports whether the save actually landed.
    if (txt) txt.textContent = t('update_saving');
    if (!await triggerDownload(blob, filename)) {
      showFailed('');
      return;
    }

    persistUpdateDownloaded(newVersion);
    showUpdateDone(overlay, filename);
  } catch (e) {
    if (e && e.name === 'AbortError') return;
    showFailed(e.message || e);
    showToast(t('update_download_failed') + ': ' + (e.message || e));
  }
}

async function checkLatestVersion() {
  var btn = document.getElementById('checkVersionBtn');
  var prevText = btn ? btn.textContent : '';
  if (btn) {
    btn.disabled = true;
    btn.textContent = t('checking_version');
  }
  try {
    var r = await fetch('/api/version-check', { method: 'POST' });
    var text = await r.text();
    var data = {};
    try { data = JSON.parse(text) } catch (e) { }
    if (!r.ok) {
      showToast(text || t('version_check_failed'));
      return;
    }
    latestVersion = data.latestVersion || '';
    renderLatestVersion();
    if (!latestVersion) {
      showToast(t('version_check_failed'));
      return;
    }
    if (compareSemver(latestVersion, appVersion) > 0) maybeWarnNewVersion();
    else showToast(t('version_up_to_date').replace('{v}', latestVersion));
  } catch (e) {
    showToast(e.message || t('version_check_failed'));
  } finally {
    if (btn) {
      btn.disabled = false;
      btn.textContent = prevText || t('check_now');
    }
  }
}
// ===== STORAGE / CACHE BUDGET =====

function fmtBytes(n) {
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
  if (n < 1024 * 1024 * 1024) return (n / 1024 / 1024).toFixed(1) + ' MB';
  return (n / 1024 / 1024 / 1024).toFixed(2) + ' GB';
}

// Every id the backend reports needs a row in all three maps below. A missing
// one is not a blank space: the label falls back to the server's English
// string (so a Persian user reads English), the tile loses its icon, and the
// bar turns grey. "telegram" was added to the backend without them.
var _storageLabelMap = {
  'media': 'storage_media',
  'telemirror': 'storage_telemirror',
  'saved-media': 'storage_saved_media',
  'messages': 'storage_messages',
  'telegram': 'storage_telegram',
};
var _storageIconMap = {
  'media': '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" style="width:22px;height:22px"><rect x="3" y="3" width="18" height="18" rx="2"/><circle cx="8.5" cy="8.5" r="1.5"/><path d="m21 15-5-5L5 21"/></svg>',
  'telemirror': '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" style="width:22px;height:22px"><path d="m22 2-7 20-4-9-9-4z"/><path d="M22 2 11 13"/></svg>',
  'saved-media': '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" style="width:22px;height:22px"><path d="m19 21-7-4-7 4V5a2 2 0 0 1 2-2h10a2 2 0 0 1 2 2v16z"/></svg>',
  'messages': '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" style="width:22px;height:22px"><path d="M7.9 20A9 9 0 1 0 4 16.1L2 22z"/></svg>',
  // The Telegram tab's own icon, so the row and the tab read as the same
  // thing. TeleMirror's plane is a different shape on purpose: that one is
  // public channels read without an account, this one is the account itself.
  'telegram': '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" style="width:22px;height:22px"><path d="M21.854 2.147l-10.94 10.939"/><path d="M21.854 2.147L17.5 20.5a.5.5 0 0 1-.927.07l-3.66-7.23-7.23-3.66a.5.5 0 0 1 .07-.927z"/></svg>',
};
var _storageColorMap = {
  'media': '#3b82f6',
  'telemirror': '#8b5cf6',
  'saved-media': '#f59e0b',
  'messages': '#10b981',
  'telegram': '#0ea5e9',
};

function loadCacheStats() {
  fetch('/api/cache/stats').then(function (r) { return r.ok ? r.json() : null; }).then(function (d) {
    if (!d) return;
    var el = document.getElementById('storageTotalEl');
    if (el) el.textContent = fmtBytes(d.total || 0);
    var inp = document.getElementById('storageBudgetInput');
    if (inp) inp.value = d.budgetMB || 1024;
    var list = document.getElementById('storageCacheList');
    if (!list) return;
    var total = d.total || 1;
    var html = '';
    (d.caches || []).forEach(function (c) {
      var canDelete = c.id !== 'saved-media';
      var label = _storageLabelMap[c.id] ? t(_storageLabelMap[c.id]) : c.label;
      var ico = _storageIconMap[c.id] || '';
      var clr = _storageColorMap[c.id] || 'var(--text-dim)';
      var pct = total > 0 ? Math.max(1, Math.round((c.bytes / total) * 100)) : 0;
      if (c.bytes === 0) pct = 0;
      html += '<div style="display:flex;align-items:center;gap:12px;padding:12px 0;border-bottom:1px solid var(--border)">'
        + '<div style="width:40px;height:40px;border-radius:10px;background:' + clr + '18;color:' + clr + ';display:flex;align-items:center;justify-content:center;flex-shrink:0">' + ico + '</div>'
        + '<div style="flex:1;min-width:0">'
        + '<div style="font-size:14px;color:var(--text);margin-bottom:3px">' + esc(label) + '</div>'
        + '<div style="height:4px;border-radius:2px;background:var(--border);overflow:hidden"><div style="height:100%;width:' + pct + '%;background:' + clr + ';border-radius:2px"></div></div>'
        + '</div>'
        + '<div style="display:flex;align-items:center;gap:6px;flex-shrink:0">'
        + '<span dir="ltr" style="font-size:13px;color:var(--text-dim)">' + fmtBytes(c.bytes) + '</span>'
        + (canDelete ? '<button class="btn btn-flat btn-sm" onclick="clearOneCache(\'' + escAttr(c.id) + '\')" style="color:var(--danger,#e74c3c);padding:4px 6px;font-size:11px;line-height:0">'
          + '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" style="width:16px;height:16px"><polyline points="3,6 5,6 21,6"/><path d="M19 6l-1 14a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2L5 6"/><path d="M10 11v6"/><path d="M14 11v6"/></svg></button>' : '')
        + '</div></div>';
    });
    list.innerHTML = html;
  }).catch(function () { });
}

function clearOneCache(which) {
  fetch('/api/cache/clear-one', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ which: which })
  }).then(function (r) {
    if (r.ok) {
      showToast(t('cache_cleared') || 'Cleared');
      loadCacheStats();
    }
  }).catch(function () { });
}

function saveCacheBudget() {
  var inp = document.getElementById('storageBudgetInput');
  var mb = parseInt(inp && inp.value, 10);
  if (!mb || mb < 50) { showToast(t('storage_budget_min') || 'Minimum 50 MB'); return; }
  fetch('/api/cache/budget', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ mb: mb })
  }).then(function (r) {
    if (r.ok) {
      showToast(t('saved') || 'Saved');
      loadCacheStats();
    }
  }).catch(function () { });
}

function toggleResolverBankInfo() {
  var panel = document.getElementById('resolverBankInfoPanel');
  var btn = document.getElementById('resolverBankInfoBtn');
  if (!panel || !btn) return;
  var open = panel.hasAttribute('hidden');
  if (open) panel.removeAttribute('hidden');
  else panel.setAttribute('hidden', '');
  btn.setAttribute('aria-expanded', open ? 'true' : 'false');
}

// ── Backup & Restore ──

function doBackupExport() {
  var pw = document.getElementById('bkExportPw').value;
  if (!pw) { showToast(t('backup_password_required') || 'Enter a password'); return; }
  var sections = [];
  if (document.getElementById('bkProfiles').checked) sections.push('profiles');
  if (document.getElementById('bkChat').checked) sections.push('chat');
  if (document.getElementById('bkSaved').checked) sections.push('saved');
  if (document.getElementById('bkMedia').checked) sections.push('savedMedia');
  if (document.getElementById('bkTelegram').checked) sections.push('telegram');
  if (document.getElementById('bkTelegramCache').checked) sections.push('telegramCache');
  if (document.getElementById('bkBgImage').checked) sections.push('bgImage');
  if (!sections.length) { showToast(t('backup_select_section') || 'Select at least one section'); return; }

  var btn = document.getElementById('bkExportBtn');
  btn.disabled = true;
  var origText = btn.textContent;
  btn.textContent = '...';

  fetch('/api/backup/export', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ password: pw, sections: sections })
  }).then(function (r) {
    if (r.status === 423) throw new Error(t('backup_saved_locked') || 'Unlock saved messages first');
    if (!r.ok) return r.text().then(function (t) { throw new Error(t); });
    return r.blob();
  }).then(function (blob) {
    triggerDownload(blob, 'thefeed-backup.tfbak');
    // Native bridges finish the save themselves (Downloads toast on Android,
    // a Save-to-Files picker on iOS/macOS), so claiming success here would be
    // premature — and wrong if the user cancels the picker.
    if (!(window.Android || window.IOS)) {
      showToast(t('backup_exported') || 'Backup exported');
    }
  }).catch(function (e) {
    showToast(e.message || 'Export failed');
  }).finally(function () {
    btn.disabled = false;
    btn.textContent = origText;
    document.getElementById('bkExportPw').value = '';
  });
}

function doBackupPreview() {
  var fileEl = document.getElementById('bkImportFile');
  var pw = document.getElementById('bkImportPw').value;
  if (!fileEl.files.length) { showToast(t('backup_select_file') || 'Select a backup file'); return; }
  if (!pw) { showToast(t('backup_password_required') || 'Enter a password'); return; }

  var fd = new FormData();
  fd.append('file', fileEl.files[0]);
  fd.append('password', pw);

  fetch('/api/backup/preview', { method: 'POST', body: fd })
    .then(function (r) {
      if (r.status === 401) throw new Error(t('backup_wrong_password') || 'Wrong password');
      if (!r.ok) return r.text().then(function (t) { throw new Error(t); });
      return r.json();
    })
    .then(function (d) { renderBackupPreview(d); })
    .catch(function (e) { showToast(e.message || 'Preview failed'); });
}

function renderBackupPreview(d) {
  var el = document.getElementById('bkPreviewResult');
  if (!el) return;
  var inputs = document.getElementById('bkImportInputs');
  if (inputs) inputs.style.display = 'none';

  var html = '<div style="padding:12px;background:var(--bg);border-radius:8px;border:1px solid var(--border)">';

  if (d.createdAt) {
    var dt = new Date(d.createdAt * 1000);
    html += '<div style="font-size:12px;color:var(--text-dim);margin-bottom:8px">' + esc(dt.toLocaleString()) + '</div>';
  }

  var items = [];
  if (d.profiles) {
    items.push({ id: 'profiles', label: t('backup_profiles') || 'Configs & Settings',
      detail: d.profiles.count + ' — ' + (d.profiles.names || []).join(', ') });
  }
  if (d.chat) {
    var parts = [];
    if (d.chat.identity) parts.push(t('backup_identity') || 'Identity');
    if (d.chat.contacts) parts.push(d.chat.contacts + ' ' + (t('backup_contacts') || 'contacts'));
    if (d.chat.threads) parts.push(d.chat.threads + ' ' + (t('backup_threads') || 'threads'));
    if (d.chat.messages) parts.push(d.chat.messages + ' ' + (t('backup_msgs') || 'messages'));
    items.push({ id: 'chat', label: t('backup_chat') || 'Chat', detail: parts.join(', ') });
  }
  if (d.saved) {
    items.push({ id: 'saved', label: t('backup_saved') || 'Saved Messages',
      detail: d.saved.count + ' ' + (t('backup_items') || 'items') });
  }
  if (d.savedMedia) {
    items.push({ id: 'savedMedia', label: t('backup_media') || 'Saved Media',
      detail: d.savedMedia.count + ' ' + (t('backup_files') || 'files') + ' (' + fmtBytes(d.savedMedia.bytes) + ')' });
  }
  if (d.bgImage) {
    items.push({ id: 'bgImage', label: t('backup_bg') || 'Background Image', detail: fmtBytes(d.bgImage.bytes) });
  }
  if (d.telegramCache) {
    items.push({ id: 'telegramCache', label: t('backup_telegram_cache') || 'Telegram messages',
      detail: fmtBytes(d.telegramCache.bytes) });
  }
  if (d.telegram) {
    items.push({ id: 'telegram', label: t('backup_telegram') || 'Telegram session',
      detail: fmtBytes(d.telegram.bytes) + ' — ' + (t('backup_telegram_warn') || '') });
  }

  items.forEach(function (it) {
    html += '<label style="display:flex;align-items:flex-start;gap:8px;padding:6px 0;font-size:13px;color:var(--text);border-bottom:1px solid var(--border)">'
      + '<input type="checkbox" class="bk-restore-section" value="' + it.id + '" checked style="margin-top:2px">'
      + '<span style="flex:1;min-width:0"><span style="display:block">' + esc(it.label) + '</span>'
      + '<span style="display:block;font-size:11px;color:var(--text-dim);word-break:break-word">' + esc(it.detail) + '</span></span>'
      + '</label>';
  });

  html += '</div>';
  html += '<div style="display:flex;gap:8px;margin-top:10px">'
    + '<button class="btn btn-outline" onclick="hideBackupPreview()" style="flex:1">'
    + (t('cancel') || 'Cancel') + '</button>'
    + '<button class="btn btn-primary" id="bkRestoreBtn" onclick="doBackupRestore()" style="flex:1">'
    + (t('backup_restore_btn') || 'Restore') + '</button>'
    + '</div>';

  el.innerHTML = html;
  el.style.display = '';
}

function hideBackupPreview() {
  var el = document.getElementById('bkPreviewResult');
  if (el) { el.style.display = 'none'; el.innerHTML = ''; }
  var inputs = document.getElementById('bkImportInputs');
  if (inputs) inputs.style.display = '';
}

async function doBackupRestore() {
  var fileEl = document.getElementById('bkImportFile');
  var pw = document.getElementById('bkImportPw').value;
  if (!fileEl.files.length || !pw) return;

  var checks = document.querySelectorAll('.bk-restore-section:checked');
  var sections = [];
  for (var i = 0; i < checks.length; i++) sections.push(checks[i].value);
  if (!sections.length) { showToast(t('backup_select_section') || 'Select at least one section'); return; }

  if (!(await showConfirmDialog(t('backup_restore_confirm') || 'This will overwrite current data. Continue?'))) return;

  var btn = document.getElementById('bkRestoreBtn');
  if (btn) { btn.disabled = true; btn.textContent = '...'; }

  var fd = new FormData();
  fd.append('file', fileEl.files[0]);
  fd.append('password', pw);
  fd.append('sections', JSON.stringify(sections));

  fetch('/api/backup/restore', { method: 'POST', body: fd })
    .then(function (r) {
      if (r.status === 401) throw new Error(t('backup_wrong_password') || 'Wrong password');
      if (r.status === 423) throw new Error(t('backup_saved_locked') || 'Unlock saved messages first');
      if (!r.ok) return r.text().then(function (t) { throw new Error(t); });
      return r.json();
    })
    .then(function () {
      showToast(t('backup_restored') || 'Backup restored');
      setTimeout(function () { location.reload(); }, 1000);
    })
    .catch(function (e) {
      showToast(e.message || 'Restore failed');
      if (btn) { btn.disabled = false; btn.textContent = t('backup_restore_btn') || 'Restore'; }
    });
}

