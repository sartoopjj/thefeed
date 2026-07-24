// ===== BACKGROUND IMAGE & PATTERN =====
var BG_PATTERNS = ['pattern-1','pattern-10','pattern-11','pattern-13','pattern-23','pattern-31'];

function _setBg(url) {
  var targets = [
    document.querySelector('.chat-area'),
    document.querySelector('.chat-body-wrap')
  ];
  for (var i = 0; i < targets.length; i++) {
    var el = targets[i];
    if (!el) continue;
    el.style.backgroundImage = url ? 'url("' + url + '")' : '';
    el.style.backgroundSize = url ? 'cover' : '';
    el.style.backgroundPosition = url ? 'center' : '';
    el.style.backgroundRepeat = url ? 'no-repeat' : '';
  }
  document.getElementById('messages').style.background = url ? 'transparent' : '';
}

function _applyPattern(id) {
  // .chat-area is the shared shell column hosting feed + reparented mirror/chat
  // panes; the pattern lives on its ::before. (.tm-body was removed in the
  // redesign; .chat-body-wrap is the standalone chat sheet.)
  var els = [
    document.querySelector('.chat-area'),
    document.querySelector('.chat-body-wrap')
  ];
  for (var i = 0; i < els.length; i++) {
    if (!els[i]) continue;
    if (id) els[i].setAttribute('data-bg-pattern', id);
    else els[i].removeAttribute('data-bg-pattern');
  }
  var msgs = document.getElementById('messages');
  if (msgs) msgs.style.background = id ? 'transparent' : '';
}

function getBgPattern() {
  try { return localStorage.getItem('bgPattern') || ''; } catch (e) { return ''; }
}

function selectBgPattern(id) {
  try { localStorage.setItem('bgPattern', id || ''); } catch (e) {}
  _applyPattern(id);
  _updatePatternGrid();
  if (id) {
    // Clear custom image when selecting a pattern
    try { fetch('/api/bg-image', { method: 'DELETE' }) } catch (e) {}
    _setBg('');
    var inp = document.getElementById('bgImageInput');
    if (inp) inp.value = '';
  }
}

function _updatePatternGrid() {
  var cur = getBgPattern();
  var items = document.querySelectorAll('.bg-pattern-item');
  for (var i = 0; i < items.length; i++) {
    var it = items[i];
    if (it.getAttribute('data-pattern') === cur) {
      it.classList.add('selected');
    } else {
      it.classList.remove('selected');
    }
  }
}

function loadBgImage() {
  var pat = getBgPattern();
  if (pat) { _applyPattern(pat); return; }
  var url = '/api/bg-image?t=' + Date.now();
  fetch(url).then(function (r) {
    if (r.status === 204 || !r.ok) {
      if (!getBgPattern()) selectBgPattern(BG_PATTERNS[0]);
      return;
    }
    _setBg('/api/bg-image?t=' + Date.now());
  }).catch(function () { });
}
async function applyBgImage() {
  var inp = document.getElementById('bgImageInput');
  if (!inp.files || !inp.files[0]) return;
  var file = inp.files[0];
  if (file.size > 10 * 1024 * 1024) { showToast('File too large (max 10MB)'); return }
  try {
    var buf = await new Promise(function (resolve, reject) {
      var fr = new FileReader();
      fr.onload = function () { resolve(fr.result); };
      fr.onerror = function () { reject(fr.error || new Error('read failed')); };
      fr.readAsArrayBuffer(file);
    });
    var r = await fetch('/api/bg-image', {
      method: 'POST',
      headers: { 'Content-Type': file.type || 'application/octet-stream' },
      body: buf
    });
    if (!r.ok) { showToast(await r.text()); return }
    // Clear pattern when uploading custom image
    selectBgPattern('');
    _setBg('/api/bg-image?t=' + Date.now());
    showToast(t('apply'));
  } catch (e) { showToast((e && e.message) || 'failed'); }
}
async function clearBgImage() {
  try { await fetch('/api/bg-image', { method: 'DELETE' }) } catch (e) { }
  _setBg('');
  selectBgPattern('');
  document.getElementById('bgImageInput').value = '';
  showToast(t('clear_bg'));
}

