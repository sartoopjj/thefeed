// ===== SAVED MESSAGES (backend-backed) =====
var viewingSaved = false;
var savedMessages = [];
var savedLocked = false; // true when the store is passphrase-locked (HTTP 423)

// cssEsc escapes a string for use inside a querySelector attribute value, using
// the native CSS.escape where available and a minimal fallback for older
// WebViews that lack it.
function cssEsc(s) {
  s = String(s);
  if (window.CSS && CSS.escape) return CSS.escape(s);
  return s.replace(/["\\\]]/g, '\\$&');
}

async function saveMessage(channelNum, messageId) {
  var r = await fetch('/api/saved', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ channelNum: channelNum, messageId: messageId }) });
  if (!r.ok) throw new Error('save failed');
  return r.json();
}
async function unsaveMessage(id) {
  await fetch('/api/saved', { method: 'DELETE', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ id: id }) });
}
async function getAllSaved() {
  try {
    var r = await fetch('/api/saved');
    if (r.status === 423) { savedLocked = true; return []; }
    savedLocked = false;
    if (!r.ok) return [];
    var data = await r.json();
    return data.items || [];
  } catch (e) { return []; }
}
// getSavedUnseen returns true if the active profile has saved something newer
// than the last time it opened the Saved view. null on fetch failure.
async function getSavedUnseen() {
  try { var r = await fetch('/api/saved/count'); if (!r.ok) return null; var data = await r.json(); return !!data.unseen; } catch (e) { return null; }
}

// markSavedSeen clears the "new" dot on the server (survives reload/port change).
async function markSavedSeen() {
  try { await fetch('/api/saved/seen', { method: 'POST' }); } catch (e) {}
}
function isSavedLocal(channelId, messageId) {
  for (var i = 0; i < savedMessages.length; i++) { if (savedMessages[i].channelId === channelId && savedMessages[i].messageId === messageId) return true; }
  return false;
}
function findSavedId(channelId, messageId) {
  for (var i = 0; i < savedMessages.length; i++) {
    if (savedMessages[i].channelId === channelId && savedMessages[i].messageId === messageId) return savedMessages[i].id;
  }
  return '';
}
async function persistSavedMedia(size, crc) {
  try { await fetch('/api/saved/media/persist', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ size: size, crc: parseInt(crc, 16) }) }); } catch (e) {}
}

// Update the sidebar "new" dot. Pass a boolean to set it directly (no fetch);
// otherwise ask the server whether there's anything unseen. The dot shows when
// the profile has saved something since it last opened the Saved view.
async function updateSavedBadge(knownUnseen) {
  var badge = document.getElementById('savedBadge');
  if (!badge) return;
  var unseen = (typeof knownUnseen === 'boolean') ? knownUnseen : await getSavedUnseen();
  if (unseen === null) return; // fetch failed — leave the dot as-is
  badge.hidden = !unseen;
}

// Open the saved messages view (replaces chat panel content)
async function openSavedMessages() {
  viewingSaved = true;
  selectedChannel = 0;
  openChat();

  // Mark sidebar entry active
  document.querySelectorAll('.ch-item').forEach(function (el) { el.classList.remove('active'); });
  var entry = document.getElementById('savedChannelBtn');
  if (entry) entry.classList.add('active');

  // Update header — saved title + swap the feed actions for the saved ones
  // (search toggle + lock) via the .sm-active flag on the header.
  var chatName = document.getElementById('chatName');
  var chatSub = document.getElementById('chatSub');
  var header = document.querySelector('.chat-area > .chat-header') || document.querySelector('.chat-header');
  if (chatName) { chatName.textContent = t('saved_messages'); }
  if (chatSub) { chatSub.textContent = ''; }
  if (header) header.classList.add('sm-active');
  // Show the Saved (bookmark) icon in the header avatar — not the last
  // channel's leftover picture.
  var hAv = document.getElementById('chatHeaderAvatar');
  if (hAv) {
    hAv.classList.remove('ch-avatar-noimg');
    hAv.classList.add('is-saved');
    hAv.innerHTML = (typeof icon === 'function') ? icon('bookmark') : '';
  }

  // Hide send panel
  var sendPanel = document.getElementById('sendPanel');
  if (sendPanel) sendPanel.classList.remove('visible');

  // Fetch lock state to pick correct icon before rendering
  try {
    var ls = await (await fetch('/api/saved/lock')).json();
    _savedLockMode = ls.mode || 'device';
  } catch (e) {}
  updateSavedLockBtn();

  // Load and render
  savedMessages = await getAllSaved();
  renderSavedView();
  // Opening the view clears the "new" dot (persisted server-side).
  await markSavedSeen();
  updateSavedBadge(false);
}

function closeSavedMessages() {
  viewingSaved = false;
  // Remove saved-mode so #messages reverts to normal scrolling
  var messagesEl = document.getElementById('messages');
  if (messagesEl) messagesEl.classList.remove('saved-mode');
  // Restore the feed header actions (drop the saved-mode flag).
  var header = document.querySelector('.chat-area > .chat-header') || document.querySelector('.chat-header');
  if (header) { header.classList.remove('sm-active'); header.classList.remove('sm-search-open'); }
  openSidebar();
  var entry = document.getElementById('savedChannelBtn');
  if (entry) entry.classList.remove('active');
}

// _savedLockMode caches the last known lock state so the icon renders without
// an extra network round-trip on re-renders. Refreshed on every openSavedMessages().
var _savedLockMode = 'device'; // 'device' | 'passphrase'

// toggleSavedSearch shows/hides the search field (the title-bar search button).
// Hidden by default so the saved view isn't a permanent search bar.
function toggleSavedSearch() {
  var root = document.querySelector('.sm-saved-root');
  if (!root) return;
  var open = root.classList.toggle('sm-search-open');
  var btn = document.getElementById('savedSearchToggle');
  if (btn) btn.classList.toggle('on', open);
  var input = document.getElementById('savedSearch');
  if (open) {
    if (input) setTimeout(function () { try { input.focus(); } catch (e) {} }, 30);
  } else if (input && input.value) {
    input.value = '';
    filterSavedMessages(); // restore the full list when closing a search
  }
}

// updateSavedLockBtn refreshes the title-bar lock button's icon + tooltip from
// the current lock mode (it lives on the header now, not in the list).
function updateSavedLockBtn() {
  var btn = document.getElementById('savedLockBtn');
  if (!btn) return;
  var passOn = _savedLockMode === 'passphrase';
  btn.innerHTML = (typeof icon === 'function') ? icon(passOn ? 'lockKeyhole' : 'lockKeyholeOpen') : '';
  var ttl = passOn ? (t('remove_passphrase') || 'Remove passphrase') : (t('set_passphrase') || 'Set passphrase');
  btn.title = ttl;
  btn.setAttribute('aria-label', ttl);
}

