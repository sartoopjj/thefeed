// ===== NAMED RESOLVER LISTS =====
// The user keeps multiple named resolver lists (e.g. "Home",
// "Office", "Mobile") and switches between them with one tap. UI
// is a single tab strip at the top of the Resolver Bank modal:
// each list is a tab, "+" creates a new list, "Bank" is the master
// pool. The selected list-tab shows ⋮ that opens an inline
// rename/delete menu — no extra modals.

var rlState = { lists: [], selected: '' };

async function loadResolverLists() {
  try {
    var r = await fetch('/api/resolvers/lists');
    if (!r.ok) { renderResolverTabs(); return; }
    rlState = await r.json();
    renderResolverTabs();
  } catch (e) { renderResolverTabs(); }
}

function renderResolverTabs() {
  var tabs = document.getElementById('resolverTabs');
  if (!tabs) return;
  hideResolverTabMenu();
  tabs.innerHTML = '';
  var lists = (rlState && rlState.lists) || [];
  lists.forEach(function (li) {
    var tab = document.createElement('button');
    tab.type = 'button';
    tab.className = 'rtab' + (li.selected && currentResolverTab === 'active' ? ' rtab-active' : '');
    tab.setAttribute('role', 'tab');
    tab.title = li.name;
    var label = document.createElement('span');
    label.textContent = li.name;
    var count = document.createElement('span');
    count.className = 'rtab-count';
    count.textContent = li.count || 0;
    tab.appendChild(label);
    tab.appendChild(count);
    if (li.selected && currentResolverTab === 'active') {
      var more = document.createElement('span');
      more.className = 'rtab-menu-btn';
      more.setAttribute('aria-label', t('manage_lists') || 'Manage');
      more.innerHTML = window.icon('more'); // vertical ellipsis
      more.onclick = function (e) {
        e.stopPropagation();
        toggleResolverTabMenu(this);
      };
      tab.appendChild(more);
    }
    tab.onclick = function () {
      hideResolverTabMenu();
      if (li.selected && currentResolverTab === 'active') return;
      if (!li.selected) {
        selectResolverList(li.name);
      } else {
        // Already selected but on the Bank view — hop back.
        switchResolverTab('active');
      }
    };
    tabs.appendChild(tab);
  });
  // "+" — create new list. Always present so an empty install
  // still has an obvious onboarding affordance.
  var add = document.createElement('button');
  add.type = 'button';
  add.className = 'rtab rtab-add';
  add.title = t('save_current_list') || 'Save current as new list';
  add.setAttribute('aria-label', t('save_current_list') || 'New list');
  add.innerHTML = window.icon('add');
  add.onclick = function () {
    hideResolverTabMenu();
    saveCurrentResolverListPrompt();
  };
  tabs.appendChild(add);
  renderBankPill();
  // Mirror list changes into the reparented Resolver sidebar (rename/delete/
  // select all funnel through here).
  if (typeof renderResolverSidebar === 'function') renderResolverSidebar();
}

function renderBankPill() {
  // The Bank chip sits on the modal header row (right edge in
  // LTR / left in RTL) — different shape and location from the
  // list-tabs because it points at the master pool, not a list.
  var slot = document.getElementById('resolverBankSlot');
  if (!slot) return;
  slot.innerHTML = '';
  var bank = document.createElement('button');
  bank.type = 'button';
  bank.id = 'resolverBankTab';
  bank.className = 'rtab-bank' + (currentResolverTab === 'bank' ? ' rtab-active' : '');
  bank.setAttribute('role', 'tab');
  var icon = document.createElement('span');
  icon.className = 'rtab-bank-icon';
  icon.setAttribute('aria-hidden', 'true');
  icon.innerHTML = window.icon('bank'); // bank pouch
  var bankLabel = document.createElement('span');
  bankLabel.textContent = t('resolver_tab_bank') || 'Bank';
  var bankCount = document.createElement('span');
  bankCount.className = 'rtab-count';
  bankCount.id = 'resolverBankCount';
  bank.appendChild(icon);
  bank.appendChild(bankLabel);
  bank.appendChild(bankCount);
  bank.onclick = function () {
    hideResolverTabMenu();
    switchResolverTab('bank');
  };
  slot.appendChild(bank);
}

