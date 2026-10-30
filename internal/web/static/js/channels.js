// ===== CHANNELS =====
async function loadChannels() {
  try {
    var r = await fetch('/api/channels'); channels = await r.json(); if (!channels) channels = [];
    // Pull server-persisted seen-state once so unread counts survive the
    // loopback port changing (localStorage is wiped when the origin does).
    await syncSeenFromServer();
    // Initialise NEW-badge baseline for any channel we've never seen
    // before. Without this the badge can never fire on a channel the
    // user hasn't manually opened yet (since channelHasNew suppresses
    // when no baseline exists).
    var baselineDirty = false;
    for (var ci = 0; ci < channels.length; ci++) {
      var ch0 = channels[ci];
      var n0 = ch0.Name || ch0.name || '';
      if (!n0) continue;
      if (previousContentHashes[n0] === undefined) {
        previousContentHashes[n0] = ch0.ContentHash || ch0.contentHash || 0;
        baselineDirty = true;
      }
      if (!(n0 in previousMsgIDs)) {
        previousMsgIDs[n0] = ch0.LastMsgID || ch0.lastMsgID || 0;
        baselineDirty = true;
      }
    }
    if (baselineDirty) {
      saveSeenMap('thefeed_seen_ids', previousMsgIDs);
      saveSeenMap('thefeed_seen_hashes', previousContentHashes);
      // Persist new baselines server-side too (server only fills gaps).
      pushSeenBulk(previousMsgIDs, previousContentHashes);
    }
    await loadAutoUpdate();
    renderChannels(); updateSendPanel();
    var initBar = document.getElementById('prog-init'); if (initBar) initBar.remove();
    var sr = await fetch('/api/status'); var st = await sr.json();
    telegramLoggedIn = !!st.telegramLoggedIn;
    if (st.nextFetch) { serverNextFetch = st.nextFetch; updateNextFetchDisplay() }
    if (st.latestVersion) { latestVersion = st.latestVersion; renderLatestVersion() }
  } catch (e) { }
}

// UI cache of the active profile's auto-update list (server is source of truth).
var autoUpdateChannels = new Set();
async function loadAutoUpdate() {
  try {
    var r = await fetch('/api/auto-update');
    if (!r.ok) return;
    var d = await r.json();
    autoUpdateChannels = new Set();
    if (Array.isArray(d.channels)) {
      for (var i = 0; i < d.channels.length; i++) {
        var n = String(d.channels[i] || '').replace(/^@/, '').trim();
        if (n) autoUpdateChannels.add(n);
      }
    }
  } catch (e) { }
}
async function toggleAutoUpdate(name, ev) {
  if (ev) { ev.stopPropagation(); ev.preventDefault(); }
  var clean = String(name || '').replace(/^@/, '').trim();
  if (!clean) return;
  // Optimistic flip; rollback if the server rejects.
  if (autoUpdateChannels.has(clean)) autoUpdateChannels.delete(clean);
  else autoUpdateChannels.add(clean);
  renderChannels();
  try {
    var r = await fetch('/api/auto-update/toggle', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ channel: clean })
    });
    if (!r.ok) {
      if (autoUpdateChannels.has(clean)) autoUpdateChannels.delete(clean);
      else autoUpdateChannels.add(clean);
      renderChannels();
      return;
    }
    var d = await r.json();
    autoUpdateChannels = new Set();
    if (Array.isArray(d.channels)) {
      for (var i = 0; i < d.channels.length; i++) {
        var n = String(d.channels[i] || '').replace(/^@/, '').trim();
        if (n) autoUpdateChannels.add(n);
      }
    }
    renderChannels();
    var template = d.enabled
      ? (t('auto_update_on') || 'Auto-update is now ON for @{name}')
      : (t('auto_update_off') || 'Auto-update is now OFF for @{name}');
    showToast(template.replace('{name}', d.channel || clean), 5000);
  } catch (e) { }
}