function renderSavedView() {
  var el = document.getElementById('messages');
  if (!el) return;

  // Keep the title-bar lock button in sync (its mode can change via the gate).
  updateSavedLockBtn();

  // Mark #messages as a non-scrolling flex container for the saved view layout
  el.classList.add('saved-mode');

  // Locked: show an unlock gate instead of the timeline.
  // Device-key-lost (mode!=passphrase) shows a different message with no password field.
  if (savedLocked) {
    var isDeviceKeyLost = _savedLockMode !== 'passphrase';
    var gateTitle = isDeviceKeyLost
      ? esc(t('saved_device_key_lost') || 'Device encryption key lost')
      : esc(t('saved_locked') || 'Saved Messages is locked');
    var gateSub = isDeviceKeyLost
      ? esc(t('saved_device_key_lost_sub') || 'The local key that protected your saved messages is unreadable. Data cannot be recovered.')
      : esc(t('saved_locked_sub') || 'Content is sealed with AES-GCM and lives only on this device.');
    var gateHtml = '<div class="sm-saved-root"><div class="sm-gate">'
      + '<div class="sm-gate-icon"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="11" width="18" height="11" rx="2"/><path d="M7 11V7a5 5 0 0 1 10 0v4"/><circle cx="12" cy="16" r="1" fill="currentColor"/></svg></div>'
      + '<div class="sm-gate-title" dir="auto">' + gateTitle + '</div>'
      + '<div class="sm-gate-sub" dir="auto">' + gateSub + '</div>'
      + '<div class="sm-gate-enc"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z"/></svg>AES-GCM \xb7 Argon2id</div>';
    if (!isDeviceKeyLost) {
      gateHtml += '<div class="sm-pw-field">'
        + '<input class="sm-pw-input" type="password" id="savedUnlockInput" placeholder="' + escAttr(t('enter_passphrase') || 'Enter passphrase') + '" onkeydown="if(event.key===\'Enter\')unlockSavedView()">'
        + '<button class="sm-pw-eye" onclick="savedModalToggleUnlockEye()" type="button"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M2.062 12.348a1 1 0 0 1 0-.696 10.75 10.75 0 0 1 19.876 0 1 1 0 0 1 0 .696 10.75 10.75 0 0 1-19.876 0"/><circle cx="12" cy="12" r="3"/></svg></button>'
        + '</div>'
        + '<button class="sm-unlock-btn" onclick="unlockSavedView()">' + esc(t('unlock') || 'Unlock') + '</button>';
    }
    gateHtml += '<button class="sm-reset-link" onclick="resetSavedView()">' + esc(t('reset_saved') || 'Reset Saved Messages') + '</button>'
      + '</div></div>';
    el.innerHTML = gateHtml;
    if (!isDeviceKeyLost) {
      var inp = document.getElementById('savedUnlockInput');
      if (inp) inp.focus();
    }
    return;
  }

  var hasItems = savedMessages && savedMessages.length > 0;

  var html = '<div class="sm-saved-root"><div class="sm-list">';

  var pinnedItems = savedMessages ? savedMessages.filter(function(m) { return m.pinned; }) : [];

  // Search input — hidden by default; the title-bar search button toggles it
  // (.chat-header.sm-search-open). The lock button also lives on the title bar
  // now, so this row is just the search field.
  html += '<div class="sm-search-bar">'
    + '<div class="sm-search-inner">'
    + '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="11" cy="11" r="8"/><path d="m21 21-4.3-4.3"/></svg>'
    + '<input type="search" id="savedSearch" placeholder="' + escAttr(t('search_saved') || 'Search saved messages…') + '" oninput="filterSavedMessages()">'
    + '<button class="sm-search-clear" id="savedSearchClear" onclick="clearSavedSearch()" style="display:none" aria-label="Clear search"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><circle cx="12" cy="12" r="10"/><path d="m15 9-6 6"/><path d="m9 9 6 6"/></svg></button>'
    + '</div>'
    + '</div>';

  // Scrollable items (pinned banner rides at the top of the scroll so it slides
  // under the floating search bar with the rest of the list).
  html += '<div class="sm-scroll" id="savedList">';
  if (pinnedItems.length) {
    html += renderPinnedBanner(pinnedItems);
  }
  if (hasItems) {
    var lastDate = '';
    var dateLocale = lang === 'fa' ? 'fa-IR' : 'en-US';
    var dateOpts = lang === 'fa' ? { year: 'numeric', month: 'long', day: 'numeric', calendar: 'persian' } : { year: 'numeric', month: 'long', day: 'numeric' };
    for (var i = 0; i < savedMessages.length; i++) {
      var rec = savedMessages[i];
      var savedDate = new Date(rec.savedAt).toLocaleDateString(dateLocale, dateOpts);
      if (savedDate !== lastDate) {
        html += '<div class="sm-date-sep"><span>' + esc(savedDate) + '</span></div>';
        lastDate = savedDate;
      }
      html += renderSavedItem(rec);
    }
  } else {
    html += '<div class="sm-empty">'
      + '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><path d="m19 21-7-4-7 4V5a2 2 0 0 1 2-2h10a2 2 0 0 1 2 2v16z"/></svg>'
      + '<h3>' + esc(t('saved_empty_title') || 'Nothing saved yet') + '</h3>'
      + '<p>' + esc(t('saved_empty') || 'Bookmark messages, write notes, or upload files.') + '</p>'
      + '</div>';
  }
  html += '</div>'; // end .sm-scroll

  // Compose bar
  html += '<div class="sm-compose">'
    + '<button class="sm-compose-attach" onclick="document.getElementById(\'savedFileInput\').click()" aria-label="' + escAttr(t('upload_file') || 'Attach file') + '">'
    + '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m21.44 11.05-9.19 9.19a6 6 0 0 1-8.49-8.49l8.57-8.57A4 4 0 1 1 18 8.84l-8.59 8.57a2 2 0 0 1-2.83-2.83l8.49-8.48"/></svg>'
    + '</button>'
    + '<input type="file" id="savedFileInput" style="display:none" onchange="uploadSavedFile(this)">'
    + '<textarea class="sm-compose-input" id="savedInput" dir="auto" rows="1" placeholder="' + escAttr(t('note_placeholder') || 'Write a note…') + '" oninput="savedInputResize()" onkeydown="savedInputKey(event)"></textarea>'
    + '<button class="sm-compose-send" onclick="sendSavedNote()" aria-label="' + escAttr(t('chat_send') || 'Send') + '">'
    + '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><path d="M3.714 3.048a.498.498 0 0 0-.683.627l2.843 7.627a2 2 0 0 1 0 1.396l-2.842 7.627a.498.498 0 0 0 .682.627l18-8.5a.5.5 0 0 0 0-.904z"/><path d="M6 12h16"/></svg>'
    + '</button>'
    + '</div>';

  html += '</div></div>'; // end .sm-list + .sm-saved-root

  el.innerHTML = html;

  // Scroll to bottom (latest item)
  function jumpBottom() {
    var scroll = document.getElementById('savedList');
    if (scroll) scroll.scrollTop = scroll.scrollHeight;
  }
  jumpBottom();
  requestAnimationFrame(jumpBottom);
  setTimeout(jumpBottom, 60);
}

// Toggle show/hide on the unlock-gate password field
function savedModalToggleUnlockEye() {
  var inp = document.getElementById('savedUnlockInput');
  if (!inp) return;
  inp.type = inp.type === 'password' ? 'text' : 'password';
}

// _savedPinIdx is which pinned item the banner currently points at. Tapping the
// banner jumps to it and advances to the next one (Telegram-style cycling
// through multiple pins); the ✕ unpins ONLY the one being shown.
var _savedPinIdx = 0;