async function selectResolverList(name) {
  if (!name) return;
  // If the picked list is empty, ask the user before kicking off
  // an automatic bank-scan. Two flavors:
  //  - bank has resolvers → confirm "Scan now?"
  //  - bank also empty → info dialog pointing at "add manually" /
  //    "scanner" with no scan triggered
  var lists = (rlState && rlState.lists) || [];
  var picked = lists.find(function (l) { return l.name === name; });
  var noScan = false;
  if (picked && (picked.count || 0) === 0) {
    // Read the REAL bank size (the old #resolverBankCount element lives in the
    // now-hidden modal and is often stale/empty → wrong "bank is empty" copy).
    var bankCount = 0;
    try { var br = await fetch('/api/resolvers/bank'); if (br.ok) { var bd = await br.json(); bankCount = bd.count || 0; } } catch (e) { }
    if (bankCount > 0) {
      var ok = await showConfirmDialog(
        (t('empty_list_scan_confirm') || 'List "{n}" is empty. Scan the bank to find working resolvers?').replace('{n}', name),
        t('scan') || 'Scan',
        t('cancel') || 'Cancel');
      if (!ok) noScan = true;
    } else {
      // Bank also empty — show guidance and switch without scan.
      await showInfoDialog(
        t('empty_list_no_bank') || 'This list and the bank are both empty. Open the Bank tab to add resolvers manually, or run the resolver scanner.',
        t('ok') || 'OK');
      noScan = true;
    }
  }
  try {
    var r = await fetch('/api/resolvers/lists/select', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: name, noScan: noScan })
    });
    if (!r.ok) {
      showToast(await r.text() || t('resolver_list_switch_failed') || 'Could not switch list');
      loadResolverLists();
      return;
    }
    rlState = await r.json();
    switchResolverTab('active'); // also re-renders tabs
    try { if (typeof _fetchActiveBoard === 'function') _fetchActiveBoard(); } catch (e) { }
    try { if (typeof refreshResolversBadge === 'function') refreshResolversBadge(); } catch (e) { }
    try { await doRefresh(false); } catch (e) { }
  } catch (e) {
    showToast(e.message || 'switch failed');
  }
}

// ----- Inline tab menu (rename / delete on the selected list) -----

function toggleResolverTabMenu(anchor) {
  var menu = document.getElementById('resolverTabMenu');
  if (!menu) return;
  if (!menu.hidden) { hideResolverTabMenu(); return; }
  // Defensive: drop any leftover outside-click listener from a
  // previous open before installing a new one.
  document.removeEventListener('click', _resolverTabMenuOutside);
  // Position below the anchor. On mobile the menu lives inside a
  // transformed ancestor which makes position:fixed relative to that
  // ancestor — compensate by subtracting the ancestor's offset.
  var rect = anchor.getBoundingClientRect();
  var parentRect = menu.offsetParent ? menu.offsetParent.getBoundingClientRect() : { top: 0, left: 0, right: window.innerWidth };
  menu.style.top = (rect.bottom - parentRect.top + 4) + 'px';
  menu.style.bottom = 'auto';
  var isRtl = document.documentElement.dir === 'rtl';
  menu.hidden = false;
  var menuW = menu.offsetWidth || 140;
  var parentW = parentRect.right - parentRect.left;
  if (isRtl) {
    var rightOffset = parentRect.right - rect.right;
    rightOffset = Math.max(8, Math.min(rightOffset, parentW - menuW - 8));
    menu.style.right = rightOffset + 'px';
    menu.style.left = 'auto';
  } else {
    var leftOffset = rect.left - parentRect.left;
    leftOffset = Math.max(8, Math.min(leftOffset, parentW - menuW - 8));
    menu.style.left = leftOffset + 'px';
    menu.style.right = 'auto';
  }
  // Close on next outside click. Defer one tick so the click that
  // opened the menu doesn't immediately close it again.
  setTimeout(function () {
    document.addEventListener('click', _resolverTabMenuOutside, { once: true });
  }, 0);
}

function hideResolverTabMenu() {
  var menu = document.getElementById('resolverTabMenu');
  if (menu) menu.hidden = true;
  document.removeEventListener('click', _resolverTabMenuOutside);
}

function _resolverTabMenuOutside(e) {
  var menu = document.getElementById('resolverTabMenu');
  if (!menu || menu.hidden) return;
  if (menu.contains(e.target)) return;
  hideResolverTabMenu();
}

// ----- Bank → list picker -----
// The "+" button on each Bank row opens this popover so the user
// can pick a target named list. Click a list → POST add → toast.
var bankAddPickerAddr = '';

