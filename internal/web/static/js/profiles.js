// ===== PROFILES =====
async function loadProfiles() {
  try {
    var r = await fetch('/api/profiles'); profiles = await r.json();
    activeProfileId = profiles.active || '';
    renderProfileBtn();
  } catch (e) { profiles = null }
}

function renderProfileBtn() {
  var nameEl = document.getElementById('profileBtnName');
  var avatarEl = document.getElementById('profileBtnAvatar');
  if (!profiles || !profiles.profiles || !profiles.profiles.length) {
    nameEl.textContent = t('set_up'); avatarEl.textContent = '?'; return
  }
  var active = profiles.profiles.find(function (p) { return p.id === activeProfileId });
  var display = (active && (active.nickname || active.config.domain)) || t('profiles');
  nameEl.textContent = display;
  avatarEl.textContent = (active && (active.nickname || active.config.domain) || '?').charAt(0).toUpperCase();
  // Keep the reparented Settings sidebar's Profiles row name in sync.
  if (typeof renderSettingsSidebar === 'function') { try { renderSettingsSidebar(); } catch (e) { } }
}

// linkedConfig is the config a #thefeed:// link carried, for the import form.
function linkedConfig() {
  var h = location.hash;
  if (!h || !h.startsWith('#thefeed://')) return '';
  try { return decodeURIComponent(h.substring(1)) } catch (e) { return '' }
}

function openProfiles() {
  document.getElementById('profilesModal').classList.add('active');
  document.getElementById('importError').style.display = 'none';
  document.getElementById('importSuccess').style.display = 'none';
  // Start-up opens this again for a new user, so the link is read each time.
  document.getElementById('importUriInput').value = linkedConfig();
  renderProfilesModal();
  // New users (no profiles yet): surface the ready-to-use default configs
  // immediately so the quickest onboarding path is the most visible one.
  var noProfiles = !profiles || !profiles.profiles || !profiles.profiles.length;
  var dcList = document.getElementById('defaultConfigsList');
  if (noProfiles && dcList && dcList.style.display === 'none') toggleDefaultConfigs();
}
function closeProfiles() { document.getElementById('profilesModal').classList.remove('active') }

function buildProfileUri(id, selectedResolvers) {
  if (!profiles || !profiles.profiles) return '';
  var p = profiles.profiles.find(function (x) { return x.id === id });
  if (!p || !p.config.domain) return '';
  var resolvers = selectedResolvers || [];
  // Include the user-set nickname so the recipient lands on a
  // pre-named profile instead of "thefeed.example.com" pulled
  // from the domain. Capped at 32 chars on this side; the
  // import side enforces the same cap defensively.
  // Drop the default ":53" suffix from each resolver — the parser
  // assumes 53 when no port is given, so omitting it shortens the
  // URI considerably. Custom ports are kept and stay un-encoded.
  var compact = resolvers.map(function (r) {
    return r.replace(/:53$/, '');
  }).join(',');
  // Order matters: resolvers (r=) go LAST so a truncated URI (long resolver
  // list, lost tail) only drops trailing resolvers — domain, key, n=, sk=
  // all survive.
  var uri = 'thefeed://' + encodeURIComponent(p.config.domain)
    + '/' + encodeURIComponent(p.config.key) + '?';
  var params = [];
  var nick = (p.nickname || '').trim().slice(0, 32);
  if (nick && nick !== p.config.domain) {
    params.push('n=' + encodeURIComponent(nick));
  }
  // Pinned server signing key (base64url — already URI-safe) so the
  // recipient can verify feed content.
  if (p.config.serverKey) {
    params.push('sk=' + p.config.serverKey);
  }
  // Extra sub-domains (comma-separated). Kept before r= so resolvers stay last.
  if (p.config.extraDomains && p.config.extraDomains.length) {
    params.push('d=' + encodeURIComponent(p.config.extraDomains.join(',')));
  }
  params.push('r=' + encodeURIComponent(compact).replace(/%3A/g, ':'));
  uri += params.join('&');
  return uri;
}