// renderPinnedBanner renders the compact banner for the pin at _savedPinIdx.
function renderPinnedBanner(pinned) {
  if (!pinned.length) return '';
  if (_savedPinIdx >= pinned.length) _savedPinIdx = 0;
  var cur = pinned[_savedPinIdx];
  var rawPreview = (cur.fileName || cur.text || '');
  // Strip internal media reference tokens (e.g. "[IMAGE]8862:1,1:15954:24:...")
  // so the banner shows human-readable text, not raw storage keys.
  var preview = rawPreview
    .replace(/\[(IMAGE|VIDEO|FILE|AUDIO)\][^\s]*/g, '')
    .replace(/\s+/g, ' ').trim()
    .slice(0, 80)
    || (cur.kind === 'file' ? (cur.fileName || '') : t('media_item') || 'Media');
  // "Pinned · 2/3" when there are several; just "Pinned" for one.
  var label = (t('pinned_label') || 'Pinned')
    + (pinned.length > 1 ? ' · ' + (_savedPinIdx + 1) + '/' + pinned.length : '');
  return '<div class="sm-pinned" id="savedPinBanner" onclick="savedPinCycle()">'
    + '<span class="sm-pinned-icon"><svg viewBox="0 0 24 24" fill="currentColor"><path d="m15.113 3.21.094.083 5.5 5.5a1 1 0 0 1-1.175 1.59l-3.172 3.171-1.424 3.797a1 1 0 0 1-.158.277l-.07.08-1.5 1.5a1 1 0 0 1-1.32.082l-.095-.083L9 16.415l-3.793 3.792a1 1 0 0 1-1.497-1.32l.083-.094L7.585 15l-2.792-2.793a1 1 0 0 1-.083-1.32l.083-.094 1.5-1.5a1 1 0 0 1 .258-.187l.098-.042 3.796-1.425 3.171-3.17a1 1 0 0 1 1.497-1.26z"/></svg></span>'
    + '<div class="sm-pinned-content">'
    + '<div class="sm-pinned-label">' + esc(label) + '</div>'
    + '<div class="sm-pinned-preview" dir="auto">' + esc(preview) + '</div>'
    + '</div>'
    + '<button class="sm-pinned-close" onclick="event.stopPropagation(); savedUnpinCurrent()" title="' + escAttr(t('unpin') || 'Unpin') + '"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><path d="M18 6 6 18"/><path d="m6 6 12 12"/></svg></button>'
    + '</div>';
}

// Rebuild the banner in place from the current pin list (e.g. after cycling).
function refreshPinnedBanner() {
  var old = document.getElementById('savedPinBanner');
  var pinned = (savedMessages || []).filter(function (m) { return m.pinned; });
  if (!pinned.length) { if (old) old.remove(); return; }
  var tmp = document.createElement('div');
  tmp.innerHTML = renderPinnedBanner(pinned);
  var fresh = tmp.firstChild;
  if (old) old.replaceWith(fresh);
  else { var sc = document.getElementById('savedList'); if (sc) sc.insertBefore(fresh, sc.firstChild); }
}

// Tap the banner: scroll to the pin it shows, then advance to the next pin so a
// repeated tap walks through all of them.
function savedPinCycle() {
  var pinned = (savedMessages || []).filter(function (m) { return m.pinned; });
  if (!pinned.length) return;
  if (_savedPinIdx >= pinned.length) _savedPinIdx = 0;
  var cur = pinned[_savedPinIdx];
  var card = document.querySelector('.sm-card[data-saved-id="' + cssEsc(cur.id) + '"]');
  if (card) card.scrollIntoView({ behavior: 'smooth', block: 'center' });
  _savedPinIdx = (_savedPinIdx + 1) % pinned.length; // next tap → next pin
  refreshPinnedBanner();
}

// ✕ on the banner: unpin ONLY the pin currently shown.
function savedUnpinCurrent() {
  var pinned = (savedMessages || []).filter(function (m) { return m.pinned; });
  if (!pinned.length) return;
  if (_savedPinIdx >= pinned.length) _savedPinIdx = 0;
  var cur = pinned[_savedPinIdx];
  // Keep the index pointing at a valid pin after removal.
  if (_savedPinIdx >= pinned.length - 1) _savedPinIdx = 0;
  pinSavedItem(cur.id, false); // updates the model + rebuilds the banner
}

// Clear the search input and reset filter
function clearSavedSearch() {
  var inp = document.getElementById('savedSearch');
  if (inp) { inp.value = ''; filterSavedMessages(); }
}