// UI cache of the active profile's pinned channel list (server is source of truth).
// Populated from GET /api/settings (pinnedChannels field) during boot — no
// separate endpoint needed.
var pinnedChannels = new Set();
async function togglePinChannel(name, ev) {
  if (ev) { ev.stopPropagation(); ev.preventDefault(); }
  var clean = String(name || '').replace(/^@/, '').trim();
  if (!clean) return;
  // Optimistic flip; rollback if the server rejects.
  if (pinnedChannels.has(clean)) pinnedChannels.delete(clean);
  else pinnedChannels.add(clean);
  // Force a full rebuild so sortPinned() re-orders the list immediately
  // (the fast-path only toggles classes and skips sorting).
  _forceChannelRebuild = true;
  renderChannels();
  try {
    var r = await fetch('/api/pinned-channels/toggle', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ channel: clean })
    });
    if (!r.ok) {
      if (pinnedChannels.has(clean)) pinnedChannels.delete(clean);
      else pinnedChannels.add(clean);
      _forceChannelRebuild = true;
      renderChannels();
      return;
    }
    var d = await r.json();
    pinnedChannels = new Set();
    if (Array.isArray(d.channels)) {
      for (var i = 0; i < d.channels.length; i++) {
        var n = String(d.channels[i] || '').replace(/^@/, '').trim();
        if (n) pinnedChannels.add(n);
      }
    }
    _forceChannelRebuild = true;
    renderChannels();
    var template = d.pinned
      ? (t('pin_on') || 'Channel @{name} pinned')
      : (t('pin_off') || 'Channel @{name} unpinned');
    showToast(template.replace('{name}', d.channel || clean), 3000);
  } catch (e) { }
}