// serverKeyByteLen decodes a base64url (or base64) server key and returns its
// byte length, or -1 if it isn't valid base64. A valid ed25519 key is 32 bytes.
function serverKeyByteLen(s) {
  if (!s) return 0;
  var b = String(s).replace(/-/g, '+').replace(/_/g, '/');
  while (b.length % 4) b += '=';
  try { return atob(b).length; } catch (e) { return -1; }
}

// serverKeyState classifies a pinned key: 'none' (empty), 'ok' (32 bytes),
// or 'invalid' (present but not a 32-byte key).
function serverKeyState(s) {
  if (!s) return 'none';
  return serverKeyByteLen(s) === 32 ? 'ok' : 'invalid';
}

function renderProfilesModal() {
  var el = document.getElementById('profilesListEl');
  if (!profiles || !profiles.profiles || !profiles.profiles.length) {
    el.innerHTML = '<div style="color:var(--text-dim);padding:14px 0;font-size:13px">' + t('no_profiles') + '</div>'; return
  }
  var h = '';
  for (var i = 0; i < profiles.profiles.length; i++) {
    var p = profiles.profiles[i];
    var isActive = p.id === activeProfileId;
    var initial = (p.nickname || p.config.domain || '?').charAt(0).toUpperCase();
    h += '<div class="profile-row' + (isActive ? ' active-profile' : '') + '" id="prow-' + p.id + '">';
    h += '<div class="profile-row-main" onclick="activateProfile(\'' + p.id + '\')">';
    h += '<div class="profile-row-avatar">' + esc(initial) + '</div>';
    h += '<div class="profile-row-info"><div class="profile-row-name">' + esc(p.nickname || p.config.domain);
    if (isActive) h += '<span class="active-badge">' + t('active') + '</span>';
    h += '</div><div class="profile-row-domain">' + esc(p.config.domain);
    var ks = serverKeyState(p.config.serverKey);
    if (ks !== 'ok') {
      var msgKey = ks === 'invalid' ? 'invalid_server_key' : 'no_server_key_warning';
      h += ' <span onclick="event.stopPropagation();showToast(t(\'' + msgKey + '\'))" title="' + escAttr(t(msgKey)) + '" style="color:var(--error);font-size:12px;cursor:pointer">&#9888;</span>';
    }
    h += '</div></div>';
    h += '<div class="profile-row-btns">';
    h += '<button class="btn btn-flat btn-sm" onclick="event.stopPropagation();openShareModal(\'' + p.id + '\')" title="' + t('share') + '" style="font-size:11px">' + t('share') + '</button>';
    h += '<button class="btn btn-flat btn-sm" onclick="event.stopPropagation();openProfileEditor(\'' + p.id + '\')" title="' + t('edit') + '" style="font-size:11px">' + t('edit') + '</button>';
    h += '</div></div>';
    h += '</div>';
  }
  el.innerHTML = h;
}

// ===== SHARE PROFILE MODAL =====
var shareModalProfileId = '';

async function openShareModal(id) {
  shareModalProfileId = id;
  var p = (profiles && profiles.profiles || []).find(function (x) { return x.id === id });
  if (!p) return;
  document.getElementById('shareModalProfileName').textContent = p.nickname || p.config.domain;
  // Make sure rlState is loaded — the user may not have opened
  // the Bank modal yet, in which case rlState is its empty
  // default and the source dropdown would only have "Bank".
  if (!rlState || !rlState.lists || !rlState.lists.length) {
    try { await loadResolverLists(); } catch (e) { }
  }
  var src = document.getElementById('shareModalSource');
  src.innerHTML = '';
  var bankOpt = document.createElement('option');
  bankOpt.value = '__bank';
  bankOpt.textContent = (t('resolver_tab_bank') || 'Bank');
  src.appendChild(bankOpt);
  ((rlState && rlState.lists) || []).forEach(function (li) {
    var opt = document.createElement('option');
    opt.value = li.name;
    opt.textContent = li.name + ' (' + (li.count || 0) + ')';
    src.appendChild(opt);
  });
  document.getElementById('shareProfileModal').classList.add('active');
  await reloadShareModalResolvers();
}