function openBankAddPicker(anchor, addr) {
  // Strip any pending outside-click listener from a previous
  // open. Without this, clicking + on a second row while the
  // menu is still open lets the stale listener fire on the new
  // click and snap-close the freshly-opened menu — looks like
  // "+ doesn't work" from the user's seat.
  hideBankAddMenu();
  bankAddPickerAddr = addr;
  var menu = document.getElementById('bankAddMenu');
  if (!menu) return;
  var lists = (rlState && rlState.lists) || [];
  menu.innerHTML = '';
  if (!lists.length) {
    var empty = document.createElement('div');
    empty.style.padding = '8px 12px';
    empty.style.color = 'var(--text-dim)';
    empty.style.fontSize = '12px';
    empty.textContent = t('no_lists') || 'No saved lists';
    menu.appendChild(empty);
  } else {
    lists.forEach(function (li) {
      var b = document.createElement('button');
      b.type = 'button';
      b.onclick = function () { addBankAddrToList(li.name); };
      var name = document.createElement('span');
      name.textContent = li.name;
      var count = document.createElement('span');
      count.style.color = 'var(--text-dim)';
      count.style.fontSize = '11px';
      count.style.marginInlineStart = '6px';
      count.textContent = '(' + (li.count || 0) + ')';
      b.appendChild(name);
      b.appendChild(count);
      menu.appendChild(b);
    });
  }
  // Position below the anchor. On mobile the menu lives inside a
  // transformed ancestor (.chat-area has transform: translateX) which
  // makes position:fixed relative to that ancestor, not the viewport.
  // Compensate by subtracting the ancestor's viewport offset.
  var rect = anchor.getBoundingClientRect();
  var parentRect = menu.offsetParent ? menu.offsetParent.getBoundingClientRect() : { top: 0, left: 0, right: window.innerWidth };
  menu.style.top = (rect.bottom - parentRect.top + 4) + 'px';
  menu.style.bottom = 'auto';
  menu.hidden = false;
  var menuW = menu.offsetWidth || 160;
  var isRtl = document.documentElement.dir === 'rtl';
  if (isRtl) {
    var rightOffset = parentRect.right - rect.right;
    rightOffset = Math.max(8, Math.min(rightOffset, (parentRect.right - parentRect.left) - menuW - 8));
    menu.style.right = rightOffset + 'px';
    menu.style.left = 'auto';
  } else {
    var leftOffset = rect.left - parentRect.left;
    leftOffset = Math.max(8, Math.min(leftOffset, (parentRect.right - parentRect.left) - menuW - 8));
    menu.style.left = leftOffset + 'px';
    menu.style.right = 'auto';
  }
  // One-shot outside click closes. Defer one tick so the click
  // that opened this menu doesn't immediately close it.
  setTimeout(function () {
    document.addEventListener('click', _bankAddMenuOutside, { once: true });
  }, 0);
}

function hideBankAddMenu() {
  var menu = document.getElementById('bankAddMenu');
  if (menu) menu.hidden = true;
  document.removeEventListener('click', _bankAddMenuOutside);
}

function _bankAddMenuOutside(e) {
  var menu = document.getElementById('bankAddMenu');
  if (!menu || menu.hidden) return;
  if (menu.contains(e.target)) return;
  hideBankAddMenu();
}

async function addBankAddrToList(listName) {
  var addr = bankAddPickerAddr;
  hideBankAddMenu();
  if (!addr || !listName) return;
  try {
    var r = await fetch('/api/resolvers/lists/add', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: listName, resolvers: [addr] })
    });
    if (!r.ok) {
      showToast(await r.text() || t('add_failed') || 'Add failed');
      return;
    }
    var data = await r.json();
    if (data.added > 0) {
      showToast((t('added_to_list') || 'Added to "{n}"').replace('{n}', listName));
    } else {
      showToast((t('already_in_list') || 'Already in "{n}"').replace('{n}', listName));
    }
    // Refresh the list counts/previews immediately (don't wait for the 3s poll).
    try { await loadResolverLists(); } catch (e2) { }
  } catch (e) { showToast(e.message || 'add failed'); }
}

async function renameCurrentResolverList() {
  hideResolverTabMenu();
  var current = (rlState.lists || []).find(function (li) { return li.selected; });
  if (!current) return;
  var newName = await showInputDialog({
    title: t('rename') || 'Rename',
    message: (t('resolver_list_rename_prompt') || 'New name for "{n}"').replace('{n}', current.name),
    value: current.name,
    maxLength: 32,
  });
  if (!newName || newName === current.name) return;
  try {
    var r = await fetch('/api/resolvers/lists/rename', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: current.name, newName: newName })
    });
    if (!r.ok) {
      showToast(await r.text() || t('rename_failed') || 'Rename failed');
      return;
    }
    rlState = await r.json();
    renderResolverTabs();
  } catch (e) { showToast(e.message || 'rename failed'); }
}

async function deleteCurrentResolverList() {
  hideResolverTabMenu();
  var current = (rlState.lists || []).find(function (li) { return li.selected; });
  if (!current) return;
  var ok = await showConfirmDialog(
    (t('resolver_list_delete_confirm') || 'Delete "{n}"?').replace('{n}', current.name),
    t('delete') || 'Delete',
    t('cancel') || 'Cancel');
  if (!ok) return;
  try {
    var r = await fetch('/api/resolvers/lists', {
      method: 'DELETE', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: current.name })
    });
    if (!r.ok) {
      showToast(await r.text() || t('delete_failed') || 'Delete failed');
      return;
    }
    rlState = await r.json();
    renderResolverTabs();
    try { if (typeof _fetchActiveBoard === 'function') _fetchActiveBoard(); } catch (e) { }
  } catch (e) { showToast(e.message || 'delete failed'); }
}