function renderSavedItem(record) {
  var text = record.text || '';
  var msgTs = record.timestamp || 0;
  var dateLocale = lang === 'fa' ? 'fa-IR' : 'en-US';
  var timeStr = msgTs ? new Date(msgTs * 1000).toLocaleTimeString(dateLocale, { hour: '2-digit', minute: '2-digit' }) : '';
  var savedTimeStr = new Date(record.savedAt).toLocaleTimeString(dateLocale, { hour: '2-digit', minute: '2-digit' });
  var displayTime = timeStr || savedTimeStr;

  var isOutgoing = false;
  var renderText = text;
  if (renderText.indexOf('[ME]\n') === 0) { isOutgoing = true; renderText = renderText.substring(5); }
  else if (renderText === '[ME]') { isOutgoing = true; renderText = ''; }

  // Kind discriminates the timeline. Legacy/bookmark records have no
  // kind -> treat as "bookmark". Declared before the media loop because the
  // pending-media branch reads isActive (var-hoisting bug otherwise).
  var kind = record.kind || 'bookmark';
  var isBookmark = kind === 'bookmark';
  var isNote = kind === 'note';
  var isFile = kind === 'file';
  var isChat = kind === 'chat';

  var available = record.available !== false;
  var isActive = record.isActive === true;
  var configLabel = record.configLabel || record.domain || '';
  var jumpDisabled = isActive ? '' : ' disabled';
  var isPinned = !!record.pinned;
  var isNew = !record.seen;
  var unavailableCls = available ? '' : ' unavailable';

  // ── Media (persisted blobs, pending downloads) ───────────
  var mediaHtml = '';
  var imageCards = [];
  var record_media = record.media || [];
  var captionText = renderText.replace(/^(\[(?:IMAGE|VIDEO|FILE|AUDIO|STICKER|GIF|CONTACT|LOCATION)\][^\n]*\n?)+/, '');
  for (var mi = 0; mi < record_media.length; mi++) {
    var md = record_media[mi];
    var crcHex = (md.crc >>> 0).toString(16);
    if (md.persisted) {
      var src = '/api/saved/media?size=' + md.size + '&crc=' + crcHex;
      var dlName = (record.kind === 'file' && record.fileName)
        ? record.fileName
        : md.fname
        ? md.fname
        : (md.tag.replace(/[\[\]]/g, '').toLowerCase() || 'media') + '-' + md.size;
      var dlBtn = '<button class="saved-media-save" onclick="savedMediaSave(\'' + jsStr(src) + '\',\'' + jsStr(dlName) + '\')" '
        + 'title="' + escAttr(t('download') || 'Download') + '" aria-label="' + escAttr(t('download') || 'Download') + '">'
        + icon('download') + '</button>';
      if (md.tag === '[IMAGE]' || md.tag === '[STICKER]' || md.tag === '[GIF]') {
        imageCards.push('<div class="media-card"><img class="media-img saved-media-open" src="' + src + '" loading="lazy" alt="" onclick="savedMediaOpen(this)">' + dlBtn + '</div>');
      } else if (md.tag === '[VIDEO]') {
        mediaHtml += '<div class="media-card"><video class="media-video" src="' + src + '" controls preload="metadata"></video>' + dlBtn + '</div>';
      } else {
        var extRaw = dlName.split('.').pop().toUpperCase().slice(0, 5) || 'FILE';
        var fIconCls = extRaw === 'PDF' ? 'pdf'
          : (extRaw === 'JPG' || extRaw === 'PNG' || extRaw === 'JPEG' || extRaw === 'WEBP' || extRaw === 'HEIC') ? 'img'
          : (extRaw === 'MP4' || extRaw === 'MOV' || extRaw === 'MKV') ? 'video'
          : (extRaw === 'DOC' || extRaw === 'DOCX') ? 'doc' : 'other';
        mediaHtml += '<div class="sm-file-row">'
          + '<div class="sm-file-icon ' + fIconCls + '">' + esc(extRaw) + '</div>'
          + '<div class="sm-file-meta">'
          + '<div class="sm-filename">' + esc(dlName) + '</div>'
          + '<div class="sm-filesize">' + formatBytes(md.size) + '</div>'
          + '</div></div>'
          + '<button class="sm-dl-btn" onclick="savedMediaSave(\'' + jsStr(src) + '\',\'' + jsStr(dlName) + '\')">'
          + '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M12 15V3"/><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="7,10 12,15 17,10"/></svg>'
          + esc(t('download') || 'Download')
          + '</button>';
      }
    } else if (isActive) {
      mediaHtml += '<div class="saved-media-pending"><button class="saved-media-dl" data-dl-size="' + md.size + '" data-dl-crc="' + escAttr(crcHex) + '">' + icon('download') + ' ' + md.tag + ' (' + formatBytes(md.size) + ')</button></div>';
    } else {
      mediaHtml += '<div class="saved-media-pending"><span class="saved-media-dl saved-media-dl-disabled">' + icon('download') + ' ' + md.tag + ' (' + formatBytes(md.size) + ')</span></div>';
    }
  }
  // Group images in a grid when there are multiple
  if (imageCards.length > 1) {
    var gridN = imageCards.length === 3 ? 3 : 2;
    var gridCls = 'sm-album sm-album-' + gridN;
    mediaHtml = '<div class="' + gridCls + '">' + imageCards.join('') + '</div>' + mediaHtml;
  } else if (imageCards.length === 1) {
    mediaHtml = imageCards[0] + mediaHtml;
  }

  // File without persisted blob — show filename tile if present
  var fileRowHtml = '';
  if (isFile && !mediaHtml && record.fileName) {
    var ext2 = (record.fileName.split('.').pop() || '').toUpperCase().slice(0, 5);
    var fIconCls2 = ext2 === 'PDF' ? 'pdf'
      : (ext2 === 'JPG' || ext2 === 'PNG' || ext2 === 'JPEG' || ext2 === 'WEBP') ? 'img'
      : (ext2 === 'MP4' || ext2 === 'MOV') ? 'video'
      : (ext2 === 'DOC' || ext2 === 'DOCX') ? 'doc' : 'other';
    fileRowHtml = '<div class="sm-file-row">'
      + '<div class="sm-file-icon ' + fIconCls2 + '">' + esc(ext2 || 'FILE') + '</div>'
      + '<div class="sm-file-meta">'
      + '<div class="sm-filename">' + esc(record.fileName) + '</div>'
      + (record.fileSize ? '<div class="sm-filesize">' + formatBytes(record.fileSize) + '</div>' : '')
      + '</div></div>';
  }

  var textHtml = captionText ? linkify(captionText) : (isNote ? linkify(renderText) : '');
  textHtml = textHtml.replace(/🇮🇷/g, '<img src="/static/lion-sun.svg" alt="" style="height:1.1em;vertical-align:middle">');

  // ── Kind chip icons ────────────────────────────────────────
  var kindChipIcon = '';
  var kindLabel = '';
  if (isBookmark) {
    kindChipIcon = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="m19 21-7-4-7 4V5a2 2 0 0 1 2-2h10a2 2 0 0 1 2 2v16z"/></svg>';
    kindLabel = t('bookmark') || 'Bookmark';
  } else if (isNote) {
    kindChipIcon = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M11 4H4a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7"/><path d="M18.5 2.5a2.121 2.121 0 0 1 3 3L12 15l-4 1 1-4 9.5-9.5z"/></svg>';
    kindLabel = t('note') || 'Note';
  } else if (isFile) {
    kindChipIcon = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="17,8 12,3 7,8"/><line x1="12" y1="3" x2="12" y2="15"/></svg>';
    kindLabel = t('file') || 'File';
  } else if (isChat) {
    kindChipIcon = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M7.9 20A9 9 0 1 0 4 16.1L2 22z"/></svg>';
    kindLabel = t('messenger') || 'Messenger';
  }

  // ── Source label ───────────────────────────────────────────
  var sourceHtml = '';
  if (isBookmark) {
    var removedNote = available ? '' : ' <span class="sm-config-removed">(' + esc(t('config_removed') || 'removed') + ')</span>';
    sourceHtml = '<span class="sm-card-source">' + esc(configLabel ? configLabel + ' \xb7 ' : '') + esc(record.channelId) + removedNote + '</span>';
  }

  // ── Card HTML ──────────────────────────────────────────────
  // Buttons use data-attributes + event delegation (see initSavedMessages)
  // rather than inline onclick, so the wire-protocol channel name embedded in
  // record.id can never break out of a JS-string context.
  var html = '<div class="sm-card ' + kind + (isPinned ? ' pinned-item' : '') + unavailableCls + '"'
    + ' data-saved-id="' + escAttr(record.id) + '"'
    + ' data-kind="' + kind + '"'
    + ' data-pinned="' + (isPinned ? '1' : '0') + '"'
    + '>';

  // New-item dot
  if (isNew) html += '<div class="sm-new-dot"></div>';

  // Card header
  html += '<div class="sm-card-hd">'
    + '<span class="sm-kind-chip ' + kind + '">' + kindChipIcon + esc(kindLabel) + '</span>'
    + sourceHtml
    + '<span class="sm-card-time">' + esc(displayTime) + '</span>'
    + '<div class="sm-card-actions">'
    + '<button class="sm-action-btn pin' + (isPinned ? ' on' : '') + '" data-action="pin" title="' + escAttr(isPinned ? (t('unpin') || 'Unpin') : (t('pin') || 'Pin')) + '">'
    + '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M12 17v5"/><path d="M9 10.76a2 2 0 0 1-1.11 1.79l-1.78.9A2 2 0 0 0 5 15.24V16a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1v-.76a2 2 0 0 0-1.11-1.79l-1.78-.9A2 2 0 0 1 15 10.76V7a1 1 0 0 1 1-1 2 2 0 0 0 0-4H8a2 2 0 0 0 0 4 1 1 0 0 1 1 1z"/></svg>'
    + '</button>'
    + '<button class="sm-action-btn copy" data-action="copy" title="' + escAttr(t('copy') || 'Copy') + '">'
    + '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg>'
    + '</button>'
    + '<button class="sm-action-btn del" data-action="remove" title="' + escAttr(t('remove_from_saved') || 'Delete') + '">'
    + '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="3,6 5,6 21,6"/><path d="M19 6l-1 14a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2L5 6"/><path d="M10 11v6"/><path d="M14 11v6"/></svg>'
    + '</button>'
    + '</div>'
    + '</div>'; // end .sm-card-hd

  // Chat sender row
  if (isChat && record.nickname) {
    var sName = record.nickname;
    var sInitial = sName.charAt(0).toUpperCase();
    var avatarColors = ['#7c3aed', '#2563eb', '#059669', '#d97706', '#dc2626'];
    var sColor = avatarColors[sName.charCodeAt(0) % avatarColors.length];
    html += '<div class="sm-sender-row">'
      + '<div class="sm-sender-av" style="background:' + sColor + '">' + esc(sInitial) + '</div>'
      + '<span class="sm-sender-name">' + esc(sName) + '</span>'
      + '<span class="sm-sender-e2e"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="3" y="11" width="18" height="11" rx="2"/><path d="M7 11V7a5 5 0 0 1 10 0v4"/></svg>E2E</span>'
      + '</div>';
  }

  // File tile (no persisted blob yet)
  html += fileRowHtml;

  // Persisted media (images, videos, file rows)
  html += mediaHtml;

  // Text / caption
  if (textHtml) {
    html += '<p class="sm-card-text" dir="auto">' + textHtml + '</p>';
  }

  // Jump button (bookmarks and Telemirror items)
  if (isBookmark) {
    html += '<button class="sm-jump-btn" data-action="jump" data-jump-ch="' + escAttr(record.channelId) + '" data-jump-msg="' + record.messageId + '"' + jumpDisabled + '>'
      + '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M15 3h6v6"/><path d="M10 14 21 3"/><path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6"/></svg>'
      + esc(t('jump_to_original') || 'Jump to message')
      + '</button>';
  } else if (record.tmSource) {
    html += '<button class="sm-jump-btn" data-action="tm-jump" data-tm-source="' + escAttr(record.tmSource) + '">'
      + '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M15 3h6v6"/><path d="M10 14 21 3"/><path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6"/></svg>'
      + esc(t('jump_to_original') || 'Jump')
      + '</button>';
  }

  html += '</div>'; // end .sm-card
  return html;
}