function closeShareModal() {
  document.getElementById('shareProfileModal').classList.remove('active');
  shareModalProfileId = '';
}

async function reloadShareModalResolvers() {
  var src = document.getElementById('shareModalSource');
  var source = src ? src.value : '__bank';
  var el = document.getElementById('shareModalResolvers');
  if (!el) return;
  el.innerHTML = '<span style="color:var(--text-dim)">' + (t('loading') || '…') + '</span>';
  var addrs = [];
  try {
    if (source === '__bank') {
      var r = await fetch('/api/resolvers/bank');
      var data = r.ok ? await r.json() : { bank: [] };
      (data.bank || []).forEach(function (b) { addrs.push(b.addr); });
    } else {
      var lr = await fetch('/api/resolvers/lists?include=resolvers');
      if (lr.ok) {
        var ld = await lr.json();
        var li = (ld.lists || []).find(function (x) { return x.name === source; });
        if (li && li.resolvers) addrs = li.resolvers.slice();
      }
    }
  } catch (e) { }
  if (!addrs.length) {
    el.innerHTML = '<span style="color:var(--text-dim)">' + (t('no_active_resolvers') || '—') + '</span>';
    updateShareModalUri();
    return;
  }
  var h = '';
  addrs.forEach(function (addr) {
    h += '<label style="display:block;cursor:pointer;padding:2px 0">'
      + '<input type="checkbox" class="share-modal-cb" value="' + escAttr(addr) + '" checked onchange="updateShareModalUri()" style="width:auto;margin-inline-end:6px;vertical-align:middle">'
      + esc(addr) + '</label>';
  });
  el.innerHTML = h;
  updateShareModalUri();
}

function toggleAllShareModalResolvers(checked) {
  document.querySelectorAll('.share-modal-cb').forEach(function (cb) { cb.checked = checked });
  updateShareModalUri();
}

function getShareModalUri() {
  var selected = [];
  document.querySelectorAll('.share-modal-cb').forEach(function (cb) { if (cb.checked) selected.push(cb.value) });
  return buildProfileUri(shareModalProfileId, selected);
}

function updateShareModalUri() {
  var uri = getShareModalUri();
  var ta = document.getElementById('shareModalUri');
  if (ta) ta.value = uri || (t('no_config') || '');
}

function copyShareModalUri() {
  var uri = getShareModalUri();
  if (!uri) { showToast(t('no_config')); return }
  navigator.clipboard.writeText(uri).then(function () { showToast(t('copied')) }).catch(function () {
    var ta = document.getElementById('shareModalUri');
    if (ta) { ta.select(); ta.setSelectionRange(0, 9999); }
    showToast(t('copied'));
  });
}


var _switchingProfile = false;
async function activateProfile(id) {
  if (id === activeProfileId) { closeProfiles(); return }
  if (_switchingProfile) return; // ignore rapid repeat taps while a switch is already in flight
  _switchingProfile = true;
  var skipCheck = true;
  try {
    var bankR = await fetch('/api/resolvers/bank');
    if (bankR.ok) {
      var bankD = await bankR.json();
      var activeN = 0;
      (bankD.bank || []).forEach(function (b) { if (b.active) activeN++ });
      if (activeN > 0) skipCheck = await askRescan(activeN);
    }
  } catch (e) { }
  try {
    var r = await fetch('/api/profiles/switch', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ id: id, skipCheck: skipCheck }) });
    if (!r.ok) { _switchingProfile = false; return; }
    activeProfileId = id; selectedChannel = 0; channels = [];
    refreshingChannels = {};
    progressSilencedUntil = Date.now() + 5000;
    // Reset the chat header so it doesn't keep showing the previous
    // profile's channel name/handle while the user picks a new one.
    document.getElementById('chatName').textContent = 'thefeed';
    document.getElementById('chatSub').textContent = '';
    if (typeof setChatHeaderAvatar === 'function') setChatHeaderAvatar(null);
    if (typeof updateFeedHeaderChrome === 'function') updateFeedHeaderChrome();
    document.getElementById('progressPanel').innerHTML = '';
    document.getElementById('messages').innerHTML = '<div class="empty-state"><p>' + t('switching') + '</p></div>';
    // On desktop the chat panel is always visible; collapse it back to
    // the sidebar so the empty state doesn't sit next to a stale header.
    openSidebar();
    await loadProfiles(); closeProfiles();
    await loadChannels();
    if (channels.length === 0) {
      // No cache for this profile — DNS metadata fetch is in flight; the
      // SSE 'update' handler will replace this with the "pick a channel"
      // hint once channels actually arrive.
      showInitProgress(); await doRefresh();
    } else {
      document.getElementById('messages').innerHTML = '<div class="empty-state"><p>' + (t('select_channel_hint') || '') + '</p></div>';
    }
  } catch (e) { }
  _switchingProfile = false;
}

