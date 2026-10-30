// ===== SCANNER =====
var scanPollTimer = null;
var scanLastResults = []; // cache for selection
var scannerActivePreset = ''; // server-side preset name (e.g. 'default')
var scannerPresetIpCount = 0; // IP count from preset

function countCIDRIPs(text) {
  var lines = text.split('\n');
  var total = 0;
  for (var i = 0; i < lines.length; i++) {
    var line = lines[i].trim();
    if (!line || line[0] === '#') continue;
    var slash = line.indexOf('/');
    if (slash !== -1) {
      var prefix = parseInt(line.substring(slash + 1));
      if (prefix >= 0 && prefix <= 32) total += (1 << (32 - prefix)) >>> 0;
    } else if (/^\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}$/.test(line)) {
      total += 1;
    }
  }
  return total;
}
function updateScanIpCount() {
  var el = document.getElementById('scanIpCount');
  var text = document.getElementById('scanTargets').value;
  var count = countCIDRIPs(text);
  var presetCount = scannerActivePreset ? scannerPresetIpCount : 0;
  var totalCount = count + presetCount;
  if (totalCount > 0) {
    var parts = [];
    if (count > 0) parts.push(count.toLocaleString() + ' ' + t('scanner_from_input'));
    if (presetCount > 0) parts.push(presetCount.toLocaleString() + ' ' + t('scanner_from_preset'));
    el.textContent = '≈ ' + totalCount.toLocaleString() + ' IPs' + (parts.length > 1 ? ' (' + parts.join(' + ') + ')' : '');
    el.style.display = '';
  } else {
    el.style.display = 'none';
  }
}

function openScanner() {
  document.getElementById('scannerModal').classList.add('active');
  populateScanProfileSelect();
  pollScannerOnce();
}
function closeScanner() {
  document.getElementById('scannerModal').classList.remove('active');
  if (scanPollTimer) { clearInterval(scanPollTimer); scanPollTimer = null }
}

function populateScanProfileSelect() {
  var sel = document.getElementById('scanProfile');
  sel.innerHTML = '';
  if (profiles && profiles.profiles) {
    for (var i = 0; i < profiles.profiles.length; i++) {
      var p = profiles.profiles[i];
      var opt = document.createElement('option');
      opt.value = p.id;
      opt.textContent = p.nickname || p.id;
      if (p.id === activeProfileId) opt.selected = true;
      sel.appendChild(opt);
    }
  }
}

async function loadScannerPresets() {
  if (scannerActivePreset === 'default') {
    // Toggle off
    scannerActivePreset = '';
    scannerPresetIpCount = 0;
    renderPresetTag();
    updateScanIpCount();
    return;
  }
  // Turn the preset ON immediately so the button always works — the scan
  // resolves "default" server-side (see handleScannerStart), so it runs even
  // if we never learn the exact IP count. The count is display-only; fetching
  // it must never be able to fail the whole action (a transient WebKit "Load
  // failed" on iOS otherwise left the preset unset and the user stuck).
  scannerActivePreset = 'default';
  scannerPresetIpCount = 0;
  renderPresetTag();
  updateScanIpCount();
  try {
    var r = await fetch('/api/scanner/presets');
    if (!r.ok) return;
    var data = await r.json();
    var presets = data.presets || [];
    if (presets.length > 0) {
      scannerActivePreset = presets[0].name || 'default';
      scannerPresetIpCount = presets[0].count || 0;
      renderPresetTag();
      updateScanIpCount();
    }
  } catch (e) { /* count is display-only; preset is already active */ }
}
function renderPresetTag() {
  var tag = document.getElementById('scanPresetTag');
  if (scannerActivePreset) {
    tag.style.display = '';
    tag.innerHTML = '<img src="/static/lion-sun.svg" alt="" style="height:14px;vertical-align:middle;margin-right:4px"> ' + t('scanner_preset_active');
  } else {
    tag.style.display = 'none';
    tag.textContent = '';
  }
}