// Auto-grow the note textarea (1–6 rows) like the messenger composer.
function savedInputResize() {
  var ta = document.getElementById('savedInput');
  if (!ta) return;
  ta.style.height = 'auto';
  ta.style.height = Math.min(ta.scrollHeight, 140) + 'px';
}

// Enter sends; Shift+Enter inserts a newline.
function savedInputKey(e) {
  if (e.key === 'Enter' && !e.shiftKey) {
    e.preventDefault();
    sendSavedNote();
  }
}

// sendSavedNote posts the composer text as a kind:"note" item and appends it.
async function sendSavedNote() {
  var ta = document.getElementById('savedInput');
  if (!ta) return;
  var text = (ta.value || '').trim();
  if (!text) return;
  ta.disabled = true;
  try {
    var r = await fetch('/api/saved/note', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ text: text })
    });
    if (!r.ok) { showToast(t('saved_save_failed') || 'Could not save'); return; }
    var item = await r.json();
    savedMessages.push(item);
    ta.value = '';
    renderSavedView();
    updateSavedBadge();
  } catch (e) {
    showToast(t('saved_save_failed') || 'Could not save');
  } finally {
    var ta2 = document.getElementById('savedInput');
    if (ta2) { ta2.disabled = false; ta2.focus(); }
  }
}

// uploadSavedFile sends the chosen file to the backend as a kind:"file" item.
// On Android WebView the File object's .size can be 0 at the moment the
// change event fires (HEIC/gallery picker hasn't resolved the bytes yet).
// We always read the actual bytes first via arrayBuffer() so the multipart
// payload has real content — the server then sees the correct size.
async function uploadSavedFile(inputEl) {
  var f = inputEl.files && inputEl.files[0];
  inputEl.value = ''; // allow re-selecting the same file later
  if (!f) return;
  showToast(t('uploading') || 'Uploading…');
  try {
    // Read into memory first — resolves the 0-size HEIC problem on Android.
    var buf = await f.arrayBuffer();
    if (!buf || buf.byteLength === 0) { showToast(t('saved_save_failed') || 'Could not read file'); return; }
    if (buf.byteLength > 50 * 1024 * 1024) { showToast(t('file_too_large') || 'File is too large (max 50 MB)'); return; }
    var blob = new Blob([buf], { type: f.type || 'application/octet-stream' });
    var fd = new FormData();
    fd.append('file', blob, f.name);
    var r = await fetch('/api/saved/upload', { method: 'POST', body: fd });
    if (!r.ok) { showToast(r.status === 413 ? (t('file_too_large') || 'File is too large (max 50 MB)') : (t('saved_save_failed') || 'Could not save')); return; }
    var item = await r.json();
    savedMessages.push(item);
    renderSavedView();
    updateSavedBadge();
  } catch (e) {
    showToast(t('saved_save_failed') || 'Could not save');
  }
}

// unlockSavedView submits the passphrase from the lock gate and reloads.
async function unlockSavedView() {
  var inp = document.getElementById('savedUnlockInput');
  var pass = inp ? inp.value : '';
  if (!pass) return;
  try {
    var r = await fetch('/api/saved/unlock', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ passphrase: pass }) });
    if (r.status === 401) { showToast(t('wrong_passphrase') || 'Wrong passphrase'); return; }
    if (!r.ok) { showToast(t('saved_save_failed') || 'Could not save'); return; }
    savedLocked = false;
    savedMessages = await getAllSaved();
    renderSavedView();
  } catch (e) { showToast(t('saved_save_failed') || 'Could not save'); }
}

// resetSavedView is the forgotten-passphrase escape hatch (destructive).
// Uses the custom modal instead of confirm() (which is disabled on Android).
function resetSavedView() {
  showSavedModal({
    title: t('reset_saved') || 'Reset Saved Messages',
    body: '<p style="color:var(--error);font-size:var(--text-sm);margin:0 0 8px;font-weight:600">' + (t('warning') || '⚠ Warning') + '</p>'
      + '<p style="color:var(--text-dim);font-size:var(--text-sm);margin:0">' + (t('reset_saved_confirm') || 'This permanently deletes ALL saved messages and the passphrase. This cannot be undone.') + '</p>',
    confirmLabel: t('reset_saved') || 'Delete Everything',
    confirmClass: 'btn-danger',
    onConfirm: async function() {
      try {
        var r = await fetch('/api/saved/lock/reset', { method: 'POST' });
        if (!r.ok) { showToast(t('saved_save_failed') || 'Could not save'); return false; }
        savedLocked = false;
        _savedLockMode = 'device';
        savedMessages = await getAllSaved();
        renderSavedView();
      } catch (e) { showToast(t('saved_save_failed') || 'Could not save'); }
    }
  });
}

// toggleSavedPassphrase — shows the custom passphrase modal (device-safe;
// replaces the old browser prompt/confirm which is disabled on Android WebView).
async function toggleSavedPassphrase() {
  try {
    var st = await (await fetch('/api/saved/lock')).json();
    if (st.mode === 'passphrase') {
      showSavedModal({
        title: t('remove_passphrase') || 'Remove Passphrase',
        body: '<p style="color:var(--text-dim);font-size:var(--text-sm);margin:0 0 12px">' + (t('remove_passphrase_desc') || 'This will revert to transparent device-key protection. Your saved data will stay.') + '</p>',
        confirmLabel: t('remove_passphrase') || 'Remove',
        confirmClass: 'btn-danger',
        onConfirm: async function() {
          var r = await fetch('/api/saved/lock/remove', { method: 'POST' });
          if (r.ok) _savedLockMode = 'device';
          showToast(r.ok ? (t('passphrase_removed') || 'Passphrase removed') : (t('saved_save_failed') || 'Could not save'));
          renderSavedView();
        }
      });
      return;
    }
    showSavedModal({
      title: t('set_passphrase') || 'Set Passphrase',
      body: '<p style="color:var(--text-dim);font-size:var(--text-sm);margin:0 0 12px">' + (t('set_passphrase_desc') || 'Encrypts your saved messages with a passphrase. You will need to unlock each session.') + '</p>'
        + '<div class="saved-modal-field">'
        + '<input type="password" id="savedModalPass" class="saved-modal-input" placeholder="' + escAttr(t('enter_passphrase') || 'Enter passphrase') + '" autocomplete="new-password">'
        + '<button type="button" class="saved-modal-eye" onclick="savedModalToggleEye()">'
        + '<svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M2.062 12.348a1 1 0 0 1 0-.696 10.75 10.75 0 0 1 19.876 0 1 1 0 0 1 0 .696 10.75 10.75 0 0 1-19.876 0"/><circle cx="12" cy="12" r="3"/></svg>'
        + '</button></div>',
      confirmLabel: t('set_passphrase') || 'Set',
      onConfirm: async function() {
        var inp = document.getElementById('savedModalPass');
        var pass = inp ? inp.value : '';
        if (!pass) { showToast(t('enter_passphrase') || 'Enter a passphrase'); return false; }
        var r2 = await fetch('/api/saved/lock', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ passphrase: pass }) });
        if (r2.ok) _savedLockMode = 'passphrase';
        showToast(r2.ok ? (t('passphrase_set') || 'Passphrase set') : (t('saved_save_failed') || 'Could not save'));
        renderSavedView();
      },
      onOpen: function() {
        var inp = document.getElementById('savedModalPass');
        if (inp) inp.focus();
      }
    });
  } catch (e) { showToast(t('saved_save_failed') || 'Could not save'); }
}

// savedModalToggleEye: show/hide the passphrase input characters
function savedModalToggleEye() {
  var inp = document.getElementById('savedModalPass');
  if (!inp) return;
  inp.type = inp.type === 'password' ? 'text' : 'password';
}