// ===== IMPORT URI =====
async function pasteImportUri() {
  var input = document.getElementById('importUriInput');
  if (!input) return;
  try {
    var text = await navigator.clipboard.readText();
    if (text) {
      input.value = text.trim();
      input.focus();
    }
  } catch (e) {
    // Clipboard API can fail (no permission, http context, etc.) —
    // fall back to focus + select-all so the user can Cmd-V into
    // an empty field with one tap.
    input.focus();
    try { input.select(); } catch (e2) { }
    showToast(t('clipboard_blocked') || 'Clipboard blocked — paste manually');
  }
}

async function doImportUri() {
  var errEl = document.getElementById('importError'); var okEl = document.getElementById('importSuccess');
  errEl.style.display = 'none'; okEl.style.display = 'none';
  var uri = document.getElementById('importUriInput').value.trim();
  if (!uri.startsWith('thefeed://')) { errEl.textContent = t('invalid_uri'); errEl.style.display = 'block'; return }
  try {
    var body = uri.substring('thefeed://'.length);
    var qIdx = body.indexOf('?'); var path = qIdx >= 0 ? body.substring(0, qIdx) : body;
    var params = qIdx >= 0 ? body.substring(qIdx + 1) : '';
    var parts = path.split('/');
    var domain = decodeURIComponent(parts[0] || ''); var key = decodeURIComponent(parts[1] || '');
    var resolvers = [];
    var sharedNick = '';
    var serverKey = '';
    var extraDomains = [];
    params.split('&').forEach(function (kv) {
      var eq = kv.indexOf('=');
      if (eq < 0) return;
      var k = kv.substring(0, eq);
      var v = kv.substring(eq + 1);
      if (k === 'r' && v) resolvers = decodeURIComponent(v).split(',').filter(Boolean);
      else if (k === 'n' && v) {
        try { sharedNick = decodeURIComponent(v); } catch (e) { sharedNick = ''; }
      }
      else if (k === 'sk' && v) serverKey = v.trim();
      else if (k === 'd' && v) {
        try { extraDomains = decodeURIComponent(v).split(',').map(function (s) { return s.trim() }).filter(Boolean); } catch (e) { extraDomains = []; }
      }
    });
    if (!domain || !key) { errEl.textContent = t('uri_missing'); errEl.style.display = 'block'; return }
    // No 8.8.8.8 / 1.1.1.1 fallback — an empty resolver list in
    // the URI is the sender's deliberate choice (e.g. they want
    // the recipient to bring their own bank).
    sharedNick = sanitizeNickname(sharedNick);
    // Only ask about bank merging when the URI actually carried
    // resolvers — empty list means the sender shared zero, so
    // there's nothing to merge and no prompt to show.
    await confirmAddResolversToBank(resolvers);
    // Create profile without resolvers (they're in the bank now).
    var profile = { id: '', nickname: sharedNick || domain, config: { domain: domain, key: key, serverKey: serverKey, extraDomains: extraDomains, queryMode: 'single', rateLimit: 6 } };
    var r = await fetch('/api/profiles', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ action: 'create', profile: profile }) });
    if (!r.ok) throw new Error('save failed');
    okEl.textContent = t('import_success').replace('{d}', domain);
    if (!serverKey) { okEl.textContent += ' ' + t('no_server_key_warning'); }
    okEl.style.display = 'block';
    document.getElementById('importUriInput').value = '';
    if (linkedConfig()) history.replaceState(null, '', location.pathname + location.search);
    await loadProfiles(); renderProfilesModal();
    refreshResolversBadge();
  } catch (e) { errEl.textContent = t('import_error') + ': ' + e.message; errEl.style.display = 'block' }
}