var _renderChannelsTimer = null;
var _forceChannelRebuild = false;
function renderChannels() {
  // Debounce: avoid rapid sequential DOM rebuilds that cause hover flicker
  if (_renderChannelsTimer) clearTimeout(_renderChannelsTimer);
  _renderChannelsTimer = setTimeout(_renderChannelsNow, 50);
}
function _renderChannelsNow() {
  _renderChannelsTimer = null;
  var el = document.getElementById('channelList');
  var savedBtn = document.getElementById('savedChannelBtn');
  if (savedBtn) savedBtn.remove();
  if (!channels || !channels.length) {
    var _hint = resolverScanHint || (t('no_channels_hint') + ' <button onclick="jumpToLog()" style="background:none;border:none;cursor:pointer;font-size:13px;vertical-align:middle;padding:0 2px;color:inherit">' + icon('log') + '</button> ' + t('no_channels_hint2'));
    el.innerHTML = '<div style="padding:20px;text-align:center;color:var(--text-dim);font-size:13px">' + t('no_channels') + '<br><span id="no-ch-hint" style="font-size:11px;opacity:.7;line-height:1.8">' + _hint + '</span></div>';
    if (savedBtn) el.insertBefore(savedBtn, el.firstChild);
    return
  }
  // Fast-path: if channel count matches existing items, just update classes/badges
  var existingItems = el.querySelectorAll('.ch-item');
  if (existingItems.length === channels.length) {
    var needsFullRebuild = _forceChannelRebuild;
    _forceChannelRebuild = false;
    for (var ci = 0; ci < channels.length && !needsFullRebuild; ci++) {
      var ch = channels[ci], nm = ch.Name || ch.name || 'Channel ' + (ci + 1);
      var lbl = ch.DisplayName || ch.displayName || nm;
      if (existingItems[ci].dataset.name !== nm || existingItems[ci].dataset.label !== lbl) { needsFullRebuild = true; break }
    }
    if (!needsFullRebuild) {
      for (var ui = 0; ui < channels.length; ui++) {
        var num = ui + 1;
        existingItems[ui].classList.toggle('active', num === selectedChannel);
        var chNm = channels[ui].Name || channels[ui].name || '';
        var previewEl = existingItems[ui].querySelector('.ch-preview');
        if (previewEl) previewEl.innerHTML = (num !== selectedChannel) ? channelUnreadBadge(channels[ui]) : '';
        var autoBtn = existingItems[ui].querySelector('.ch-autoupdate');
        if (autoBtn) {
          var key = chNm.replace(/^@/, '').trim();
          autoBtn.classList.toggle('on', autoUpdateChannels.has(key));
        }
        var pinBtn = existingItems[ui].querySelector('.ch-pin');
        if (pinBtn) {
          var pinKey = chNm.replace(/^@/, '').trim();
          pinBtn.classList.toggle('on', pinnedChannels.has(pinKey));
        }
        // Avatar diff against profilePicCache. Without this the
        // fast path skips avatar updates and SSE-driven reloads
        // don't reach the UI.
        var ct2 = channels[ui].ChatType || channels[ui].chatType || 0;
        var isX2 = ct2 === 2;
        var bareHandle2 = chNm.replace(/^@/, '').toLowerCase();
        if (isX2 && bareHandle2.indexOf('x/') === 0) bareHandle2 = bareHandle2.substring(2);
        var unameKey2 = isX2 ? ('x:' + bareHandle2) : bareHandle2;
        var avatarEl = existingItems[ui].querySelector('.ch-avatar');
        if (avatarEl) {
          var imgEl = avatarEl.querySelector('.ch-avatar-img');
          var shouldShow = !!profilePicCache.users[unameKey2];
          if (shouldShow && !imgEl) {
            var img = document.createElement('img');
            img.className = 'ch-avatar-img';
            img.src = '/api/profile-pics/' + encodeURIComponent(unameKey2);
            img.loading = 'lazy';
            img.alt = '';
            img.onerror = function () {
              this.parentNode.classList.add('ch-avatar-noimg');
              this.remove();
            };
            avatarEl.classList.remove('ch-avatar-noimg');
            avatarEl.insertBefore(img, avatarEl.firstChild);
          } else if (!shouldShow && imgEl) {
            imgEl.remove();
            avatarEl.classList.add('ch-avatar-noimg');
          }
        }
      }
      if (savedBtn) el.insertBefore(savedBtn, el.firstChild);
      _updateRefreshBadge(); return;
    }
  }
  var pubs = [], privs = [], xposts = [];
  for (var i = 0; i < channels.length; i++) {
    var c = channels[i];
    var ct = c.ChatType || c.chatType || 0;
    if (ct === 2) xposts.push({ ch: c, idx: i });
    else if (ct === 1) privs.push({ ch: c, idx: i });
    else pubs.push({ ch: c, idx: i });
  }
  // Sort pinned channels to the top within each section, respecting pin order.
  function sortPinned(items) {
    var pinned = [], unpinned = [];
    var pinOrderMap = new Map();
    var idx = 0;
    pinnedChannels.forEach(function (v) { pinOrderMap.set(v, idx++); });
    for (var k = 0; k < items.length; k++) {
      var ch = items[k].ch;
      var pKey = (ch.Name || ch.name || '').replace(/^@/, '').trim();
      if (pinOrderMap.has(pKey)) {
        items[k]._pinIdx = pinOrderMap.get(pKey);
        pinned.push(items[k]);
      } else {
        unpinned.push(items[k]);
      }
    }
    pinned.sort(function(a, b) { return a._pinIdx - b._pinIdx; });
    return pinned.concat(unpinned);
  }
  pubs = sortPinned(pubs);
  xposts = sortPinned(xposts);
  privs = sortPinned(privs);
  function section(title, items) {
    if (!items.length) return ''; var h = '';
    if (title) h += '<div class="channel-section-title">' + esc(title) + '</div>';
    for (var j = 0; j < items.length; j++) {
      var e = items[j], num2 = e.idx + 1;
      var handle = e.ch.Name || e.ch.name || 'Channel ' + num2;
      var label = e.ch.DisplayName || e.ch.displayName || handle;
      var ct2 = e.ch.ChatType || e.ch.chatType || 0;
      var isPriv = e.ch.ChatType === 1 || e.ch.chatType === 1;
      var isX = ct2 === 2;
      var avatarName = label;
      if (isX && handle.toLowerCase().indexOf('x/') === 0) avatarName = handle.substring(2);
      if (avatarName.charAt(0) === '@') avatarName = avatarName.substring(1);
      var avatarText = (avatarName || '?').charAt(0).toUpperCase();
      var active = num2 === selectedChannel ? ' active' : '';
      var chNm2 = e.ch.Name || e.ch.name || '';
      var badge = (num2 !== selectedChannel) ? channelUnreadBadge(e.ch) : '';
      h += '<div class="ch-item' + active + '" data-name="' + escAttr(handle) + '" data-label="' + escAttr(label) + '" onclick="selectChannel(' + num2 + ')">';
      // X channels: e.ch.Name is "x/<handle>" (category prefix);
      // strip before re-attaching the bundle's "x:" prefix.
      var avatarImg = '';
      var bareHandle = (chNm2 || avatarName || '').replace(/^@/, '').toLowerCase();
      if (isX && bareHandle.indexOf('x/') === 0) bareHandle = bareHandle.substring(2);
      var unameKey = isX ? ('x:' + bareHandle) : bareHandle;
      if (profilePicCache.users[unameKey]) {
        avatarImg = '<img class="ch-avatar-img" src="/api/profile-pics/'
          + encodeURIComponent(unameKey)
          + '" loading="lazy" alt=""'
          + ' onerror="this.parentNode.classList.add(\'ch-avatar-noimg\');this.remove()">';
      }
      h += '<div class="ch-avatar">' + avatarImg + '<span class="ch-avatar-letter">' + esc(avatarText) + '</span></div>';
      var chSubText = !isX ? (handle.charAt(0) === '@' ? handle : '@' + handle) : '';
      h += '<div class="ch-info"><div class="ch-name">' + formatTitleWithFlag(label) + (isPriv ? '<span class="ch-type-tag">' + t('private') + '</span>' : (isX ? '<span class="ch-type-tag x-tag">' + t('x_label') + '</span>' : '')) + '</div>';
      if (chSubText) h += '<div class="ch-sub">' + esc(chSubText) + '</div>';
      h += '<div class="ch-preview">' + badge + '</div></div>';
      var autoKey = handle.replace(/^@/, '').trim();
      var autoOn = autoUpdateChannels.has(autoKey);
      var autoTitle = esc(t('auto_update_toggle') || 'Auto-update this channel');
      h += '<button type="button" class="ch-autoupdate' + (autoOn ? ' on' : '') + '"'
        + ' title="' + autoTitle + '" aria-label="' + autoTitle + '"'
        + ' onclick="toggleAutoUpdate(\'' + jsStr(autoKey) + '\', event)">' + icon('timer') + '</button>';
      var pinKey = handle.replace(/^@/, '').trim();
      var pinOn = pinnedChannels.has(pinKey);
      var pinTitle = esc(t('pin_channel') || 'Pin this channel');
      // Use 📌 for pinned, and hollow pin 📍 or just simple styling for unpinned.
      var pinGlyph = pinOn ? icon('pinned') : icon('unpinned');
      h += '<button type="button" class="ch-pin' + (pinOn ? ' on' : '') + '"'
        + ' title="' + pinTitle + '" aria-label="' + pinTitle + '"'
        + ' onclick="togglePinChannel(\'' + jsStr(pinKey) + '\', event)">' + pinGlyph + '</button>';
      h += '</div>';
    }
    return h;
  }
  var oldScroll = el.scrollTop;
  el.innerHTML = section('', pubs) + section(t('x_posts'), xposts) + section(t('private'), privs);
  if (savedBtn) el.insertBefore(savedBtn, el.firstChild);
  el.scrollTop = oldScroll;
  _updateRefreshBadge();
}
function _updateRefreshBadge() {
  var hasNew = false;
  for (var k = 0; k < channels.length; k++) {
    if (k + 1 === selectedChannel) continue;
    if (channelHasNew(channels[k])) { hasNew = true; break }
  }
  document.getElementById('refreshBtn').classList.toggle('refresh-has-new', hasNew);
}