// showSavedModal — tiny custom modal (replaces prompt/confirm; works on Android).
// opts: { title, body(html), confirmLabel, confirmClass, onConfirm, onOpen }
function showSavedModal(opts) {
  var existing = document.getElementById('savedModalOverlay');
  if (existing) existing.remove();

  var overlay = document.createElement('div');
  overlay.id = 'savedModalOverlay';
  overlay.className = 'saved-modal-overlay';
  overlay.innerHTML =
    '<div class="saved-modal-box" role="dialog" aria-modal="true">'
    + '<div class="saved-modal-title">' + esc(opts.title || '') + '</div>'
    + '<div class="saved-modal-body">' + (opts.body || '') + '</div>'
    + '<div class="saved-modal-actions">'
    + '<button class="saved-modal-btn saved-modal-cancel" id="savedModalCancel">' + esc(t('cancel') || 'Cancel') + '</button>'
    + '<button class="saved-modal-btn saved-modal-confirm ' + (opts.confirmClass || '') + '" id="savedModalConfirm">' + esc(opts.confirmLabel || 'OK') + '</button>'
    + '</div></div>';

  function onKeydown(e) { if (e.key === 'Escape') closeModal(); }
  function closeModal() {
    document.removeEventListener('keydown', onKeydown); // remove on every close path, not just Escape
    overlay.remove();
  }

  overlay.addEventListener('click', function(e) { if (e.target === overlay) closeModal(); });
  overlay.querySelector('#savedModalCancel').addEventListener('click', closeModal);
  overlay.querySelector('#savedModalConfirm').addEventListener('click', async function() {
    var result = opts.onConfirm && (await opts.onConfirm());
    if (result !== false) closeModal();
  });
  document.addEventListener('keydown', onKeydown);

  document.body.appendChild(overlay);
  requestAnimationFrame(function() { overlay.classList.add('visible'); });
  if (opts.onOpen) setTimeout(opts.onOpen, 50);
}

// pinSavedItem toggles the pinned flag. Updates the banner + item in-place
// for a fast, animation-friendly result without a full re-render.
async function pinSavedItem(savedId, pinned) {
  try {
    var r = await fetch('/api/saved/pin', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ id: savedId, pinned: pinned }) });
    if (!r.ok) { showToast(t('saved_save_failed') || 'Could not save'); return; }
    // Update in-memory model
    for (var i = 0; i < savedMessages.length; i++) {
      if (savedMessages[i].id === savedId) { savedMessages[i].pinned = pinned; break; }
    }
    // Targeted DOM update: flip the item's data-pinned attribute + pin button state
    var wrapper = document.querySelector('.sm-card[data-saved-id="' + cssEsc(savedId) + '"]');
    if (wrapper) {
      wrapper.setAttribute('data-pinned', pinned ? '1' : '0');
      if (pinned) wrapper.classList.add('pinned-item'); else wrapper.classList.remove('pinned-item');
      var pb = wrapper.querySelector('[data-action="pin"]');
      if (pb) {
        if (pinned) pb.classList.add('on'); else pb.classList.remove('on');
        pb.title = pinned ? (t('unpin') || 'Unpin') : (t('pin') || 'Pin');
      }
    }
    // Refresh the pinned banner. Reset the cycle to the first pin so the banner
    // shows a sensible item after a pin/unpin (renderPinnedBanner clamps anyway).
    _savedPinIdx = 0;
    var oldBanner = document.getElementById('savedPinBanner');
    var pinnedNow = savedMessages.filter(function(m) { return m.pinned; });
    if (pinnedNow.length) {
      var newBannerHtml = renderPinnedBanner(pinnedNow);
      var tmp = document.createElement('div');
      tmp.innerHTML = newBannerHtml;
      var newBanner = tmp.firstChild;
      if (oldBanner) {
        oldBanner.replaceWith(newBanner);
      } else {
        // Insert at the TOP of the scroll list (where the initial render puts
        // it) — NOT before the floating search bar, which dropped it outside the
        // scroll and behind the title/search bars at the very top of the page.
        var scroll = document.getElementById('savedList');
        if (scroll) scroll.insertBefore(newBanner, scroll.firstChild);
      }
    } else if (oldBanner) {
      oldBanner.remove();
    }
    showToast(pinned ? (t('pinned_toast') || 'Pinned') : (t('unpinned_toast') || 'Unpinned'));
  } catch (e) { showToast(t('saved_save_failed') || 'Could not save'); }
}

function filterSavedMessages() {
  var inp = document.getElementById('savedSearch');
  var q = (inp ? inp.value : '').toLowerCase();
  var clearBtn = document.getElementById('savedSearchClear');
  if (clearBtn) clearBtn.style.display = q ? '' : 'none';
  var items = document.querySelectorAll('.sm-card');
  items.forEach(function(el) {
    var texts = ['.sm-card-text', '.sm-filename', '.sm-card-source', '.sm-sender-name']
      .map(function(sel) { var n = el.querySelector(sel); return n ? n.textContent : ''; })
      .join(' ').toLowerCase();
    el.style.display = texts.includes(q) ? '' : 'none';
  });
}

async function jumpToSavedOriginal(channelId, messageId) {
  // Find the channel number
  var chNum = 0;
  for (var i = 0; i < channels.length; i++) {
    if ((channels[i].Name || channels[i].name) === channelId) { chNum = i + 1; break; }
  }
  if (!chNum) { showToast(t('channel_removed') || 'Channel removed'); return; }

  // Restore the normal chat layout (removes #messages.saved-mode + header) before
  // navigating, otherwise the saved-view flex layout leaks into the channel.
  closeSavedMessages();

  gotoChannelPost(chNum, messageId);
}

// jumpToTelemirrorPost reopens the Telemirror browser at the saved post's
// channel and scrolls to the exact post if it's in the loaded set. tmSource is
// "channel/msgNum". Leaves the Saved view (closes it) like the bookmark jump.
function jumpToTelemirrorPost(tmSource) {
  var channel = (tmSource || '').split('/')[0];
  if (!channel) return;
  closeSavedMessages();
  if (typeof openTelemirror !== 'function') { showToast(t('msg_not_in_cache') || 'Unavailable'); return; }
  openTelemirror();
  // Select the channel once the modal is up, then try to scroll to the post.
  setTimeout(function () {
    if (typeof tmSelectFromClick === 'function') tmSelectFromClick(channel);
    setTimeout(function () {
      var el = document.querySelector('.tm-post[data-post-id="' + cssEsc(tmSource) + '"]');
      if (el) {
        el.scrollIntoView({ behavior: 'auto', block: 'center' });
        el.classList.add('tm-post-highlight');
        setTimeout(function () { el.classList.remove('tm-post-highlight'); }, 2000);
      }
    }, 1500);
  }, 300);
}

async function removeSavedItem(savedId, btnEl) {
  try {
    await unsaveMessage(savedId);
  } catch (e) {
    showToast(t('saved_save_failed') || 'Could not save');
    return;
  }
  savedMessages = savedMessages.filter(function(m) { return m.id !== savedId; });
  var item = btnEl.closest('.sm-card');
  if (item) {
    item.classList.add('removing');
    setTimeout(function() {
      item.remove();
      if (savedMessages.length === 0) { renderSavedView(); return; }
      // Refresh pinned banner in case this item was pinned
      var pinnedNow = savedMessages.filter(function(m) { return m.pinned; });
      var oldBanner = document.getElementById('savedPinBanner');
      if (!pinnedNow.length && oldBanner) {
        oldBanner.remove();
      } else if (pinnedNow.length && oldBanner) {
        var tmp = document.createElement('div');
        tmp.innerHTML = renderPinnedBanner(pinnedNow);
        oldBanner.replaceWith(tmp.firstChild);
      }
    }, 250);
  }
  updateSavedBadge();
  showToast(t('unsaved_toast') || 'Removed from Saved');
}