// confirmAddResolversToBank merges resolvers into the shared bank.
// Asks first when the bank already has entries; no-op on empty input.
async function confirmAddResolversToBank(resolvers) {
  if (!resolvers || !resolvers.length) return;
  var bankData = { count: 0 };
  try {
    var bankRes = await fetch('/api/resolvers/bank', { signal: AbortSignal.timeout(5000) });
    if (bankRes.ok) bankData = await bankRes.json();
  } catch (e) { }
  var shouldAdd = true;
  if (bankData.count > 0 && bankData.count <= 200) {
    shouldAdd = await showConfirmDialog(t('import_add_resolvers').replace('{n}', resolvers.length), t('yes'), t('no'));
  } else if (bankData.count > 200) {
    shouldAdd = await showConfirmDialog(t('import_add_resolvers_large').replace('{n}', resolvers.length).replace('{c}', bankData.count), t('yes'), t('no'));
  }
  if (shouldAdd) {
    await fetch('/api/resolvers/bank', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ resolvers: resolvers }) });
  }
}

// ===== BUILT-IN DEFAULT CONFIGS =====
// Flag emblem shown next to each built-in config (same asset as the
// scanner preset tag).
var DEFAULT_CONFIG_EMBLEM = '<img class="flag-emblem" src="/static/lion-sun.svg" alt="">';

var defaultConfigs = null; // {profiles: [...], resolvers: [...]} from /api/profiles/defaults

async function toggleDefaultConfigs() {
  var el = document.getElementById('defaultConfigsList');
  var btn = document.getElementById('dcToggle');
  if (!el) return;
  if (el.style.display !== 'none') {
    el.style.display = 'none';
    if (btn) { btn.classList.remove('open'); btn.setAttribute('aria-expanded', 'false'); }
    return;
  }
  if (!defaultConfigs) {
    try {
      var r = await fetch('/api/profiles/defaults');
      if (!r.ok) throw new Error(await r.text() || 'failed');
      defaultConfigs = await r.json();
    } catch (e) { showToast(t('import_error') + ': ' + e.message); return }
  }
  renderDefaultConfigs();
  el.style.display = '';
  if (btn) { btn.classList.add('open'); btn.setAttribute('aria-expanded', 'true'); }
}

function renderDefaultConfigs() {
  var el = document.getElementById('defaultConfigsList');
  if (!el || !defaultConfigs) return;
  // Match an imported profile by DOMAIN (the stable identity) so a default
  // whose key/serverKey changed in a new app version offers an Update.
  var byDomain = {};
  ((profiles && profiles.profiles) || []).forEach(function (p) { byDomain[p.config.domain] = p; });
  var h = '';
  (defaultConfigs.profiles || []).forEach(function (d, i) {
    var existing = byDomain[d.domain];
    h += '<div class="default-config-row">';
    h += '<span class="default-config-emblem">' + DEFAULT_CONFIG_EMBLEM + '</span>';
    h += '<div class="default-config-info"><div class="default-config-name">' + esc(d.nickname) + '</div>';
    h += '<div class="default-config-domain">' + esc(d.domain) + '</div></div>';
    var changed = existing && (
      existing.config.key !== d.key ||
      (existing.config.serverKey || '') !== (d.serverKey || '') ||
      (existing.config.extraDomains || []).join(',') !== (d.extraDomains || []).join(','));
    if (!existing) {
      h += '<button class="btn btn-primary btn-sm dc-add-btn" onclick="importDefaultConfig(' + i + ')"><span data-icon="add"></span> ' + t('import') + '</button>';
    } else if (changed) {
      h += '<button class="btn btn-primary btn-sm dc-add-btn" onclick="importDefaultConfig(' + i + ',\'' + existing.id + '\')"><span data-icon="refresh"></span> ' + t('update') + '</button>';
    } else {
      h += '<span class="dc-imported"><span data-icon="check"></span> ' + t('already_imported') + '</span>';
    }
    h += '</div>';
  });
  el.innerHTML = h;
  if (typeof hydrateIcons === 'function') hydrateIcons(el);
}