// channelHasNew returns true when the channel's content has changed
// since the user last visited it.
function channelHasNew(ch) {
  if (!ch) return false;
  var name = ch.Name || ch.name || '';
  if (!name) return false;
  var hash = ch.ContentHash || ch.contentHash || 0;
  var lastID = ch.LastMsgID || ch.lastMsgID || 0;
  var ct = ch.ChatType || ch.chatType || 0;
  var seenHash = previousContentHashes[name];
  var seenID = previousMsgIDs[name] || 0;
  if (seenHash === undefined && seenID === 0) return false;
  if (seenHash !== undefined && hash !== 0 && hash !== seenHash) return true;
  if (ct !== 2 && seenID > 0 && lastID > seenID) return true;
  return false;
}

// channelUnreadBadge returns the inner HTML for a channel's list badge, or
// '' for none. Telegram channels show a numeric unread count (LastMsgID
// minus the last-seen ID — message IDs are sequential), capped as "99+".
// X/Twitter channels keep the boolean "NEW" label because their IDs aren't
// sequential, so a real count can't be derived.
var UNREAD_CAP = 99;
function channelUnreadBadge(ch) {
  if (!channelHasNew(ch)) return '';
  var ct = ch.ChatType || ch.chatType || 0;
  if (ct === 2) return '<span class="ch-badge">NEW</span>';
  var name = ch.Name || ch.name || '';
  var n = (ch.LastMsgID || ch.lastMsgID || 0) - (previousMsgIDs[name] || 0);
  if (n <= 0) return '<span class="ch-badge">NEW</span>'; // content changed (edit) but no new message
  return '<span class="ch-badge">' + (n > UNREAD_CAP ? (UNREAD_CAP + '+') : n) + '</span>';
}