function renderPatternGrid() {
  var grid = document.getElementById('bgPatternGrid');
  if (!grid) return;
  var cur = getBgPattern();
  var html = '<div class="bg-pattern-item bg-pattern-none' + (!cur ? ' selected' : '') +
    '" data-pattern="" onclick="selectBgPattern(\'\')">✕</div>';
  for (var i = 0; i < BG_PATTERNS.length; i++) {
    var p = BG_PATTERNS[i];
    html += '<div class="bg-pattern-item' + (cur === p ? ' selected' : '') +
      '" data-pattern="' + p + '" onclick="selectBgPattern(\'' + p + '\')">' +
      '<img src="/static/img/patterns/' + p + '.svg" alt="' + p + '"></div>';
  }
  grid.innerHTML = html;
}

// ===== EVENTS =====
document.addEventListener('keydown', function (e) {
  if (e.key === 'Enter' && document.activeElement === document.getElementById('sendInput')) { e.preventDefault(); sendMessage() }
  if (e.key === 'Enter' && document.activeElement === document.getElementById('peAddChannelInput')) { e.preventDefault(); addChannelEditor() }
  if (e.key === 'Enter' && document.activeElement === document.getElementById('msgSearchInput')) { e.preventDefault(); msgSearchNext() }
  if (e.key === 'Escape') {
    if (typeof closeSettings === 'function') closeSettings();
    closeProfiles(); closeProfileEditor(); closeScanner(); closeMsgSearch(); closeExportModal(); closeResolversModal();
    if (typeof closeTelemirror === 'function') closeTelemirror();
  }
});
mobileQuery.addEventListener('change', function () {
  var app = document.getElementById('app');
  if (!mobileQuery.matches) {
    app.classList.remove('chat-open');
  } else if (chatIsOpen) {
    app.classList.add('chat-open');
  }
});

// Handle thefeed:// URI hash import
(function () { var h = location.hash; if (h && h.startsWith('#thefeed://')) { document.getElementById('importUriInput').value = decodeURIComponent(h.substring(1)); openProfiles(); doImportUri() } })();

// ===== AUTO-SCROLL DURING TEXT SELECTION =====
(function () {
  var scrollSpeed = 0, scrollFrame = null, messagesEl = null;
  function startAutoScroll() {
    if (scrollFrame) return;
    function step() {
      if (scrollSpeed === 0 || !messagesEl) { scrollFrame = null; return }
      messagesEl.scrollTop += scrollSpeed;
      scrollFrame = requestAnimationFrame(step);
    }
    scrollFrame = requestAnimationFrame(step);
  }
  function stopAutoScroll() {
    scrollSpeed = 0;
    if (scrollFrame) { cancelAnimationFrame(scrollFrame); scrollFrame = null }
  }
  document.addEventListener('DOMContentLoaded', function () {
    messagesEl = document.getElementById('messages');
    if (!messagesEl) return;
    var edgeZone = 40;
    function handleMove(clientY) {
      var sel = window.getSelection();
      if (!sel || sel.isCollapsed) return;
      var rect = messagesEl.getBoundingClientRect();
      if (clientY < rect.top + edgeZone) {
        scrollSpeed = -Math.max(2, (edgeZone - (clientY - rect.top)) / 3);
        startAutoScroll();
      } else if (clientY > rect.bottom - edgeZone) {
        scrollSpeed = Math.max(2, (edgeZone - (rect.bottom - clientY)) / 3);
        startAutoScroll();
      } else { stopAutoScroll() }
    }
    messagesEl.addEventListener('touchmove', function (e) { if (e.touches[0]) handleMove(e.touches[0].clientY) });
    messagesEl.addEventListener('touchend', stopAutoScroll);
    messagesEl.addEventListener('touchcancel', stopAutoScroll);
    messagesEl.addEventListener('mousemove', function (e) { if (e.buttons === 1) handleMove(e.clientY) });
    document.addEventListener('mouseup', stopAutoScroll);
  });
})();

// Close modals (bottom sheets) when clicking outside
(function () {
  var modalMap = {
    settingsModal: function () { closeSettings() },
    profilesModal: function () { closeProfiles() },
    profileEditorModal: function () { closeProfileEditor && closeProfileEditor() },
    shareProfileModal: function () { closeShareModal() },
    exportModal: function () { closeExportModal() },
    resolversModal: function () { closeResolversModal() },
    scannerModal: function () { closeScanner() },
    savedResolversModal: function () { savedResolversSkip && savedResolversSkip() },
  };
  document.addEventListener('click', function (e) {
    var overlay = e.target;
    if (!overlay.classList.contains('modal-overlay') || !overlay.classList.contains('active')) return;
    // Only close if user clicked directly on the overlay backdrop, not the modal content
    if (e.target !== overlay) return;
    var fn = modalMap[overlay.id];
    if (fn) fn();
  });
})();

init();