async function saveCurrentResolverListPrompt() {
  var name = await showInputDialog({
    title: t('save_current_list') || 'New list',
    message: t('resolver_list_name_prompt') || 'Name this list:',
    placeholder: t('resolver_list_name_ph') || 'List name',
    maxLength: 32,
  });
  if (!name) return;
  try {
    var r = await fetch('/api/resolvers/lists/save', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: name, mode: 'create' })
    });
    if (r.status === 409) {
      var ok = await showConfirmDialog(
        (t('resolver_list_overwrite_confirm') || 'A list named "{n}" already exists. Overwrite?').replace('{n}', name),
        t('overwrite') || 'Overwrite',
        t('cancel') || 'Cancel');
      if (!ok) return;
      r = await fetch('/api/resolvers/lists/save', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: name, mode: 'overwrite' })
      });
    }
    if (!r.ok) {
      showToast(await r.text() || t('save_failed') || 'Save failed');
      return;
    }
    rlState = await r.json();
    renderResolverTabs();
    showToast((t('resolver_list_saved') || 'Saved "{n}"').replace('{n}', name));
  } catch (e) { showToast(e.message || 'save failed'); }
}

// ===== SAVED RESOLVERS PROMPT =====
function checkAndShowSavedResolversPrompt(status) {
  // Suppressed by the user via "Don't show again" → settings can
  // re-enable it.
  if (localStorage.getItem('thefeed_scan_prompt_off') === '1') return;
  // Per-tab debounce so we don't re-pop on every soft reload
  // within the same browsing session.
  if (sessionStorage.getItem('thefeed_scan_prompt_shown')) return;
  // Only prompt when there are SAVED resolvers to offer as a fast path
  // ("Use Now" vs "Scan Again"). A fresh install / wiped state has nothing to
  // reuse and auto-scans on its own, so a lone "Scan Now?" prompt here is both
  // redundant and reads as broken (a single, orphaned action button).
  var hasSaved = !!(status.lastScan && status.lastScan.count);
  if (!hasSaved) return;
  var ls = status.lastScan;
  var ageSec = Math.floor(Date.now() / 1000) - ls.scannedAt;
  var ageStr;
  if (ageSec < 3600) ageStr = Math.max(1, Math.round(ageSec / 60)) + ' ' + t('minutes_ago');
  else ageStr = Math.round(ageSec / 3600) + ' ' + t('hours_ago');
  document.getElementById('savedResolversMsg').textContent =
    t('saved_resolvers_msg').replace('{n}', ls.count).replace('{t}', ageStr);
  // Both actions present: "Scan Again" (outline) + "Use Now" (primary).
  var useBtn = document.getElementById('savedResolversUseBtn');
  if (useBtn) useBtn.style.display = '';
  var rescanBtn = document.getElementById('savedResolversRescanBtn');
  if (rescanBtn) {
    rescanBtn.classList.add('btn-outline');
    rescanBtn.classList.remove('btn-primary');
    rescanBtn.textContent = t('saved_resolvers_rescan');
  }
  var srm = document.getElementById('savedResolversModal');
  // Lift above the floating nav (z-index 9300); the base .modal-overlay (100)
  // would otherwise render this startup prompt under the nav bar. Matches the
  // askRescan overlay lift.
  srm.style.zIndex = '9600';
  srm.classList.add('active');
}
function savedResolversSkip() {
  // "Later" — just close, server already applied saved resolvers and refresh is underway
  document.getElementById('savedResolversModal').classList.remove('active');
  sessionStorage.setItem('thefeed_scan_prompt_shown', '1');
}
function savedResolversNever() {
  // "Don't show again" — persist across sessions. localStorage
  // for fast access, /api/settings for survival across Android
  // restarts (the WebView origin port changes each launch, so
  // localStorage alone gets reset).
  localStorage.setItem('thefeed_scan_prompt_off', '1');
  try {
    fetch('/api/settings', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ scanPromptOff: true })
    });
  } catch (e) { }
  savedResolversSkip();
  showToast(t('startup_scan_off') || 'Startup scan prompt disabled');
}
function savedResolversUseNow() {
  // Server already applied saved resolvers at startup; just close the popup
  savedResolversSkip();
  showToast(t('saved_resolvers_applied'));
}
async function savedResolversRescan() {
  savedResolversSkip();
  try { await fetch('/api/rescan', { method: 'POST' }) } catch (e) { }
  showToast(t('rescan_started'));
}