async function startScan() {
  var targets = document.getElementById('scanTargets').value.trim().split('\n').filter(function (s) { return s.trim() });
  if (!targets.length && !scannerActivePreset) { showToast(t('scanner_targets')); return }
  // Clear stale results from previous scan.
  scanLastResults = [];
  document.getElementById('scanResultsBody').innerHTML = '';
  document.getElementById('scannerApplySection').style.display = 'none';
  var body = {
    targets: targets,
    preset: scannerActivePreset || undefined,
    profileId: document.getElementById('scanProfile').value,
    rateLimit: parseInt(document.getElementById('scanRateLimit').value) || 50,
    timeout: parseInt(document.getElementById('scanTimeout').value) || 10,
    maxIPs: parseInt(document.getElementById('scanMaxIPs').value) || 0,
    expandSubnet: document.getElementById('scanExpand').checked,
    queryMode: document.getElementById('scanQueryMode').value
  };
  try {
    var r = await fetch('/api/scanner/start', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
    if (!r.ok) { showToast(await r.text() || 'Failed to start'); return }
    showScanRunning();
    startScanPolling();
  } catch (e) { showToast(e.message) }
}

async function stopScan() {
  try { await fetch('/api/scanner/stop', { method: 'POST' }) } catch (e) { }
  if (scanPollTimer) { clearInterval(scanPollTimer); scanPollTimer = null }
  setTimeout(pollScannerOnce, 300);
}

async function toggleScanPause() {
  var btn = document.getElementById('scanPauseBtn');
  var isPaused = btn.dataset.paused === '1';
  try {
    await fetch('/api/scanner/' + (isPaused ? 'resume' : 'pause'), { method: 'POST' });
    pollScannerOnce();
  } catch (e) { }
}

// scanActive drives the "a scan is running" indicators (resolver nav tab +
// the Find-more row in the resolver sidebar) so the user can tell something is
// happening even from another section — like the old spinning magnifier.
window.scanActive = false;
function updateScanIndicators() {
  var nav = document.getElementById('navTabResolver');
  if (nav) nav.classList.toggle('scanning', !!window.scanActive);
  // The Find-more row is re-rendered by renderResolverSidebar (which reads
  // window.scanActive), but toggle the live one too for instant feedback.
  var find = document.querySelector('#resolverSidebar .rs-item-find');
  if (find) find.classList.toggle('rs-scanning', !!window.scanActive);
}

function showScanRunning() {
  document.getElementById('scannerConfig').style.display = 'none';
  document.getElementById('scannerProgressSection').style.display = '';
  document.getElementById('scannerResults').style.display = '';
  document.getElementById('scanStartBtn').style.display = 'none';
  document.getElementById('scanStopBtn').style.display = '';
  document.getElementById('scanPauseBtn').style.display = '';
  document.getElementById('scannerApplySection').style.display = 'none';
  document.getElementById('scannerIconBtn').classList.add('scanning');
  window.scanActive = true; updateScanIndicators();
}

function showScanIdle() {
  document.getElementById('scannerConfig').style.display = '';
  document.getElementById('scannerProgressSection').style.display = 'none';
  document.getElementById('scannerResults').style.display = 'none';
  document.getElementById('scannerApplySection').style.display = 'none';
  document.getElementById('scanStartBtn').style.display = '';
  document.getElementById('scanStartBtn').textContent = t('scanner_start');
  document.getElementById('scanStopBtn').style.display = 'none';
  document.getElementById('scanPauseBtn').style.display = 'none';
  document.getElementById('scannerIconBtn').classList.remove('scanning');
  window.scanActive = false; updateScanIndicators();
}

function resetScannerUI() {
  showScanIdle();
  document.getElementById('scannerAboutFull').style.display = 'none';
  document.getElementById('scannerAboutShort').querySelector('a').style.display = '';
}

function showScanDone(progress) {
  document.getElementById('scannerConfig').style.display = 'none';
  document.getElementById('scannerProgressSection').style.display = '';
  document.getElementById('scannerResults').style.display = '';
  document.getElementById('scanStartBtn').style.display = 'none';
  document.getElementById('scanStopBtn').style.display = 'none';
  document.getElementById('scanPauseBtn').style.display = 'none';
  document.getElementById('scannerIconBtn').classList.remove('scanning');
  window.scanActive = false; updateScanIndicators();
  // Always show the apply section (it has the New Scan button).
  document.getElementById('scannerApplySection').style.display = '';
  if (progress && progress.results && progress.results.length > 0) {
    updateScanSelectedCount();
  }
  if (scanPollTimer) { clearInterval(scanPollTimer); scanPollTimer = null }
}

function getSelectedScanIPs() {
  var cbs = document.querySelectorAll('.scan-select-cb:checked');
  var ips = [];
  for (var i = 0; i < cbs.length; i++) ips.push(cbs[i].dataset.ip);
  return ips;
}

function updateScanSelectedCount() {
  var n = getSelectedScanIPs().length;
  var label = '(' + n + ')';
  var el1 = document.getElementById('scanAppendCount');
  var el3 = document.getElementById('scanCopyCount');
  if (el1) el1.textContent = label;
  if (el3) el3.textContent = label;
}

function toggleScanSelectAll(checked) {
  var cbs = document.querySelectorAll('.scan-select-cb');
  for (var i = 0; i < cbs.length; i++) cbs[i].checked = checked;
  updateScanSelectedCount();
}

function renderScanProgress(p) {
  var pct = p.total > 0 ? Math.round(p.scanned / p.total * 100) : 0;
  document.getElementById('scanProgressFill').style.width = pct + '%';
  document.getElementById('scanProgressText').textContent = p.scanned + ' / ' + p.total;
  document.getElementById('scanFoundText').textContent = p.found || 0;

  var stateKey = 'scanner_' + p.state;
  document.getElementById('scanStatusLabel').textContent = t(stateKey);

  var pauseBtn = document.getElementById('scanPauseBtn');
  if (p.state === 'paused') {
    pauseBtn.textContent = t('scanner_resume');
    pauseBtn.dataset.paused = '1';
  } else {
    pauseBtn.textContent = t('scanner_pause');
    pauseBtn.dataset.paused = '0';
  }

  // Render results table
  var results = p.results || [];
  scanLastResults = results;
  var body = document.getElementById('scanResultsBody');
  body.innerHTML = '';
  for (var i = 0; i < results.length; i++) {
    var r = results[i];
    var tr = document.createElement('tr');
    tr.style.borderTop = '1px solid var(--border)';
    tr.innerHTML = '<td style="padding:8px"><input type="checkbox" class="scan-select-cb" data-ip="' + escAttr(r.ip) + '" checked onchange="updateScanSelectedCount()"></td>' +
      '<td style="padding:8px;font-family:monospace;font-size:13px">' + esc(r.ip) + '</td>' +
      '<td style="padding:8px;text-align:right;font-size:13px;color:var(--text-dim)">' + (r.latencyMs != null ? Math.round(r.latencyMs) + 'ms' : '-') + '</td>' +
      '<td style="padding:4px 8px"><button class="btn btn-flat" style="font-size:12px;padding:4px 8px;min-width:0" onclick="navigator.clipboard.writeText(\'' + jsStr(r.ip) + '\');showToast(t(\'copied\'))">' + icon('copy') + '</button></td>';
    body.appendChild(tr);
  }

  if (p.state === 'done') {
    showScanDone(p);
  } else if (p.state === 'running' || p.state === 'paused') {
    showScanRunning();
    if (p.state === 'paused') {
      document.getElementById('scanPauseBtn').textContent = t('scanner_resume');
      document.getElementById('scanPauseBtn').dataset.paused = '1';
    }
  }
}

function startScanPolling() {
  if (scanPollTimer) clearInterval(scanPollTimer);
  scanPollTimer = setInterval(pollScannerOnce, 1500);
}

async function pollScannerOnce() {
  try {
    var r = await fetch('/api/scanner/progress');
    if (!r.ok) return;
    var p = await r.json();
    if (p.state === 'running' || p.state === 'paused') {
      renderScanProgress(p);
      if (!scanPollTimer) startScanPolling();
    } else if (p.state === 'done') {
      renderScanProgress(p);
    } else {
      // idle
      if (p.results && p.results.length > 0) {
        renderScanProgress(p);
        showScanDone(p);
      } else {
        showScanIdle();
      }
    }
  } catch (e) { }
}

async function applyScanResults(mode) {
  var ips = getSelectedScanIPs();
  if (!ips.length) { showToast(t('scanner_no_results')); return }
  try {
    var r = await fetch('/api/scanner/apply', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ resolvers: ips, mode: mode, profileId: document.getElementById('scanProfile').value })
    });
    if (!r.ok) { showToast(await r.text() || 'Failed'); return }
    showToast(t('scanner_applied'));
    await loadProfiles();
  } catch (e) { showToast(e.message) }
}