// importDefaultConfig imports default config i, or updates the existing
// profile updateId in place (key/serverKey changed in a new version).
async function importDefaultConfig(i, updateId) {
  if (!defaultConfigs || !defaultConfigs.profiles || !defaultConfigs.profiles[i]) return;
  var d = defaultConfigs.profiles[i];
  var errEl = document.getElementById('importError'); var okEl = document.getElementById('importSuccess');
  errEl.style.display = 'none'; okEl.style.display = 'none';
  try {
    await confirmAddResolversToBank(defaultConfigs.resolvers || []);
    // On update, keep the user's chosen nickname; otherwise use the default's.
    var nick = d.nickname || d.domain;
    if (updateId && profiles && profiles.profiles) {
      var ex = profiles.profiles.find(function (x) { return x.id === updateId });
      if (ex && ex.nickname) nick = ex.nickname;
    }
    var profile = { id: updateId || '', nickname: nick, config: { domain: d.domain, key: d.key, serverKey: d.serverKey || '', extraDomains: d.extraDomains || [], queryMode: 'single', rateLimit: 6 } };
    // skipCheck: importing/updating a default config must reuse existing
    // resolvers, not force a full rescan (the server only truly rescans when no
    // resolvers are available anywhere).
    var r = await fetch('/api/profiles', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ action: updateId ? 'update' : 'create', profile: profile, skipCheck: true }) });
    if (!r.ok) throw new Error(await r.text() || 'save failed');
    okEl.textContent = (updateId ? t('update_success') : t('import_success')).replace('{d}', nick); okEl.style.display = 'block';
    await loadProfiles(); renderProfilesModal(); renderDefaultConfigs();
    refreshResolversBadge();
  } catch (e) { errEl.textContent = t('import_error') + ': ' + e.message; errEl.style.display = 'block' }
}

// ===== PROFILE EDITOR =====
function openProfileEditor(id) {
  editingProfileId = id;
  document.getElementById('profileEditorModal').classList.add('active');
  document.getElementById('peError').style.display = 'none';
  document.getElementById('peWarning').style.display = 'none';
  if (id) {
    document.getElementById('profileEditorTitle').textContent = t('edit_profile');
    document.getElementById('peDeleteBtn').style.display = '';
    var p = profiles && profiles.profiles && profiles.profiles.find(function (x) { return x.id === id });
    if (p) {
      document.getElementById('peNick').value = p.nickname || '';
      document.getElementById('peDomain').value = p.config.domain || '';
      document.getElementById('peKey').value = p.config.key || '';
      document.getElementById('peServerKey').value = p.config.serverKey || '';
      document.getElementById('peExtraDomains').value = (p.config.extraDomains || []).join(', ');
    }
    updateServerKeyHint();
    document.getElementById('peChannelSection').style.display = '';
    var isActive = id === activeProfileId;
    if (isActive) {
      document.getElementById('peChannelNote').textContent = t('channel_mgmt_note');
      document.getElementById('peAddChannelRow').style.display = 'flex';
      loadEditorChannels();
    } else {
      document.getElementById('peChannelNote').textContent = t('channel_mgmt_inactive');
      document.getElementById('peAddChannelRow').style.display = 'none';
      document.getElementById('peChannelList').innerHTML = '';
    }
  } else {
    document.getElementById('profileEditorTitle').textContent = t('new_profile');
    document.getElementById('peDeleteBtn').style.display = 'none';
    document.getElementById('peNick').value = '';
    document.getElementById('peDomain').value = '';
    document.getElementById('peKey').value = '';
    document.getElementById('peServerKey').value = '';
    document.getElementById('peExtraDomains').value = '';
    updateServerKeyHint();
    document.getElementById('peChannelSection').style.display = 'none';
  }
}
function closeProfileEditor() {
  document.getElementById('profileEditorModal').classList.remove('active');
  editingProfileId = null;
  // Clear the passphrase so a leftover value in the hidden modal can't trip
  // Chrome's "save password?" prompt on a later navigation (it pairs the
  // nickname + this field as a login form).
  var k = document.getElementById('peKey'); if (k) k.value = '';
}

