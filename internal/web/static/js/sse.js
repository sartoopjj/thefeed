// ===== SSE =====
function connectSSE() {
  if (eventSource) eventSource.close();
  // Clear stale progress items from a previous connection
  document.getElementById('progressPanel').innerHTML = '';
  resolverScanHint = '';
  eventSource = new EventSource('/api/events');
  eventSource.addEventListener('log', function (e) { addLogLine(JSON.parse(e.data)) });
  // Server-pushed toast: data is an i18n key, localised on the client.
  eventSource.addEventListener('toast', function (e) {
    var key; try { key = JSON.parse(e.data) } catch (x) { key = e.data }
    if (key && typeof showToast === 'function') showToast(t(key));
  });
  // Chat messenger events (inbox arrivals, send/receive progress).
  eventSource.addEventListener('chat', function (e) {
    var data; try { data = JSON.parse(e.data) } catch (x) { return }
    if (typeof chatOnSSE === 'function') chatOnSSE(data);
  });
  eventSource.addEventListener('update', async function (e) {
    refreshResolversBadge();
    var data; try { data = JSON.parse(e.data) } catch (x) { data = e.data }
    // Server signals that the saved resolver lists changed (auto-
    // populated from a bank scan, or user-triggered rescan
    // overwrote them). Refresh the tab strip so the count
    // badge stops showing 0 while resolvers are actually live.
    // Server finished a background telemirror refresh for this
    // channel — let telemirror.js re-fetch if it's the active one.
    if (typeof data === 'string' && data.indexOf('telemirror:') === 0) {
      if (typeof window.tmOnServerUpdate === 'function') {
        try { window.tmOnServerUpdate(data.slice('telemirror:'.length)); } catch (e2) { }
      }
      return;
    }
    // The Telegram blocklist changed (blocked or unblocked on another device).
    if (data === 'tel-blocks') {
      if (typeof window.telLoadBlocked === 'function') window.telLoadBlocked();
      return;
    }
    if (data === 'resolver-lists') {
      // Resolver UI is a reparented section now (not the old modal). Refresh the
      // lists/sidebar + the visible board when the section is open.
      if (document.documentElement.classList.contains('resolver-section')) {
        try { loadResolverLists(); } catch (e2) { } // → renderResolverTabs → renderResolverSidebar
        try {
          if (typeof resolverView !== 'undefined' && resolverView === 'bank') { if (typeof _fetchBankBoard === 'function') _fetchBankBoard(); }
          else if (typeof _fetchActiveBoard === 'function') _fetchActiveBoard();
        } catch (e2) { }
      }
      return;
    }
    // Throttle (not debounce): first event fires immediately,
    // subsequent events at most every 600 ms during a burst.
    if (data === 'profile-pics') {
      var THROTTLE_MS = 600;
      var now = Date.now();
      var since = now - profilePicsLastReloadAt;
      if (since >= THROTTLE_MS) {
        profilePicsLastReloadAt = now;
        loadProfilePicState().catch(function () { });
      } else if (!profilePicsReloadTimer) {
        // Trailing call for the last avatar in the burst.
        profilePicsReloadTimer = setTimeout(function () {
          profilePicsReloadTimer = null;
          profilePicsLastReloadAt = Date.now();
          loadProfilePicState().catch(function () { });
        }, THROTTLE_MS - since);
      }
      return;
    }
    // Another tab/device switched the active profile.
    if (data === 'profiles') {
      var prevActive = activeProfileId;
      await loadProfiles();
      await loadFontSize(); // Refetch settings to update pinnedChannels for the new profile
      if (activeProfileId !== prevActive) {
        // The active profile changed under us — drop stale local state
        // so the user doesn't see the old profile's channel/messages.
        selectedChannel = 0;
        channels = [];
        document.getElementById('chatName').textContent = 'thefeed';
        document.getElementById('chatSub').textContent = '';
        if (typeof setChatHeaderAvatar === 'function') setChatHeaderAvatar(null);
        if (typeof updateFeedHeaderChrome === 'function') updateFeedHeaderChrome();
        openSidebar();
      }
      if (document.getElementById('profilesModal').classList.contains('active')) {
        renderProfilesModal();
      }
      return;
    }
    var wasEmpty = channels.length === 0;
    var snapChannel = selectedChannel;
    await loadChannels();
    if (wasEmpty && channels.length > 0 && selectedChannel === 0) {
      closeProfiles();
      // Flip to the sidebar on mobile so the user can see and pick a
      // channel — the empty-state hint is invisible while the chat
      // panel covers the screen.
      openSidebar();
      document.getElementById('messages').innerHTML = '<div class="empty-state"><p>' + (t('select_channel_hint') || '') + '</p></div>';
      return;
    }
    if (data && typeof data === 'object' && data.type === 'no_changes') {
      // Only show the toast if the user explicitly asked for this
      // refresh AND is still on the same channel. Channel-open and
      // auto-refresh ticks stay silent.
      if (data.channel === manualRefreshChannel && data.channel === selectedChannel) {
        showToast(t('no_new_messages'));
      }
      if (data.channel === manualRefreshChannel) manualRefreshChannel = 0;
      if (snapChannel > 0) { delete refreshingChannels[snapChannel]; var fb = document.getElementById('prog-fetch-ch-' + snapChannel); if (fb) fb.remove() }
    } else if (data && typeof data === 'object' && data.channel) {
      delete refreshingChannels[data.channel]; var fb2 = document.getElementById('prog-fetch-ch-' + data.channel); if (fb2) fb2.remove();
      if (data.channel === manualRefreshChannel) manualRefreshChannel = 0;
      // Re-render only when the content pane is visible — skip work for a channel
      // the user backed away from (and don't scroll a hidden pane).
      if (data.channel === selectedChannel && _chatPanelVisible()) await loadMessages(data.channel)
    } else if (snapChannel > 0 && snapChannel === selectedChannel) {
      delete refreshingChannels[snapChannel]; var fb3 = document.getElementById('prog-fetch-ch-' + snapChannel); if (fb3) fb3.remove();
      if (_chatPanelVisible()) await loadMessages(snapChannel)
    }
    updateSendPanel();
  });
  eventSource.onerror = function () {
    if (eventSource.readyState === EventSource.CLOSED) { eventSource.close(); setTimeout(connectSSE, 3000) }
  };
}