function copySelectedScanResults() {
  var ips = getSelectedScanIPs();
  if (!ips.length) { showToast(t('scanner_no_results')); return }
  navigator.clipboard.writeText(ips.join('\n')).then(function () { showToast(t('copied')) });
}

function copyAllScanResults() {
  var ips = scanLastResults.map(function (r) { return r.ip });
  if (!ips.length) { showToast(t('scanner_no_results')); return }
  navigator.clipboard.writeText(ips.join('\n')).then(function () { showToast(t('copied')) });
}

// ===== MESSAGE SEARCH =====
var msgSearchMatches = [], msgSearchIdx = -1;
// Normalize Arabic/Persian: map ي→ی, ك→ک, ة→ه, etc.
function normalizeArabicPersian(s) {
  return s
    .replace(/\u064A/g, '\u06CC')   // Arabic Ya -> Persian Ya
    .replace(/\u0643/g, '\u06A9')   // Arabic Kaf -> Persian Kaf
    .replace(/\u0629/g, '\u0647')   // Arabic Ta Marbuta -> He
    .replace(/\u0649/g, '\u06CC')   // Arabic Alef Maksura -> Persian Ya
    .replace(/\u06C0/g, '\u0647')   // He with Hamza above -> He
    .replace(/[\u0623\u0625\u0622]/g, '\u0627') // Alef variants -> plain Alef
    .replace(/\u0624/g, '\u0648')   // Waw with Hamza -> plain Waw
    .replace(/\u0626/g, '\u06CC')   // Ya with Hamza -> Ya
    .replace(/\u0621/g, '')          // standalone Hamza -> remove
    .replace(/[\u064B-\u065F\u0610-\u061A\u0670]/g, '') // strip tashkil/diacritics
    .replace(/[\u200C\u200D\u200E\u200F]/g, '') // strip ZWNJ, ZWJ, directional marks
}
var _kebabParent = null;
function toggleKebabMenu(e) {
  e.stopPropagation();
  var menu = document.getElementById('headerKebabMenu');
  if (!menu) return;
  if (menu.classList.contains('open')) { closeKebabMenu(); return; }
  var btn = e.currentTarget;
  var r = btn.getBoundingClientRect();
  _kebabParent = menu.parentNode;
  document.body.appendChild(menu);
  menu.style.top = (r.bottom + 4) + 'px';
  menu.style.right = (window.innerWidth - r.right) + 'px';
  menu.classList.add('open');
}
function closeKebabMenu() {
  var m = document.getElementById('headerKebabMenu');
  if (!m) return;
  m.classList.remove('open');
  if (_kebabParent && m.parentNode === document.body) {
    _kebabParent.appendChild(m);
    _kebabParent = null;
  }
}
document.addEventListener('click', closeKebabMenu);