async function downloadSavedMedia(btnEl, savedId, size, crcHex) {
  btnEl.disabled = true;
  btnEl.textContent = t('loading') || 'Loading...';
  var rec = null;
  for (var i = 0; i < savedMessages.length; i++) { if (savedMessages[i].id === savedId) { rec = savedMessages[i]; break; } }
  if (!rec) { btnEl.disabled = false; return; }
  var parsed = findMediaDescriptor(rec.text, size, crcHex);
  if (!parsed) { btnEl.disabled = false; showToast(t('msg_not_in_cache') || 'Unavailable'); return; }
  // Pick the source the same way the feed does: prefer the GitHub relay (fast),
  // fall back to DNS (slow). GH-only media has ch/blk == 0 — the fast path is
  // keyed by size+crc, not channel, so it still works; hardcoding slow was the
  // bug. A successful fetch is also served from the server cache by (size,crc),
  // so a prior channel download is reused here too.
  var sources = [];
  if (parsed.githubAvailable) sources.push('fast');
  if (parsed.dnsAvailable) sources.push('slow');
  if (sources.length === 0) sources.push('slow');
  try {
    var ok = false;
    for (var si = 0; si < sources.length; si++) {
      var url = '/api/media/get?ch=' + parsed.channel + '&blk=' + parsed.blocks
        + '&size=' + parsed.size + '&crc=' + parsed.crc + '&source=' + sources[si];
      var r = await fetch(url);
      if (r.ok) { await r.blob(); ok = true; break; }
    }
    if (!ok) throw new Error('download failed');
    await persistSavedMedia(size, crcHex);
    savedMessages = await getAllSaved();
    renderSavedView();
  } catch (e) { btnEl.disabled = false; showToast(t('media_failed') || 'Download failed'); }
}
// findMediaDescriptor locates the descriptor whose (size, crc) matches the
// clicked item. A structured media tag ("[TAG]size:flags:ch:blk:crc") can sit
// anywhere in the text — after a caption, inside a [REPLY] wrapper, or as the
// Nth item of an album — so we scan for every occurrence rather than assuming
// it starts at position 0. We do NOT require ch/blk > 0: GH-relay-only media
// legitimately has ch == blk == 0, and the caller fetches it via the fast path.
var savedMediaTagRe = /\[(IMAGE|VIDEO|FILE|AUDIO|STICKER|GIF|CONTACT|LOCATION)\]\d+:[0-9,]+:\d+:\d+:[0-9a-fA-F]+/g;
function findMediaDescriptor(text, size, crcHex) {
  var wantCrc = parseInt(crcHex, 16);
  savedMediaTagRe.lastIndex = 0;
  var m;
  while ((m = savedMediaTagRe.exec(text)) !== null) {
    var p = parseDownloadableMedia(text.substring(m.index), '[' + m[1] + ']');
    if (p.size === size && p.crc && parseInt(p.crc, 16) === wantCrc) return p;
  }
  return null;
}

// Context menu logic
var msgContextMenu = null;
var longPressTimer = null;
var longPressMsgEl = null;
// After a long-press opens the menu, Android/iOS WebViews fire a synthesized
// "click" on the element under the finger when it lifts. On a downloaded-media
// post that element is .media-image-loaded (onclick=mediaOpen → lightbox), so
// the stray click would open the viewer AND dismiss our menu. This flag lets a
// capturing handler swallow exactly that one click.
var suppressNextMsgClick = false;

function showMsgContextMenu(msgEl, x, y) {
  closeMsgContextMenu();
  if (viewingSaved || !selectedChannel) return; // no context menu in saved view
  var msgDiv = msgEl.closest('.msg');
  if (!msgDiv) return;

  // Get message data from the DOM
  var metaEl = msgDiv.querySelector('.msg-meta');
  if (!metaEl) return;
  var idSpan = metaEl.querySelector('span');
  if (!idSpan) return;
  var msgId = parseInt((idSpan.textContent || '').replace('#', ''));
  if (!msgId) return;

  var chName = channelName(selectedChannel);

  var menu = document.createElement('div');
  menu.className = 'msg-context-menu';
  menu.style.position = 'fixed';
  menu.style.left = Math.min(x, window.innerWidth - 180) + 'px';
  menu.style.top = Math.min(y, window.innerHeight - 80) + 'px';

  var saved = isSavedLocal(chName, msgId);
  if (saved) {
    menu.innerHTML = '<button onclick="contextMenuUnsave(' + msgId + ')">' + icon('bookmark') + ' ' + t('remove_from_saved') + '</button>';
  } else {
    menu.innerHTML = '<button onclick="contextMenuSave(' + msgId + ')">' + icon('bookmark') + ' ' + t('forward_to_saved') + '</button>';
  }
  document.body.appendChild(menu);
  msgContextMenu = menu;

  // Dismiss on click outside
  setTimeout(function () {
    document.addEventListener('click', closeMsgContextMenu, { once: true });
    document.addEventListener('scroll', closeMsgContextMenu, { once: true, capture: true });
  }, 50);
}

function closeMsgContextMenu() {
  if (msgContextMenu) {
    msgContextMenu.remove();
    msgContextMenu = null;
  }
}

async function contextMenuSave(msgId) {
  closeMsgContextMenu();
  try {
    await saveMessage(selectedChannel, msgId);
    // Refresh local cache so isSavedLocal() reflects the new item
    getAllSaved().then(function(items) { savedMessages = items; });
    updateSavedBadge();
    showToast(t('saved_toast') || 'Forwarded to Saved');
    // Update the inline save button state if it's visible
    _refreshSaveBtnState(msgId);
  } catch (e) { showToast(t('save_failed') || 'Could not save'); }
}

async function contextMenuUnsave(msgId) {
  closeMsgContextMenu();
  var chName = channelName(selectedChannel);
  var id = findSavedId(chName, msgId);
  if (!id) return;
  try {
    await unsaveMessage(id);
  } catch (e) {
    showToast(t('saved_save_failed') || 'Could not save');
    return;
  }
  getAllSaved().then(function(items) { savedMessages = items; });
  updateSavedBadge();
  showToast(t('unsaved_toast') || 'Removed from Saved');
  _refreshSaveBtnState(msgId);
}

// msgSaveToggle — called by the inline .msg-save-btn in the message meta bar.
async function msgSaveToggle(msgId, btnEl) {
  if (!selectedChannel) return;
  var chName = channelName(selectedChannel);
  var alreadySaved = isSavedLocal(chName, msgId);
  if (alreadySaved) {
    var id = findSavedId(chName, msgId);
    if (!id) return;
    await unsaveMessage(id);
    getAllSaved().then(function(items) { savedMessages = items; });
    updateSavedBadge();
    showToast(t('unsaved_toast') || 'Removed from Saved');
    if (btnEl) btnEl.classList.remove('msg-save-btn-active');
  } else {
    try {
      await saveMessage(selectedChannel, msgId);
      getAllSaved().then(function(items) { savedMessages = items; });
      updateSavedBadge();
      showToast(t('saved_toast') || 'Forwarded to Saved');
      if (btnEl) btnEl.classList.add('msg-save-btn-active');
    } catch (e) { showToast(t('save_failed') || 'Could not save'); }
  }
}

// Update the visual state of a specific msg-save-btn after a save/unsave action.
function _refreshSaveBtnState(msgId) {
  var btn = document.querySelector('.msg-save-btn[onclick*="' + msgId + '"]');
  if (!btn) return;
  var chName = channelName(selectedChannel);
  btn.classList.toggle('msg-save-btn-active', isSavedLocal(chName, msgId));
}