// updateServerKeyHint shows the validity of the server key being edited.
function updateServerKeyHint() {
  var el = document.getElementById('peServerKey');
  var hint = document.getElementById('peServerKeyHint');
  if (!el || !hint) return;
  var st = serverKeyState((el.value || '').trim());
  if (st === 'ok') { hint.textContent = t('server_key_ok'); hint.style.color = 'var(--success)'; }
  else if (st === 'invalid') { hint.textContent = t('invalid_server_key'); hint.style.color = 'var(--error)'; }
  else { hint.textContent = t('no_server_key_warning'); hint.style.color = 'var(--text-dim)'; }
}

async function loadEditorChannels() {
  var el = document.getElementById('peChannelList');
  el.innerHTML = '<div style="color:var(--text-dim);font-size:12px">' + t('loading') + '</div>';
  try {
    var r = await fetch('/api/admin', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ command: 'list_channels', arg: '' }) });
    if (!r.ok) throw new Error(await r.text() || 'failed');
    var data = await r.json();
    var chans = (data.result || '').split('\n').filter(function (s) { return s.trim() });
    if (!chans.length) { el.innerHTML = '<div style="color:var(--text-dim);font-size:12px">' + t('no_channels') + '</div>'; return }
    var h = '';
    for (var i = 0; i < chans.length; i++) {
      h += '<div class="channel-list-item"><span>' + esc(chans[i]) + '</span>';
      h += '<button class="btn btn-flat btn-sm" style="color:var(--error)" data-ch="' + escAttr(chans[i]) + '" onclick="removeChannelEditor(this.dataset.ch)">' + t('remove') + '</button></div>';
    }
    el.innerHTML = h;
  } catch (e) { el.innerHTML = '<div style="color:var(--error);font-size:12px">' + esc(e.message) + '</div>' }
}

async function addChannelEditor() {
  var input = document.getElementById('peAddChannelInput');
  var u = input.value.trim().replace(/^@/, ''); if (!u) return;
  try {
    var r = await fetch('/api/admin', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ command: 'add_channel', arg: u }) });
    if (!r.ok) { showToast(await r.text() || 'Failed'); return }
    input.value = ''; addLogLine('Channel added: ' + u); loadEditorChannels();
  } catch (e) { showToast(e.message) }
}

async function removeChannelEditor(u) {
  try {
    var r = await fetch('/api/admin', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ command: 'remove_channel', arg: u }) });
    if (!r.ok) { showToast(await r.text() || 'Failed'); return }
    addLogLine('Channel removed: ' + u); loadEditorChannels();
  } catch (e) { showToast(e.message) }
}