async function selectChannel(num) {
  if (typeof viewingSaved !== 'undefined' && viewingSaved) {
    closeSavedMessages();
  }
  // Save lastSeen for previous channel and flush any pending sticky commit.
  if (selectedChannel > 0 && currentMaxTimestamp > 0) {
    var prevName = channelName(selectedChannel);
    if (newMsgSepCommitTimer[prevName]) {
      clearTimeout(newMsgSepCommitTimer[prevName]);
      delete newMsgSepCommitTimer[prevName];
      delete newMsgSepLastSeen[prevName];
    }
    setLastSeenTimestamp(prevName, currentMaxTimestamp);
  }
  var gen = ++_selectGen;
  selectedChannel = num;
  currentMaxMsgID = 0;
  currentMaxTimestamp = 0;
  newMsgScrollDone = false;
  openChat();
  var ch = channels[num - 1]; var name = (ch && (ch.DisplayName || ch.displayName || ch.Name || ch.name)) || 'Channel ' + num;
  document.getElementById('chatName').innerHTML = formatTitleWithFlag(name);
  var chHandle = (ch && (ch.Name || ch.name)) || '';
  var isXCh = ch && (ch.ChatType || ch.chatType) === 2;
  var subHandle = (!isXCh && chHandle) ? (chHandle.charAt(0) === '@' ? chHandle : '@' + chHandle) : '';
  document.getElementById('chatSub').textContent = subHandle;
  setChatHeaderAvatar(ch);
  updateFeedHeaderChrome();
  renderChannels(); updateSendPanel();
  document.getElementById('messages').innerHTML = '<div class="empty-state"><p>' + t('loading') + '</p></div>';
  document.getElementById('scrollDownBtn').classList.remove('visible');
  await loadMessages(num);
  // Bail if user navigated away while we were loading
  if (gen !== _selectGen) return;
  // Show progress bar and mark channel as refreshing so the bar persists
  // through the metadata fetch + channel fetch (~5 s). The bar is removed
  // when the SSE update arrives with fresh data for this channel.
  showChannelFetchProgress(num, name);
  refreshingChannels[num] = true;
  doRefresh(false);
}