function toggleMsgSearch() {
  var bar = document.getElementById('msgSearchBar');
  if (bar.classList.contains('active')) { closeMsgSearch(); return }
  bar.classList.add('active');
  var inp = document.getElementById('msgSearchInput');
  inp.value = '';
  inp.focus();
  msgSearchMatches = []; msgSearchIdx = -1;
  document.getElementById('msgSearchCount').textContent = '';
}
function closeMsgSearch() {
  document.getElementById('msgSearchBar').classList.remove('active');
  document.getElementById('msgSearchInput').value = '';
  // Remove highlights
  document.querySelectorAll('.msg .search-highlight').forEach(function (el) {
    el.outerHTML = el.textContent;
  });
  msgSearchMatches = []; msgSearchIdx = -1;
  document.getElementById('msgSearchCount').textContent = '';
}
function doMsgSearch() {
  var q = normalizeArabicPersian(document.getElementById('msgSearchInput').value.trim().toLowerCase());
  // Remove old highlights
  document.querySelectorAll('.msg .search-highlight').forEach(function (el) {
    el.outerHTML = el.textContent;
  });
  msgSearchMatches = []; msgSearchIdx = -1;
  if (!q) { document.getElementById('msgSearchCount').textContent = ''; return }
  var msgs = document.querySelectorAll('.msg');
  msgs.forEach(function (msgEl) {
    highlightTextNodes(msgEl, q);
  });
  msgSearchMatches = Array.from(document.querySelectorAll('.msg .search-highlight'));
  if (msgSearchMatches.length > 0) {
    // Start from the last match (bottom of chat, most recent)
    msgSearchIdx = msgSearchMatches.length - 1;
    scrollToSearchMatch();
    document.getElementById('msgSearchCount').textContent = (msgSearchIdx + 1) + '/' + msgSearchMatches.length;
  } else {
    document.getElementById('msgSearchCount').textContent = t('search_no_results');
  }
}
function highlightTextNodes(el, q) {
  // Skip metadata and buttons
  if (el.classList && (el.classList.contains('msg-meta') || el.classList.contains('media-tag'))) return;
  var walker = document.createTreeWalker(el, NodeFilter.SHOW_TEXT, null, false);
  var nodes = [];
  while (walker.nextNode()) {
    // Skip nodes inside msg-meta
    var p = walker.currentNode.parentNode;
    var skip = false;
    while (p && p !== el) { if (p.classList && (p.classList.contains('msg-meta') || p.classList.contains('media-tag'))) { skip = true; break } p = p.parentNode; }
    if (!skip) nodes.push(walker.currentNode);
  }
  for (var i = nodes.length - 1; i >= 0; i--) {
    var node = nodes[i];
    var src = node.textContent;
    var normalized = normalizeArabicPersian(src.toLowerCase());
    var idx = normalized.indexOf(q);
    if (idx === -1) continue;
    // Map normalized indices back to original string positions
    var frag = document.createDocumentFragment();
    var pos = 0;
    while (idx !== -1) {
      // Find the original char positions that correspond to normalized idx..idx+q.length
      var origStart = mapNormIdx(src, idx);
      var origEnd = mapNormIdx(src, idx + q.length);
      if (origStart > pos) frag.appendChild(document.createTextNode(src.substring(pos, origStart)));
      var span = document.createElement('span');
      span.className = 'search-highlight';
      span.textContent = src.substring(origStart, origEnd);
      frag.appendChild(span);
      pos = origEnd;
      idx = normalized.indexOf(q, idx + q.length);
    }
    if (pos < src.length) frag.appendChild(document.createTextNode(src.substring(pos)));
    node.parentNode.replaceChild(frag, node);
  }
}
// Map a position in normalized text to position in original text
function mapNormIdx(original, normPos) {
  var ni = 0;
  for (var oi = 0; oi <= original.length; oi++) {
    if (ni >= normPos) return oi;
    if (oi < original.length) {
      var ch = original[oi];
      var norm = normalizeArabicPersian(ch.toLowerCase());
      ni += norm.length;
    }
  }
  return original.length;
}
function scrollToSearchMatch() {
  if (msgSearchMatches.length === 0) return;
  msgSearchMatches.forEach(function (el) { el.classList.remove('current') });
  var cur = msgSearchMatches[msgSearchIdx];
  if (cur) { cur.classList.add('current'); cur.scrollIntoView({ behavior: 'smooth', block: 'center' }); }
  document.getElementById('msgSearchCount').textContent = (msgSearchIdx + 1) + '/' + msgSearchMatches.length;
}
function msgSearchNext() {
  if (msgSearchMatches.length === 0) return;
  msgSearchIdx = (msgSearchIdx + 1) % msgSearchMatches.length;
  scrollToSearchMatch();
}
function msgSearchPrev() {
  if (msgSearchMatches.length === 0) return;
  msgSearchIdx = (msgSearchIdx - 1 + msgSearchMatches.length) % msgSearchMatches.length;
  scrollToSearchMatch();
}