// sanitizeNickname extracts a clean alias from raw input. If the user
// pasted a thefeed:// URI, pulls the n= param. Strips control chars,
// the clipboard emoji, newlines, and trims to 32 chars.
function sanitizeNickname(raw) {
  var v = String(raw || '');
  var m = v.match(/thefeed:\/\/[^\s?#]+(?:\?[^\s#]*)?(?:[?&]n=([^&\s#]*))/i);
  if (m && m[1]) {
    try { v = decodeURIComponent(m[1].replace(/\+/g, ' ')); } catch (e) { v = m[1]; }
  }
  // iOS Quick Type appends UI text starting with the clipboard glyph
  // (U+1F4CB) when the user manually pastes — cut from there.
  var clipIdx = v.indexOf('📋');
  if (clipIdx >= 0) v = v.slice(0, clipIdx);
  // Strip control / zero-width / bidi / BOM characters.
  v = v.replace(/[\u0000-\u001F\u007F\u200B-\u200F\u2028\u2029\uFEFF]/g, "");
  v = v.replace(/\s+/g, ' ').trim();
  return v.slice(0, 32);
}

async function saveProfile() {
  var errEl = document.getElementById('peError'); errEl.style.display = 'none';
  // Trim and clamp the nickname server-side too: the maxlength
  // attribute on the input only stops keyboard typing, not paste
  // / programmatic value setting.
  var nick = sanitizeNickname(document.getElementById('peNick').value);
  var domain = document.getElementById('peDomain').value.trim();
  var key = document.getElementById('peKey').value;
  var serverKey = (document.getElementById('peServerKey').value || '').trim();
  var extraDomains = (document.getElementById('peExtraDomains').value || '').split(',').map(function (s) { return s.trim() }).filter(Boolean);
  if (!domain || !key) { errEl.textContent = t('domain') + ' / ' + t('passphrase'); errEl.style.display = 'block'; return }
  var profile = { id: editingProfileId || '', nickname: nick || domain, config: { domain: domain, key: key, serverKey: serverKey, extraDomains: extraDomains } };
  // Preserve fields the editor doesn't manage (e.g. autoScan) so saving
  // from the editor doesn't wipe them. Server key and extra domains now
  // come from their own editable fields above.
  if (editingProfileId && profiles && profiles.profiles) {
    var existing = profiles.profiles.find(function (x) { return x.id === editingProfileId });
    if (existing && existing.config.autoScan === false) profile.config.autoScan = false;
  }
  var action = editingProfileId ? 'update' : 'create';
  var wasFirst = !profiles || !profiles.profiles || profiles.profiles.length === 0;
  var skipCheck = true;
  var isActiveEdit = editingProfileId && editingProfileId === activeProfileId;
  if (isActiveEdit || wasFirst) {
    try {
      var bankR = await fetch('/api/resolvers/bank');
      if (bankR.ok) {
        var bankD = await bankR.json();
        var activeN = 0;
        (bankD.bank || []).forEach(function (b) { if (b.active) activeN++ });
        if (activeN > 0) {
          skipCheck = await askRescan(activeN);
        }
      }
    } catch (e) { /* ignore — default to skipCheck=true */ }
  }
  try {
    var r = await fetch('/api/profiles', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ action: action, profile: profile, skipCheck: skipCheck }) });
    if (!r.ok) { errEl.textContent = await r.text(); errEl.style.display = 'block'; return }
    await loadProfiles();
    var savedEditId = editingProfileId;
    closeProfileEditor();
    if (wasFirst) {
      closeProfiles();
      document.getElementById('progressPanel').innerHTML = '';
      document.getElementById('messages').innerHTML = '<div class="empty-state"><p>' + t('loading') + '</p></div>';
      showInitProgress();
    } else {
      renderProfilesModal();
      renderDefaultConfigs(); // keep default-config Import/Update states in sync
      // Only rescan if we updated the currently active profile
      if (savedEditId && savedEditId === activeProfileId) {
        showToast(t('rescan_started'));
        document.getElementById('progressPanel').innerHTML = '';
        showInitProgress();
        await loadChannels();
        if (selectedChannel > 0) await loadMessages(selectedChannel);
      }
    }
  } catch (e) { errEl.textContent = e.message; errEl.style.display = 'block' }
}

async function deleteEditingProfile() {
  if (!editingProfileId) return;
  if (!(await showConfirmDialog(t('delete') + '?', t('delete'), t('cancel')))) return;
  try {
    // skipCheck: deleting a profile must not trigger a full resolver rescan —
    // resolvers are shared and server-agnostic, so the active list is reused
    // immediately (no scan that would transiently empty the active pool).
    await fetch('/api/profiles', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ action: 'delete', profile: { id: editingProfileId }, skipCheck: true }) });
    await loadProfiles(); closeProfileEditor();
    if (document.getElementById('profilesModal').classList.contains('active')) renderProfilesModal();
    // Refresh the default-configs list if it's open, so Import/Update button
    // states reflect the deletion (no-op when defaults aren't loaded).
    renderDefaultConfigs();
    refreshResolversBadge();
  } catch (e) { }
}