// Attach context menu listeners (call after DOM ready)
function initSavedMessages() {
  var messagesEl = document.getElementById('messages');
  if (!messagesEl) return;

  // Capture phase: eat the one synthesized click that a long-press emits when
  // the finger lifts, so it can't open the media lightbox or dismiss the menu.
  // The menu itself lives in document.body (outside #messages), so deliberate
  // taps on its buttons are unaffected.
  messagesEl.addEventListener('click', function (e) {
    if (suppressNextMsgClick) {
      suppressNextMsgClick = false;
      e.stopPropagation();
      e.preventDefault();
    }
  }, true);

  // Desktop: hover ⋮ button click
  messagesEl.addEventListener('click', function (e) {
    var btn = e.target.closest('.msg-more-btn');
    if (btn) {
      e.stopPropagation();
      var rect = btn.getBoundingClientRect();
      showMsgContextMenu(btn, rect.left, rect.bottom + 4);
      return;
    }
    // Saved-view action buttons (delegated; data-attributes are HTML-decoded
    // by getAttribute, so the channel name in the id can't break out).
    var jb = e.target.closest('[data-action="jump"]');
    if (jb && !jb.disabled) {
      jumpToSavedOriginal(jb.getAttribute('data-jump-ch'), parseInt(jb.getAttribute('data-jump-msg'), 10));
      return;
    }
    var tmb = e.target.closest('[data-action="tm-jump"]');
    if (tmb) {
      jumpToTelemirrorPost(tmb.getAttribute('data-tm-source'));
      return;
    }
    var rb = e.target.closest('[data-action="remove"]');
    if (rb) {
      var w = rb.closest('.sm-card');
      if (w) removeSavedItem(w.getAttribute('data-saved-id'), rb);
      return;
    }
    var cb = e.target.closest('[data-action="copy"]');
    if (cb) {
      var cw = cb.closest('.sm-card');
      if (cw) copySavedText(cw.getAttribute('data-saved-id'));
      return;
    }
    var pb = e.target.closest('[data-action="pin"]');
    if (pb) {
      var pw = pb.closest('.sm-card');
      if (pw) pinSavedItem(pw.getAttribute('data-saved-id'), pw.getAttribute('data-pinned') !== '1');
      return;
    }
    var db = e.target.closest('.saved-media-dl');
    if (db) {
      var w2 = db.closest('.sm-card');
      if (w2) downloadSavedMedia(db, w2.getAttribute('data-saved-id'), parseInt(db.getAttribute('data-dl-size'), 10), db.getAttribute('data-dl-crc'));
      return;
    }
  });

  // Mobile: long-press
  messagesEl.addEventListener('touchstart', function (e) {
    var msgEl = e.target.closest('.msg');
    if (!msgEl || viewingSaved) return;
    var touch = e.touches[0];
    var startX = touch.clientX, startY = touch.clientY;
    longPressMsgEl = msgEl;

    longPressTimer = setTimeout(function () {
      if (navigator.vibrate) navigator.vibrate(50);
      // Arm the click-swallower for the synthesized click on finger lift.
      // Safety-clear after 1s so it never eats an unrelated later tap.
      suppressNextMsgClick = true;
      setTimeout(function () { suppressNextMsgClick = false; }, 1000);
      showMsgContextMenu(msgEl, startX, startY);
    }, 500);

    function cancel() {
      clearTimeout(longPressTimer);
      longPressTimer = null;
      msgEl.removeEventListener('touchmove', onMove);
      msgEl.removeEventListener('touchend', cancel);
    }
    function onMove(ev) {
      var dx = ev.touches[0].clientX - startX;
      var dy = ev.touches[0].clientY - startY;
      if (Math.abs(dx) > 10 || Math.abs(dy) > 10) cancel();
    }
    msgEl.addEventListener('touchmove', onMove, { passive: true });
    msgEl.addEventListener('touchend', cancel, { once: true });
  }, { passive: true });

  // Escape to close
  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape') closeMsgContextMenu();
  });

  // Load badge on init
  updateSavedBadge();
  getAllSaved().then(function (items) {
    savedMessages = items;
    // Reflect saved state on all currently rendered msg-save-btn elements
    document.querySelectorAll('.msg-save-btn').forEach(function(btn) {
      var match = (btn.getAttribute('onclick') || '').match(/msgSaveToggle\((\d+)/);
      if (!match) return;
      var mid = parseInt(match[1], 10);
      if (!mid || !selectedChannel) return;
      var chName = channelName(selectedChannel);
      btn.classList.toggle('msg-save-btn-active', isSavedLocal(chName, mid));
    });
  });
}

function copySavedText(savedId) {
  for (var i = 0; i < savedMessages.length; i++) {
    if (savedMessages[i].id === savedId) {
      var text = (savedMessages[i].text || '').replace(/^\[ME\]\n?/, '');
      text = text.replace(/\[(?:IMAGE|VIDEO|FILE|AUDIO|STICKER|GIF|CONTACT|LOCATION)\][^\n]*/g, '').trim();
      if (!text) { showToast(t('nothing_to_copy') || 'Nothing to copy'); return; }
      if (navigator.clipboard) {
        navigator.clipboard.writeText(text).then(function() {
          showToast(t('copied') || 'Copied');
        });
      }
      return;
    }
  }
}

// Blob-based download that works in all WebViews (the <a download> attribute
// is unreliable in WKWebView and some Android WebViews).
async function savedMediaSave(url, filename) {
  try {
    var r = await fetch(url);
    if (!r.ok) { showToast(t('msg_not_in_cache') || 'Unavailable'); return; }
    var blob = await r.blob();
    triggerDownload(blob, filename || 'download');
  } catch (e) { showToast(t('msg_not_in_cache') || 'Download failed'); }
}

function savedMediaOpen(img) {
  var src = img.src || img.getAttribute('src');
  if (!src) return;
  if (typeof showImageLightbox === 'function') {
    showImageLightbox(src, '');
    return;
  }
  var overlay = document.createElement('div');
  overlay.style.cssText = 'position:fixed;inset:0;z-index:999;background:rgba(0,0,0,.85);display:flex;align-items:center;justify-content:center;cursor:zoom-out';
  overlay.onclick = function() { overlay.remove(); };
  var im = document.createElement('img');
  im.src = src;
  im.style.cssText = 'max-width:95vw;max-height:95vh;object-fit:contain;border-radius:4px';
  overlay.appendChild(im);
  document.body.appendChild(overlay);
}

// Expose globals
window.saveMessage = saveMessage;
window.unsaveMessage = unsaveMessage;
window.getAllSaved = getAllSaved;
window.openSavedMessages = openSavedMessages;
window.closeSavedMessages = closeSavedMessages;
window.jumpToSavedOriginal = jumpToSavedOriginal;
window.removeSavedItem = removeSavedItem;
window.contextMenuSave = contextMenuSave;
window.contextMenuUnsave = contextMenuUnsave;
window.filterSavedMessages = filterSavedMessages;
window.sendSavedNote = sendSavedNote;
window.uploadSavedFile = uploadSavedFile;
window.unlockSavedView = unlockSavedView;
window.resetSavedView = resetSavedView;
window.toggleSavedPassphrase = toggleSavedPassphrase;
window.pinSavedItem = pinSavedItem;
window.jumpToTelemirrorPost = jumpToTelemirrorPost;
window.savedInputResize = savedInputResize;
window.savedInputKey = savedInputKey;
window.updateSavedBadge = updateSavedBadge;
window.initSavedMessages = initSavedMessages;
window.downloadSavedMedia = downloadSavedMedia;
window.msgSaveToggle = msgSaveToggle;
window.savedModalToggleEye = savedModalToggleEye;
window.savedModalToggleUnlockEye = savedModalToggleUnlockEye;
window.savedPinCycle = savedPinCycle;
window.savedUnpinCurrent = savedUnpinCurrent;
window.refreshPinnedBanner = refreshPinnedBanner;
window.clearSavedSearch = clearSavedSearch;
window.copySavedText = copySavedText;
window.savedMediaOpen = savedMediaOpen;
window.savedMediaSave = savedMediaSave;

// Auto-init on DOMContentLoaded
if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', function () { initSavedMessages(); });
} else {
  initSavedMessages();
}