// Populate the floating feed header's avatar from a channel record, mirroring
// the channel-list avatar (profile pic if cached, else the initial letter).
// Pass null/undefined to clear it (no-channel placeholder → hidden via :empty).
function setChatHeaderAvatar(ch) {
  var el = document.getElementById('chatHeaderAvatar');
  if (!el) return;
  el.classList.remove('is-saved');
  if (!ch) { el.innerHTML = ''; el.classList.add('ch-avatar-noimg'); return; }
  var nm = (ch.DisplayName || ch.displayName || ch.Name || ch.name || '?');
  var isX = (ch.ChatType || ch.chatType || 0) === 2;
  var bare = String(ch.Name || ch.name || nm).replace(/^@/, '').toLowerCase();
  if (isX && bare.indexOf('x/') === 0) bare = bare.substring(2);
  var key = isX ? ('x:' + bare) : bare;
  var letter = (String(nm).replace(/^@/, '') || '?').charAt(0).toUpperCase();
  var img = '';
  if (typeof profilePicCache !== 'undefined' && profilePicCache.users && profilePicCache.users[key]) {
    img = '<img class="ch-avatar-img" src="/api/profile-pics/' + encodeURIComponent(key)
      + '" loading="lazy" alt="" onerror="this.parentNode.classList.add(\'ch-avatar-noimg\');this.remove()">';
  }
  el.classList.remove('ch-avatar-noimg');
  el.innerHTML = img + '<span class="ch-avatar-letter">' + esc(letter) + '</span>';
}
if (typeof window !== 'undefined') window.setChatHeaderAvatar = setChatHeaderAvatar;

// Toggle `.has-channel` on the feed header so its action buttons (search /
// copy / refresh / timer / kebab) only show when a channel is actually open —
// they're meaningless on the "select a channel" placeholder.
function updateFeedHeaderChrome() {
  var h = document.querySelector('.chat-area > .chat-header');
  if (h) h.classList.toggle('has-channel', typeof selectedChannel !== 'undefined' && selectedChannel > 0);
}
if (typeof window !== 'undefined') window.updateFeedHeaderChrome = updateFeedHeaderChrome;

function showChannelFetchProgress(num, name) {
  var panel = document.getElementById('progressPanel');
  var id = 'prog-fetch-ch-' + num;
  var existing = document.getElementById(id);
  if (existing) return;
  var item = document.createElement('div'); item.id = id; item.className = 'progress-item';
  item.dataset.lastUpdate = Date.now();
  item.innerHTML = '<button class="progress-close" onclick="this.parentNode.remove()" title="' + esc(t('dismiss') || 'Dismiss') + '">&times;</button><div class="progress-label">' + t('fetching_channel') + ' ' + (name || num) + '</div><div class="progress-bar"><div class="progress-fill" style="width:40%;animation:prog-pulse 1.5s ease-in-out infinite"></div></div>';
  panel.insertBefore(item, panel.firstChild);
  setTimeout(function () { var el = document.getElementById(id); if (el) el.remove() }, 15000);
}

function updateSendPanel() {
  var panel = document.getElementById('sendPanel');
  var ch = channels[selectedChannel - 1];
  var canSend = !!(ch && (ch.CanSend || ch.canSend));
  if (selectedChannel > 0 && telegramLoggedIn && canSend) panel.classList.add('visible');
  else panel.classList.remove('visible');
}