// ===== EXPORT MESSAGES =====
function openExportModal() {
  if (!currentMsgTexts.length) { showToast(t('export_no_messages')); return }
  document.getElementById('exportCount').value = Math.min(10, currentMsgTexts.length);
  document.getElementById('exportCount').max = currentMsgTexts.length;
  document.getElementById('exportModal').classList.add('active');
}
function closeExportModal() { document.getElementById('exportModal').classList.remove('active') }
function doExport() {
  var n = parseInt(document.getElementById('exportCount').value) || 10;
  if (n < 1) n = 1;
  if (n > currentMsgTexts.length) n = currentMsgTexts.length;
  var chName = selectedChannel > 0 && channels[selectedChannel - 1] ? (channels[selectedChannel - 1].Name || channels[selectedChannel - 1].name || 'Channel') : 'Channel';
  // Take last N messages (most recent)
  var start = currentMsgTexts.length - n;
  var lines = [];
  lines.push('=== ' + chName + ' ===');
  for (var i = start; i < currentMsgTexts.length; i++) {
    lines.push('');
    lines.push(currentMsgTexts[i]);
  }
  navigator.clipboard.writeText(lines.join('\n')).then(function () {
    showToast(t('export_copied'));
    closeExportModal();
  }).catch(function () { showToast('Copy failed') });
}

