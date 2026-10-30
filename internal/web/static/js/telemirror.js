(function () {
  var tmChannels = [];
  var tmActive = '';
  var tmPostText = {};    // pid -> plaintext (avoids huge data-text attrs)
  var tmPostMedia = {};   // pid -> media array (for persisting images on save)
  var tmLastFetchedAt = {}; // username (lower) -> last successful fetch ms

  try {
    tmActive = localStorage.getItem('tm_active') || '';
  } catch (e) { }

  // Titles now come from the backend (persisted server-side, shared across
  // ports) in the channels list (c.title) and per-channel fetch
  // (d.channel.title). tmSetTitle just updates the in-memory list entry so the
  // sidebar shows the latest name immediately; the server already persisted it.
  function tmSetTitle(username, title) {
    if (!username || !title) return;
    var key = username.toLowerCase();
    for (var i = 0; i < tmChannels.length; i++) {
      if (tmChannels[i].username && tmChannels[i].username.toLowerCase() === key) {
        tmChannels[i].title = title;
        break;
      }
    }
  }

  function tmSaveActive() {
    try { localStorage.setItem('tm_active', tmActive || ''); } catch (e) { }
  }

  function tmI18n(key, fallback) {
    try {
      var v = (typeof t === 'function') ? t(key) : '';
      return v && v !== key ? v : (fallback || '');
    } catch (e) { return fallback || ''; }
  }

  function tmEsc(s) {
    return (typeof esc === 'function') ? esc(s) : String(s == null ? '' : s)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
  }
  function tmEscAttr(s) {
    return (typeof escAttr === 'function') ? escAttr(s) :
      tmEsc(s).replace(/"/g, '&quot;').replace(/'/g, '&#39;');
  }
  // For a value going inside a single-quoted JS string in an inline handler.
  // tmEscAttr is NOT safe there: it turns ' into &#39;, which the HTML parser
  // decodes back to ' before the JS parser ever sees it, re-opening the
  // string. See jsStr in ui.js.
  function tmJsStr(s) {
    return (typeof jsStr === 'function') ? jsStr(s) :
      tmEsc(String(s == null ? '' : s)).replace(/\\/g, '\\\\')
        .replace(/'/g, "\\'").replace(/"/g, '&quot;');
  }
  function tmToast(msg, ms) {
    if (typeof showToast === 'function') showToast(msg, ms);
  }

  function tmInitial(name) {
    if (!name) return '?';
    var ch = name.replace(/^@/, '').charAt(0);
    return ch ? ch.toUpperCase() : '?';
  }

  // Scroll the active channel view to the latest (bottom) post.
  window.tmScrollToBottom = function () {
    var el = document.getElementById('tmContent');
    if (!el) return;
    var posts = el.querySelectorAll('.tm-post');
    var last = posts[posts.length - 1];
    if (last) { last.scrollIntoView({ block: 'end' }); }
    else { el.scrollTop = el.scrollHeight; }
  };

  // Bind once on first open: toggle the scroll-down button when the
  // user is more than ~150px away from the bottom of the post list.
  function tmInitScrollBtn() {
    var sc = document.getElementById('tmContent');
    var btn = document.getElementById('tmScrollDownBtn');
    if (!sc || !btn || sc._tmScrollBtnBound) return;
    sc._tmScrollBtnBound = true;
    sc.addEventListener('scroll', function () {
      var atBottom = sc.scrollHeight - sc.scrollTop - sc.clientHeight < 150;
      btn.classList.toggle('visible', !atBottom);
    });
  }
  document.addEventListener('DOMContentLoaded', tmInitScrollBtn, { once: true });

  // Fetch a page of older posts and prepend them above the current
  // ones. Calls /api/telemirror/older/<user>?before=<id> which always
  // hits upstream (not cached — pagination data otherwise grows
  // unbounded per channel).
  window.tmLoadOlder = function (beforeId, btn) {
    if (!tmActive || !beforeId) return;
    var origLabel = btn ? btn.textContent : '';
    if (btn) { btn.disabled = true; btn.textContent = tmI18n('loading', 'Loading...'); }

    var scroller = document.getElementById('tmContent');

    fetch('/api/telemirror/older/' + encodeURIComponent(tmActive) + '?before=' + encodeURIComponent(beforeId))
      .then(function (r) { return r.ok ? r.json() : Promise.reject(new Error('HTTP ' + r.status)); })
      .then(function (older) {
        if (!older || !older.posts || !older.posts.length) {
          if (btn) { btn.textContent = tmI18n('telemirror_no_older', 'No older posts'); }
          return;
        }
        var merged = older.posts.concat(window._tmCurrentPosts || []);
        var seen = {}, out = [];
        for (var i = 0; i < merged.length; i++) {
          var id = merged[i].id;
          if (id && seen[id]) continue;
          if (id) seen[id] = true;
          out.push(merged[i]);
        }
        // Keep the current view fixed while older posts are prepended above it.
        // With content-visibility gone, scrollHeight is real, so the prepended
        // block's height = (new scrollHeight - old). Reading scrollHeight forces a
        // synchronous reflow, so this is accurate the instant after the render.
        var prevH = scroller ? scroller.scrollHeight : 0;
        var prevTop = scroller ? scroller.scrollTop : 0;
        tmRenderPosts({ channel: window._tmCurrentChannel, posts: out, keepScroll: true });
        if (scroller) scroller.scrollTop = prevTop + (scroller.scrollHeight - prevH);
      })
      .catch(function () {
        if (btn) { btn.disabled = false; btn.textContent = origLabel; }
        tmToast(tmI18n('telemirror_load_older_failed', 'Failed to load older posts'));
      });
  };

  // Fullscreen image overlay. Tap the backdrop or X to close; the image
  // itself is zoomable (pinch / double-tap / wheel, see tmEnableLightboxZoom).
  // Lightbox: open pushes a history entry, close just calls
  // history.back() — the popstate handler is the only thing that
  // removes the DOM. That way explicit close and browser-back take
  // the exact same path and no layer gets confused.
  var tmLightboxPushed = false;
  window.tmCloseLightbox = function () {
    if (!document.getElementById('tmLightbox')) return;
    if (tmLightboxPushed) { try { history.back(); } catch (e) { } }
    else { document.getElementById('tmLightbox').remove(); }
  };
  window.tmOpenLightbox = function (src) {
    var existing = document.getElementById('tmLightbox');
    if (existing) existing.remove();
    var d = document.createElement('div');
    d.id = 'tmLightbox';
    d.innerHTML =
      '<button class="tm-lightbox-close" type="button" aria-label="Close">×</button>' +
      '<img src="' + tmEscAttr(src) + '" referrerpolicy="no-referrer" alt="">';
    d.addEventListener('click', function (e) {
      // A pan/pinch that ends over the backdrop must not close the lightbox.
      if (d.__tmSuppressClose) return;
      if (e.target === d || e.target.classList.contains('tm-lightbox-close')) {
        window.tmCloseLightbox();
      }
    });
    document.body.appendChild(d);
    tmEnableLightboxZoom(d);
    try { history.pushState({ view: 'tmLightbox' }, ''); tmLightboxPushed = true; } catch (e) { }
  };

  // Lightbox zoom: pinch (touch), double-tap, and mouse wheel — plus drag to
  // pan while zoomed. Pointer Events give one code path for mouse and touch.
  // The transform is translate+scale around the container centre; zoomAt keeps
  // the focal point (fingers' midpoint / cursor / tap) fixed on screen.
  function tmEnableLightboxZoom(box) {
    var img = box.querySelector('img');
    if (!img) return;
    var scale = 1, tx = 0, ty = 0;
    var ptrs = {}, nPtrs = 0;
    var startDist = 0, startScale = 1, lastTap = 0, moved = false;
    function apply() {
      img.style.transform = (scale === 1 && !tx && !ty) ? '' :
        'translate(' + tx + 'px,' + ty + 'px) scale(' + scale + ')';
    }
    function zoomAt(k, cx, cy) {
      var r = box.getBoundingClientRect();
      var fx = cx - (r.left + r.width / 2), fy = cy - (r.top + r.height / 2);
      var ns = Math.min(6, Math.max(1, scale * k));
      k = ns / scale;
      tx = fx - k * (fx - tx);
      ty = fy - k * (fy - ty);
      scale = ns;
      if (scale === 1) { tx = 0; ty = 0; }
      apply();
    }
    function two() {
      var ids = Object.keys(ptrs);
      return [ptrs[ids[0]], ptrs[ids[1]]];
    }
    img.addEventListener('pointerdown', function (e) {
      ptrs[e.pointerId] = { x: e.clientX, y: e.clientY };
      nPtrs++;
      moved = false;
      if (nPtrs === 2) {
        var p = two();
        startDist = Math.hypot(p[0].x - p[1].x, p[0].y - p[1].y);
        startScale = scale;
      }
      try { img.setPointerCapture(e.pointerId); } catch (err) { }
      e.preventDefault();
    });
    img.addEventListener('pointermove', function (e) {
      var prev = ptrs[e.pointerId];
      if (!prev) return;
      ptrs[e.pointerId] = { x: e.clientX, y: e.clientY };
      if (nPtrs === 2 && startDist > 0) {
        var p = two();
        var d2 = Math.hypot(p[0].x - p[1].x, p[0].y - p[1].y);
        var k = (d2 / startDist) * startScale / scale;
        zoomAt(k, (p[0].x + p[1].x) / 2, (p[0].y + p[1].y) / 2);
        moved = true;
      } else if (nPtrs === 1 && scale > 1) {
        tx += e.clientX - prev.x;
        ty += e.clientY - prev.y;
        if (Math.abs(e.clientX - prev.x) + Math.abs(e.clientY - prev.y) > 2) moved = true;
        apply();
      }
    });
    function up(e) {
      if (!ptrs[e.pointerId]) return;
      delete ptrs[e.pointerId];
      nPtrs--;
      if (nPtrs < 2) startDist = 0;
      if (moved) {
        box.__tmSuppressClose = true;
        setTimeout(function () { box.__tmSuppressClose = false; }, 80);
      }
      if (e.type === 'pointerup' && !moved && nPtrs === 0) {
        var now = Date.now();
        if (now - lastTap < 300) { // double-tap: toggle 1x ↔ 2.5x at the tap point
          zoomAt(scale > 1 ? 1 / scale : 2.5, e.clientX, e.clientY);
          lastTap = 0;
        } else {
          lastTap = now;
        }
      }
    }
    img.addEventListener('pointerup', up);
    img.addEventListener('pointercancel', up);
    box.addEventListener('wheel', function (e) {
      e.preventDefault();
      zoomAt(Math.exp(-e.deltaY * 0.0022), e.clientX, e.clientY);
    }, { passive: false });
    // iOS 13+ ignores user-scalable=no; block native pinch-zoom on
    // the lightbox so our custom zoom works cleanly.
    var pg = function (e) { e.preventDefault(); };
    box.addEventListener('gesturestart', pg, { passive: false });
    box.addEventListener('gesturechange', pg, { passive: false });
  }

  // Telegram wraps every emoji in <i class="emoji" style="background-image:url(...)"><b>X</b></i>
  // so it can render its own sprite. Outside Telegram's CSS the sprite
  // never loads but the inline-styled box stays — leaving a visible
  // background patch around each glyph. Strip the wrapper; the device
  // renders the inner character natively.
  function tmStripEmojiSprites(html) {
    if (!html) return html;
    return String(html).replace(/<i\s+class="emoji"[^>]*>([\s\S]*?)<\/i>/g, '$1');
  }

  // Deterministic colour-from-name so the placeholder avatars don't all
  // look identical. Mirrors what Telegram's web client does.
  function tmAvatarColor(name) {
    var palette = ['#e57373', '#f06292', '#ba68c8', '#9575cd', '#7986cb',
      '#64b5f6', '#4fc3f7', '#4dd0e1', '#4db6ac', '#81c784',
      '#aed581', '#dce775', '#ffd54f', '#ffb74d', '#ff8a65'];
    var h = 0;
    for (var i = 0; i < name.length; i++) h = (h * 31 + name.charCodeAt(i)) >>> 0;
    return palette[h % palette.length];
  }

  function tmAvatarHTML(username, name, size) {
    size = size || 40;
    var disp = name || username || '';
    var initial = '<span class="tm-avatar-initial">' + tmEsc(tmInitial(disp)) + '</span>';
    var bg = tmAvatarColor(disp || '?');
    var key = (username || '').toLowerCase();
    var inner = initial;
    if (key) {
      // Always request the stable server URL. Server returns 404 if no
      // avatar is cached — onerror then falls back to the initial.
      var src = '/api/telemirror/avatar/' + encodeURIComponent(key);
      inner = '<img src="' + tmEscAttr(src) + '" loading="lazy" decoding="async" alt=""'
        + ' onerror="tmAvatarLoadFailed(this, \'' + tmJsStr(key) + '\')">'
        + initial;
    }
    return '<div class="tm-avatar" style="width:' + size + 'px;height:' + size + 'px;background:' + bg + '">'
      + inner + '</div>';
  }

  window.tmAvatarLoadFailed = function (img, key) {
    if (img && img.parentNode) {
      img.parentNode.classList.add('tm-avatar-fallback');
      img.remove();
    }
  };

  // ===== open / close =====
  // Telemirror is reparented INTO the shell: #tmSidebar is a pane in the
  // shared sidebar and #tmPane is the posts column. "Opening" the Mirror
  // folder just sets html.tm-open and the CSS swaps the panes in place. The
  // mobile channel view reuses the feed's .app.chat-open pane-swap. History
  // layers are still tracked so the Android back button unwinds
  // list ← channel ← folder one step at a time.
  var tmHistoryPushed = false;

  // Reset the topbar so the last channel's id/avatar doesn't linger when we
  // drop back to the list.
  function tmClearTopbar() {
    var nm = document.getElementById('tmTopbarName');
    if (nm) nm.textContent = tmI18n('telemirror_title', 'Mirror');
    var sub = document.getElementById('tmTopbarSub');
    if (sub) sub.textContent = tmI18n('telemirror_subtitle', 'Custom channels');
    var av = document.getElementById('tmTopbarAvatar');
    if (av) av.innerHTML = '';
  }

  // Back to the channel list (mobile): un-swap the content pane + clear state.
  function tmBackToList() {
    document.documentElement.classList.remove('tm-channel');
    var app = document.getElementById('app');
    if (app) app.classList.remove('chat-open');
    tmClearTopbar();
  }

  window.openTelemirror = function (adopt) {
    if (typeof closeChannelSearch === 'function') closeChannelSearch();
    document.documentElement.classList.add('tm-open');
    // Land on the list, not a half-open channel from a previous session.
    document.documentElement.classList.remove('tm-channel');
    var app = document.getElementById('app');
    if (app) app.classList.remove('chat-open');
    // adopt = switching from another section (Chat) that left its history entry
    // in place — reuse it rather than pushing a new one (avoids popstate cross-talk).
    if (adopt) {
      tmHistoryPushed = true;
    } else if (!tmHistoryPushed) {
      try { history.pushState({ view: 'telemirror' }, ''); tmHistoryPushed = true; } catch (e) { }
    }
    tmLoadChannels();
  };

  // tmCloseForNav tears Mirror down for a nav SECTION SWITCH (→ Chat) without
  // the history.go() unwind. Returns whether Mirror owned a history entry so the
  // next section can adopt it.
  window.tmCloseForNav = function () {
    var had = tmHistoryPushed || tmChannelViewPushed || tmLightboxPushed;
    tmHistoryPushed = false; tmChannelViewPushed = false; tmLightboxPushed = false;
    var lb = document.getElementById('tmLightbox'); if (lb) lb.remove();
    document.documentElement.classList.remove('tm-open');
    document.documentElement.classList.remove('tm-channel');
    var app2 = document.getElementById('app'); if (app2) app2.classList.remove('chat-open');
    return had;
  };

  // Suppress N popstate events while we unwind the history layers ourselves.
  var tmSuppressPopstate = 0;

  window.closeTelemirror = function () {
    if (!document.documentElement.classList.contains('tm-open')) return;
    if (typeof closeChannelSearch === 'function') closeChannelSearch();
    var lb = document.getElementById('tmLightbox');
    if (lb) lb.remove();
    document.documentElement.classList.remove('tm-open');
    document.documentElement.classList.remove('tm-channel');
    var app = document.getElementById('app');
    if (app) app.classList.remove('chat-open');
    var steps = (tmHistoryPushed ? 1 : 0)
      + (tmChannelViewPushed ? 1 : 0)
      + (tmLightboxPushed ? 1 : 0);
    tmHistoryPushed = false;
    tmChannelViewPushed = false;
    tmLightboxPushed = false;
    if (steps > 0) {
      tmSuppressPopstate += steps;
      try { history.go(-steps); } catch (e) { }
    }
  };

  // Hardware / browser back: unwind lightbox → channel → folder one layer at
  // a time. Programmatic unwinds from closeTelemirror are swallowed.
  window.addEventListener('popstate', function () {
    if (tmSuppressPopstate > 0) { tmSuppressPopstate--; return; }
    if (tmLightboxPushed) {
      tmLightboxPushed = false;
      var lb = document.getElementById('tmLightbox');
      if (lb) lb.remove();
      return;
    }
    if (tmChannelViewPushed && tmIsMobileLayout()) {
      tmChannelViewPushed = false;
      tmBackToList();
      return;
    }
    if (tmHistoryPushed) {
      tmHistoryPushed = false;
      document.documentElement.classList.remove('tm-open');
      document.documentElement.classList.remove('tm-channel');
      var app = document.getElementById('app');
      if (app) app.classList.remove('chat-open');
      if (typeof setActiveTab === 'function') setActiveTab('feed');
    }
  });

  // The topbar back arrow (.tm-menu, mobile only) returns to the channel list.
  window.toggleTmSidebar = function () {
    tmBackToList();
  };

  // ===== channel list =====
  async function tmLoadChannels() {
    try {
      var r = await fetch('/api/telemirror/channels');
      var d = await r.json();
      tmChannels = (d.channels || []).slice();
    } catch (e) { tmChannels = []; }
    tmRenderChannels();
    if (tmActive) {
      // Already viewing a channel from a previous session — keep it.
      tmSelect(tmActive);
    } else {
      // First open: don't auto-select. Show a hint pointing the user
      // to the channel list (which is the open drawer on mobile).
      tmShowFirstHint();
    }
  }

  // Detect "mobile layout" by checking whether the hamburger button is
  // actually visible — it's display:none on >768px via the CSS rule.
  // Way more reliable than guessing from window.matchMedia, which can
  // be off on tablets / odd viewport widths / DPR changes.
  function tmIsMobileLayout() {
    var btn = document.querySelector('.tm-menu');
    return !!(btn && getComputedStyle(btn).display !== 'none');
  }

  function tmShowFirstHint() {
    var content = document.getElementById('tmContent');
    var msg, arrowGlyph;
    if (tmIsMobileLayout()) {
      arrowGlyph = window.icon('menu');
      msg = tmI18n('telemirror_first_hint_mobile',
        'Tap the menu button at the top to open the channel list, then pick or add a channel.');
    } else {
      arrowGlyph = document.documentElement.dir === 'rtl' ? window.icon('arrowRight') : window.icon('arrowLeft');
      msg = tmI18n('telemirror_first_hint',
        'Pick a channel from the list, or add a new one with the input above.');
    }
    content.innerHTML =
      '<div class="tm-first-hint">'
      + '<div class="tm-first-hint-arrow">' + arrowGlyph + '</div>'
      + '<div class="tm-first-hint-text">' + tmEsc(msg) + '</div>'
      + '</div>';
  }

  function tmRenderChannels() {
    var box = document.getElementById('tmChannelsList');
    var html = '';
    // (Saved Messages shortcut removed — it lives in the Feed sidebar only.)
    for (var i = 0; i < tmChannels.length; i++) {
      var c = tmChannels[i];
      var active = (c.username.toLowerCase() === tmActive.toLowerCase()) ? ' active' : '';
      // Name = backend title (if known) else the username; sub = @username.
      // Mirrors the feed's name + @handle two-line rows.
      var title = c.title || c.username;
      html += '<div class="tm-channel-item' + active + '" data-u="' + tmEscAttr(c.username) + '" onclick="tmSelectFromClick(this.dataset.u)">'
        + tmAvatarHTML(c.username, title, 44)
        + '<div class="tm-channel-item-meta">'
        + '<div class="tm-channel-item-name">' + tmEsc(title) + '</div>'
        + '<div class="tm-channel-item-sub">@' + tmEsc(c.username) + '</div>'
        + '</div>';
      html += '<button class="tm-x" data-u="' + tmEscAttr(c.username) + '" onclick="event.stopPropagation();tmRemove(this.dataset.u)">&times;</button>';
      html += '</div>';
    }
    box.innerHTML = html || '<div class="tm-empty">' + tmEsc(tmI18n('telemirror_pick_channel', 'Pick a channel')) + '</div>';
  }

  // Mobile: track that the sidebar collapsed into channel view so the
  // back button reopens it instead of dismissing the whole modal.
  var tmChannelViewPushed = false;
  window.tmSelectFromClick = function (username) {
    tmSelect(username);
    // Mobile: swap to the posts column (reusing the feed's pane-swap) and mark
    // the channel open so the topbar shows a Back arrow and the nav hides.
    if (tmIsMobileLayout()) {
      document.documentElement.classList.add('tm-channel');
      var app = document.getElementById('app');
      if (app) app.classList.add('chat-open');
      if (!tmChannelViewPushed) {
        try { history.pushState({ view: 'tmChannel' }, ''); tmChannelViewPushed = true; } catch (e) { }
      }
    }
  };

  function tmShowError(msg) {
    var content = document.getElementById('tmContent');
    content.innerHTML =
      '<div class="tm-empty"><p>' + tmEsc(tmI18n('telemirror_load_failed', 'Failed to load')) + '</p>'
      + '<pre style="white-space:pre-wrap;margin-top:10px;padding:10px;background:var(--bg-elevated,var(--bg));border:1px solid var(--border);border-radius:6px;color:var(--text-dim);font-size:11px;text-align:start;max-width:600px;direction:ltr">'
      + tmEsc(String(msg).slice(0, 2000))
      + '</pre></div>';
  }

  // tmShowNotFound is the friendly (no raw error dump) state for a channel
  // the proxy has no public web preview for — wrong @username, renamed, or
  // private. Distinct from tmShowError, which is for transport failures.
  function tmShowNotFound() {
    var content = document.getElementById('tmContent');
    content.innerHTML =
      '<div class="tm-empty"><p>' + tmEsc(tmI18n('telemirror_channel_not_found', 'Channel not found.')) + '</p>'
      + '<p style="margin-top:6px;color:var(--text-dim);font-size:13px">'
      + tmEsc(tmI18n('telemirror_channel_not_found_hint', 'Check the username — it may not exist, was renamed, or is private.'))
      + '</p></div>';
  }

  async function tmSelect(username, opts) {
    opts = opts || {};
    tmActive = username;
    tmRenderChannels();
    tmRenderTopbar(null, username);
    var content = document.getElementById('tmContent');
    content.innerHTML = '<div class="tm-empty">' + tmEsc(tmI18n('telemirror_loading', 'Loading...')) + '</div>';
    try {
      var url = '/api/telemirror/channel/' + encodeURIComponent(username);
      if (opts.refresh) url += '?refresh=1';
      var r = await fetch(url);
      if (!r.ok) {
        if (r.status === 404) { tmShowNotFound(); return; }
        var errBody = '';
        try { errBody = await r.text(); } catch (e2) { }
        tmShowError(errBody || ('HTTP ' + r.status));
        return;
      }
      var d = await r.json();
      tmLastFetchedAt[username.toLowerCase()] = Date.now();
      tmSaveActive();
      if (d && d.channel && d.channel.title) { tmSetTitle(username, d.channel.title); tmRenderChannels(); }
      tmRenderTopbar(d && d.channel, username);
      tmRenderPosts(d);
    } catch (e) {
      tmShowError((e && e.message) || String(e));
    }
  }
  window.tmSelect = tmSelect;

  // Server-driven update — called from the main SSE handler when the
  // backend finishes a background telemirror refresh. If the updated
  // channel is the one currently open, silently re-fetch so the user
  // sees the new posts without manually refreshing.
  window.tmOnServerUpdate = function (username) {
    if (!tmActive || !username) return;
    if (tmActive.toLowerCase() !== username.toLowerCase()) return;
    fetch('/api/telemirror/channel/' + encodeURIComponent(username))
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (d) {
        if (!d) return;
        tmLastFetchedAt[username.toLowerCase()] = Date.now();
        tmRenderTopbar(d && d.channel, username);
        tmRenderPosts(d);
      })
      .catch(function () { });
  };

  // Refresh: warns if the last successful fetch was within 10 min,
  // since hammering Translate trips Google's per-IP rate limit.
  window.tmRefreshActive = async function () {
    if (!tmActive) return;
    var key = tmActive.toLowerCase();
    var last = tmLastFetchedAt[key] || 0;
    var ageSec = Math.floor((Date.now() - last) / 1000);
    if (last && ageSec < 600) {
      var msg = (tmI18n('telemirror_refresh_warn',
        'You refreshed this channel {n} sec ago. Refreshing too often can hit a rate limit and stop working for a while. Refresh anyway?')
      ).replace('{n}', ageSec);
      var ok = await tmConfirm(msg,
        tmI18n('telemirror_refresh_yes', 'Refresh'),
        tmI18n('cancel', 'Cancel'));
      if (!ok) return;
    }
    tmSelect(tmActive, { refresh: true });
  };

  // tmConfirm — a Promise-based yes/no dialog that mounts INSIDE the
  // telemirror modal so it sits above the drawer/backdrop. The main
  // app's showConfirmDialog appends to <body> and lives at a lower
  // z-index, so it ended up hidden behind our z:9000 modal.
  function tmConfirm(message, yesText, noText) {
    return new Promise(function (resolve) {
      // Mount over the posts column (a positioned ancestor) so it sits above
      // the posts at the right stacking — the standalone modal is gone now.
      var modal = document.querySelector('.chat-area');
      if (!modal) { resolve(window.confirm(message)); return; }
      var overlay = document.createElement('div');
      overlay.className = 'tm-confirm-overlay';
      overlay.innerHTML =
        '<div class="tm-confirm-box">'
        + '<p class="tm-confirm-msg"></p>'
        + '<div class="tm-confirm-actions">'
        + '<button class="btn btn-flat" data-tm-no></button>'
        + '<button class="btn btn-primary" data-tm-yes></button>'
        + '</div>'
        + '</div>';
      overlay.querySelector('.tm-confirm-msg').textContent = message;
      overlay.querySelector('[data-tm-yes]').textContent = yesText || tmI18n('ok', 'OK');
      overlay.querySelector('[data-tm-no]').textContent = noText || tmI18n('cancel', 'Cancel');
      var done = function (val) {
        if (overlay.parentNode) overlay.parentNode.removeChild(overlay);
        resolve(val);
      };
      overlay.addEventListener('click', function (e) {
        if (e.target === overlay) done(false);
      });
      overlay.querySelector('[data-tm-no]').addEventListener('click', function () { done(false); });
      overlay.querySelector('[data-tm-yes]').addEventListener('click', function () { done(true); });
      modal.appendChild(overlay);
    });
  }

  function tmRenderTopbar(channel, username) {
    var name = (channel && channel.title) || username;
    var sub = '';
    if (channel) {
      if (channel.subscribers) sub = channel.subscribers;
      else if (username) sub = '@' + username;
    } else if (username) {
      sub = '@' + username;
    }
    document.getElementById('tmTopbarAvatar').innerHTML = tmAvatarHTML(username, name, 38);
    document.getElementById('tmTopbarName').textContent = name || '';
    document.getElementById('tmTopbarSub').textContent = sub || '';
  }

  function tmRenderPosts(data) {
    var content = document.getElementById('tmContent');
    var posts = (data && data.posts) || [];
    var ch = (data && data.channel) || {};
    // Stash for tmLoadOlder so it can merge older posts into the same
    // view without a full re-fetch of the active channel.
    window._tmCurrentPosts = posts;
    window._tmCurrentChannel = ch;
    if (!posts.length) {
      content.innerHTML = '<div class="tm-empty">' + tmEsc(tmI18n('telemirror_no_posts', 'No posts')) + '</div>';
      return;
    }
    // Telegram order: oldest first, newest at the bottom (chat-like).
    posts.sort(function (a, b) { return (a.time || '').localeCompare(b.time || ''); });

    var html = '';
    if (ch.description) {
      // Clearly mark as channel info, not a post.
      html += '<div class="tm-channel-bio">'
        + '<div class="tm-channel-bio-label">'
        + tmEsc(tmI18n('telemirror_about', 'About this channel'))
        + '</div>'
        + '<div class="tm-channel-bio-body" dir="auto">' + ch.description + '</div>'
        + '</div>';
    }
    // Load-older button if we have at least one post — anchored to the
    // smallest message id so each click steps back through history.
    var oldestId = (posts[0] && posts[0].id || '').split('/').pop();
    if (oldestId) {
      html += '<div class="tm-load-older-row">'
        + '<button class="tm-load-older" onclick="tmLoadOlder(\'' + tmJsStr(oldestId) + '\', this)">'
        + tmEsc(tmI18n('telemirror_load_older', 'Load older posts'))
        + '</button>'
        + '</div>';
    }
    // Stash plain text for each post in a JS map keyed by id, so the
    // copy button doesn't have to embed huge multiline strings into a
    // data-text attribute (which broke rendering on long Persian posts).
    tmPostText = {};
    tmPostMedia = {};

    for (var i = 0; i < posts.length; i++) {
      var p = posts[i];
      var when = tmFormatTime(p.time);
      var plain = tmPostPlainText(p);
      var pid = 'tmp_' + i;
      if (plain) tmPostText[pid] = plain;
      if (p.media && p.media.length) tmPostMedia[pid] = p.media;

      // p.id looks like "channel/12345" — show only the numeric part.
      var msgNum = (p.id || '').split('/').pop();

      // data-post-id carries the stable Telegram locator ("channel/msgNum") so a
      // post saved to Saved Messages can later jump back to this exact post.
      html += '<div class="tm-post" data-pid="' + pid + '" data-msgid="' + tmEscAttr(msgNum || '') + '" data-post-id="' + tmEscAttr(p.id || '') + '">';

      // Head: author + msg id + edited + copy button. Time at the bottom.
      html += '<div class="tm-post-head">';
      if (ch.title) html += '<span class="tm-post-author">' + tmEsc(ch.title) + '</span>';
      if (msgNum) html += '<span class="tm-post-msgid">#' + tmEsc(msgNum) + '</span>';
      if (p.edited) html += '<span class="tm-post-edited">' + tmEsc(tmI18n('telemirror_edited', 'edited')) + '</span>';
      if (plain) {
        html += '<button class="tm-post-copy"'
          + ' onclick="tmCopyPost(this)">'
          + tmEsc(tmI18n('copy', 'Copy'))
          + '</button>';
        // Save-to-Saved-Messages button (forwards the post into Saved).
        html += '<button class="tm-post-save"'
          + ' title="' + tmEscAttr(tmI18n('forward_to_saved', 'Save to Saved Messages')) + '"'
          + ' aria-label="' + tmEscAttr(tmI18n('forward_to_saved', 'Save to Saved Messages')) + '"'
          + ' onclick="tmSavePostClick(this)">'
          + window.icon('bookmark')
          + '</button>';
      }
      html += '</div>';

      if (p.forward && p.forward.author) {
        var fwdLabel = tmI18n('telemirror_forwarded_from', 'Forwarded from');
        var fwdName = tmEsc(p.forward.author);
        if (p.forward.url) {
          fwdName = '<a href="' + tmEscAttr(p.forward.url) + '" target="_blank" rel="noopener noreferrer">'
            + fwdName + '</a>';
        }
        html += '<div class="tm-post-forward">' + window.icon('reply') + ' ' + tmEsc(fwdLabel) + ' ' + fwdName + '</div>';
      }

      if (p.reply) {
        var rAuth = p.reply.author ? tmEsc(p.reply.author) : '';
        var rText = p.reply.text || '';
        var replyMsgId = (p.reply.url || '').replace(/.*\//, '') || '';
        html += '<div class="tm-post-reply"'
          + (p.reply.url ? ' data-link="' + tmEscAttr(p.reply.url) + '"' : '')
          + (replyMsgId ? ' data-reply-msgid="' + tmEscAttr(replyMsgId) + '"' : '')
          + (p.reply.url ? ' style="cursor:pointer"' : '')
          + '>';
        if (rAuth) html += '<div class="tm-post-reply-author">' + rAuth + '</div>';
        if (rText) html += '<div class="tm-post-reply-text" dir="auto">' + tmStripEmojiSprites(rText) + '</div>';
        html += '</div>';
      }

      if (p.text) html += '<div class="tm-post-text" dir="auto">' + tmStripEmojiSprites(p.text) + '</div>';

      if (p.media && p.media.length) {
        var photoCount = 0;
        for (var k = 0; k < p.media.length; k++) {
          if (p.media[k].type === 'photo' && p.media[k].thumb) photoCount++;
        }
        var gridClass = 'tm-post-media tm-album-' + Math.min(Math.max(photoCount, 1), 3);
        html += '<div class="' + gridClass + '">';
        for (var j = 0; j < p.media.length; j++) {
          html += tmRenderMedia(p.media[j], i, j, photoCount === 1);
        }
        html += '</div>';
      }

      if (p.reactions && p.reactions.length) {
        html += '<div class="tm-post-reactions">';
        for (var r = 0; r < p.reactions.length; r++) {
          var rx = p.reactions[r];
          html += '<span class="tm-reaction">'
            + '<span class="tm-reaction-emoji">' + tmEsc(rx.emoji || '?') + '</span>'
            + (rx.count ? '<span class="tm-reaction-count">' + tmEsc(rx.count) + '</span>' : '')
            + '</span>';
        }
        html += '</div>';
      }

      // Footer: timestamp + view count.
      html += '<div class="tm-post-foot">';
      if (p.views) html += '<span class="tm-views">' + window.icon('views') + ' ' + tmEsc(p.views) + '</span>';
      // <bdi> isolates the localized date so its parts (day / month / year /
      // time) keep their correct order inside the LTR-forced foot — a Jalali
      // Persian date otherwise got bidi-scrambled in RTL mode.
      if (when) html += '<span class="tm-post-time"><bdi>' + tmEsc(when) + '</bdi></span>';
      html += '</div>';

      html += '</div>';
    }
    content.innerHTML = html;
    tmNeuterLinks(content);
    // Jump to the newest (bottom) post via scrollIntoView on the last post.
    // Re-assert a couple of times to absorb the height growth as the newest
    // posts' images finish loading, unless the user has already scrolled away.
    // Skipped for "load older", which keeps its own anchor.
    if (!data.keepScroll) {
      requestAnimationFrame(function () {
        var p = content.querySelectorAll('.tm-post');
        var last = p[p.length - 1];
        if (!last) { content.scrollTop = content.scrollHeight; return; }
        last.scrollIntoView({ block: 'end' });
        // Re-assert: content-visibility resolves heights lazily; on slower
        // Android WebViews the first scrollIntoView may land before the
        // browser finishes estimating. Two retries absorb that growth.
        var retries = [150, 500];
        var pinned = content.scrollTop;
        retries.forEach(function (ms) {
          setTimeout(function () {
            if (Math.abs(content.scrollTop - pinned) < 4) {
              last.scrollIntoView({ block: 'end' });
              pinned = content.scrollTop;
            }
          }, ms);
        });
      });
    }
  }

  // Format a timestamp for display. Persian users see Jalali calendar
  // (Intl handles the conversion natively in modern browsers); other
  // languages get the system locale.
  function tmFormatTime(iso) {
    if (!iso) return '';
    var d = new Date(iso);
    if (isNaN(d.getTime())) return '';
    var lang = (typeof window !== 'undefined' && window.lang) ||
      localStorage.getItem('thefeed_lang') || 'en';
    var locale = (lang === 'fa') ? 'fa-IR-u-ca-persian' : undefined;
    try {
      return d.toLocaleString(locale, {
        year: 'numeric', month: 'short', day: 'numeric',
        hour: '2-digit', minute: '2-digit'
      });
    } catch (e) {
      return d.toLocaleString();
    }
  }

  // Strip HTML and decode all entities for the copy button.
  // Routes through a detached <div> so the browser handles every
  // entity form: named (&quot;), decimal (&#34;), hex (&#x22;). The
  // previous hand-rolled regex decoder only knew the named forms,
  // so JSON-heavy posts (full of " chars that Go's html.Render emits
  // as &#34;) copied as "&#34;_comment&#34;:" gibberish.
  //
  // The <br> / </p> / </div> / </li> pre-pass keeps block-level
  // breaks as newlines — without it, paragraphs run together because
  // textContent doesn't insert whitespace at element boundaries.
  //
  // Safe even though we set innerHTML on a fragment: the element is
  // detached, scripts aren't executed by spec, and the input is the
  // same HTML we already inject into the live DOM via tm-post-text.
  function tmPostPlainText(p) {
    if (!p.text) return '';
    // Mark INTENTIONAL breaks (<br> / block ends) with a sentinel that survives
    // the whitespace collapse below.
    var BR = String.fromCharCode(1);
    var s = String(p.text)
      .replace(/<br\s*\/?>/gi, BR)
      .replace(/<\/(p|div|li)>/gi, BR);
    var tmp = document.createElement('div');
    tmp.innerHTML = s;
    var raw = (tmp.textContent || tmp.innerText || '');
    // The browser collapses source whitespace — including the newlines a channel
    // puts between inline elements (e.g. one custom-emoji per source line) — to a
    // single space when rendering; textContent does NOT. Collapse it ourselves so
    // copy/save matches what's shown. Without this, emoji-heavy posts (e.g.
    // SoftNetConnect's 🟩 rows) saved with one spurious line break per emoji.
    // \s doesn't match the BR sentinel, so explicit breaks are preserved.
    raw = raw.replace(/\s+/g, ' ');
    raw = raw.split(BR).map(function (ln) { return ln.trim(); }).join('\n');
    raw = raw.replace(/\n{3,}/g, '\n\n').trim();
    return raw;
  }

  // Render one media tile based on its type.
  // postIdx/mediaIdx are used to build a sane filename for downloads.
  function tmRenderMedia(m, postIdx, mediaIdx, single) {
    if (m.type === 'photo' && m.thumb) {
      var fname = 'photo-' + (postIdx + 1) + '-' + (mediaIdx + 1) + '.jpg';
      // For SINGLE photos we know the real aspect ratio up front (parsed from
      // Telegram's markup), so reserve the EXACT box — the image loads into a
      // box that's already its final size, so nothing reflows and the scroll
      // never jumps. tmPhotoLoaded stays as a fallback for posts without ratio.
      var ratioStyle = (single && m.ratio > 0) ? ' style="aspect-ratio:' + m.ratio.toFixed(4) + '"' : '';
      return '<div class="tm-photo tm-photo-loading"' + ratioStyle + '>'
        + '<img src="' + tmEscAttr(m.thumb) + '" loading="lazy" decoding="async" alt=""'
        + ' referrerpolicy="no-referrer"'
        + ' onload="tmPhotoLoaded(this)"'
        + ' onclick="tmOpenLightbox(\'' + tmJsStr(m.thumb) + '\')"'
        + ' style="cursor:zoom-in"'
        + ' onerror="this.parentNode.classList.add(\'tm-photo-failed\')">'
        + '<a class="tm-photo-dl" href="' + tmEscAttr(m.thumb) + '"'
        + ' download="' + tmEscAttr(fname) + '"'
        + ' data-fname="' + tmEscAttr(fname) + '"'
        + ' title="' + tmEscAttr(tmI18n('download', 'Download')) + '"'
        + ' onclick="event.stopPropagation();return tmDownloadPhoto(this, event)">' + window.icon('download') + '</a>'
        + '</div>';
    }
    if (m.type === 'video') {
      var bg = m.thumb ? 'background-image:url(\'' + tmJsStr(m.thumb) + '\')' : '';
      var dur = m.duration ? '<span class="tm-vid-dur">' + tmEsc(m.duration) + '</span>' : '';
      return '<div class="tm-vid" style="' + bg + '">'
        + '<span class="tm-vid-play">' + window.icon('play') + '</span>' + dur + '</div>';
    }
    if (m.type === 'voice') {
      return '<div class="tm-media-tile"><span class="tm-media-icon">' + window.icon('voice') + '</span>'
        + '<div class="tm-media-meta"><div class="tm-media-title">'
        + tmEsc(tmI18n('telemirror_voice', 'Voice message'))
        + '</div><div class="tm-media-sub">' + tmEsc(m.duration || '') + '</div></div></div>';
    }
    if (m.type === 'audio') {
      return '<div class="tm-media-tile"><span class="tm-media-icon">' + window.icon('music') + '</span>'
        + '<div class="tm-media-meta"><div class="tm-media-title">'
        + tmEsc(m.title || tmI18n('telemirror_audio', 'Audio'))
        + '</div><div class="tm-media-sub">' + tmEsc(m.subtitle || m.duration || '') + '</div></div></div>';
    }
    if (m.type === 'document') {
      return '<div class="tm-media-tile"><span class="tm-media-icon">' + window.icon('document') + '</span>'
        + '<div class="tm-media-meta"><div class="tm-media-title">'
        + tmEsc(m.title || tmI18n('telemirror_file', 'File'))
        + '</div><div class="tm-media-sub">' + tmEsc(m.subtitle || '') + '</div></div></div>';
    }
    if (m.type === 'sticker' && m.thumb) {
      return '<div class="tm-sticker">'
        + '<img src="' + tmEscAttr(m.thumb) + '" loading="lazy" decoding="async" alt=""'
        + ' referrerpolicy="no-referrer"'
        + ' onerror="this.parentNode.classList.add(\'tm-photo-failed\')">'
        + '</div>';
    }
    if (m.type === 'poll') {
      var optionsHtml = '';
      if (m.options && m.options.length) {
        optionsHtml = '<ul class="tm-poll-options">';
        for (var k = 0; k < m.options.length; k++) {
          optionsHtml += '<li>' + tmEsc(m.options[k]) + '</li>';
        }
        optionsHtml += '</ul>';
      }
      return '<div class="tm-media-tile tm-media-poll"><span class="tm-media-icon">' + window.icon('stats') + '</span>'
        + '<div class="tm-media-meta"><div class="tm-media-title">'
        + tmEsc(m.title || tmI18n('telemirror_poll', 'Poll'))
        + '</div><div class="tm-media-sub">' + tmEsc(m.subtitle || '') + '</div>'
        + optionsHtml
        + '</div></div>';
    }
    return '';
  }

  // tmPhotoLoaded runs when a single photo's bytes arrive. It swaps the
  // placeholder aspect for the image's real one and — crucially — if the photo
  // sits ABOVE the viewport (the user scrolled up past it), nudges scrollTop by
  // the height change so the post they're reading doesn't jump. Albums keep
  // their fixed CSS aspect, so this only touches single photos.
  window.tmPhotoLoaded = function (img) {
    var box = img.closest ? img.closest('.tm-photo') : null;
    if (!box) return;
    box.classList.remove('tm-photo-loading');
    if (box.style.aspectRatio) return; // exact box already reserved from the parsed ratio — no reflow
    if (!box.closest('.tm-album-1')) return; // albums already reserve space
    if (!img.naturalWidth || !img.naturalHeight) return;
    var sc = document.getElementById('tmContent');
    var topBefore = box.getBoundingClientRect().top;
    var hBefore = box.getBoundingClientRect().height;
    box.style.aspectRatio = img.naturalWidth + ' / ' + img.naturalHeight;
    if (!sc) return;
    var delta = box.getBoundingClientRect().height - hBefore;
    // Compensate only when the box starts above the visible top — i.e. its
    // growth pushed the content the user is reading downward.
    if (delta && topBefore < sc.getBoundingClientRect().top) {
      sc.scrollTop += delta;
    }
  };

  // tmDownloadPhoto fetches the bytes and either hands them to the
  // Android bridge (saveMedia) or builds a blob-URL <a download> on
  // desktop. <a download> alone doesn't work on Android WebView for
  // cross-origin URLs.
  window.tmDownloadPhoto = function (anchor, ev) {
    if (ev) ev.stopPropagation();
    var url = anchor.getAttribute('href');
    var fname = anchor.getAttribute('data-fname') || 'photo.jpg';
    var bridge = (typeof window !== 'undefined' && window.Android) ? window.Android : null;

    var doFetch = function () {
      return fetch(url, { referrerPolicy: 'no-referrer' }).then(function (r) {
        if (!r.ok) throw new Error('http ' + r.status);
        return r.blob();
      });
    };

    if (bridge && typeof bridge.saveMedia === 'function') {
      doFetch().then(function (blob) {
        return tmBlobToBase64(blob).then(function (b64) {
          try { bridge.saveMedia(b64, blob.type || 'image/jpeg', fname); }
          catch (e) { tmFallbackOpen(url); }
        });
      }).catch(function () { tmFallbackOpen(url); });
      return false;
    }

    doFetch().then(function (blob) {
      triggerDownload(blob, fname);
    }).catch(function () { window.location.href = url; });
    return false;
  };

  function tmBlobToBase64(blob) {
    return new Promise(function (resolve, reject) {
      var fr = new FileReader();
      fr.onload = function () {
        var s = fr.result || '';
        var i = s.indexOf(',');
        resolve(i >= 0 ? s.substring(i + 1) : s);
      };
      fr.onerror = function () { reject(fr.error); };
      fr.readAsDataURL(blob);
    });
  }

  function tmFallbackOpen(url) {
    openExternal(url); // shared impl in core.js — handles blocked popups/WebViews
  }

  window.tmCopyPost = function (btn) {
    var post = btn.closest ? btn.closest('.tm-post') : null;
    var pid = post ? post.getAttribute('data-pid') : '';
    var text = (pid && tmPostText[pid]) || '';
    if (!text) return;
    var done = function () {
      var prev = btn.innerHTML;
      btn.innerHTML = window.icon('check');
      btn.classList.add('tm-copied');
      setTimeout(function () { btn.innerHTML = prev; btn.classList.remove('tm-copied'); }, 1200);
      tmToast(tmI18n('copied', 'Copied'));
    };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(done).catch(function () {
        // Fallback: hidden textarea + execCommand.
        tmCopyFallback(text, done);
      });
    } else {
      tmCopyFallback(text, done);
    }
  };

  // tmSavePostClick — inline-onclick shim for the .tm-post-save button: resolves
  // the post's pid + locator from the DOM and forwards to tmSavePost. Passing a
  // null channel makes tmSavePost fall back to the active channel (tmActive).
  window.tmSavePostClick = function (btn) {
    var post = btn.closest ? btn.closest('.tm-post') : null;
    if (!post) return;
    window.tmSavePost(post.getAttribute('data-pid'), null, btn, post.getAttribute('data-post-id'));
  };

  // tmSavePost — forward a Telemirror post into Saved Messages as a read-only
  // kind:"chat" snapshot. tmSource ("channel/msgNum") lets Saved jump back here.
  window.tmSavePost = async function (pid, channelUsername, btn, tmSource) {
    var text = pid && tmPostText[pid];
    if (!text && !pid) return;
    try {
      if (btn) { btn.disabled = true; }
      var media = [];
      var postMedia = (pid && tmPostMedia[pid]) || [];
      for (var mi = 0; mi < postMedia.length; mi++) {
        if (postMedia[mi].type === 'photo' && postMedia[mi].thumb) {
          try {
            var imgUrl = postMedia[mi].thumb;
            var pd = null;
            // Browser-side: fetch image directly and upload blob.
            try {
              var imgResp = await fetch(imgUrl, { referrerPolicy: 'no-referrer' });
              if (imgResp.ok) {
                var blob = await imgResp.blob();
                if (blob.size > 0) {
                  var up = await fetch('/api/saved/media/upload-blob', {
                    method: 'POST',
                    headers: { 'Content-Type': blob.type || 'image/jpeg' },
                    body: blob
                  });
                  if (up.ok) pd = await up.json();
                }
              }
            } catch (e) { /* CORS or network — try server-side */ }
            // Fallback: ask the server to fetch it.
            if (!pd) {
              var pr = await fetch('/api/saved/media/persist-tm', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ url: imgUrl })
              });
              if (pr.ok) pd = await pr.json();
            }
            if (pd && pd.size) {
              var fn = 'photo-' + (mi + 1) + '.jpg';
              media.push({ tag: '[IMAGE]', size: pd.size, crc: pd.crc, persisted: true, fname: fn });
            }
          } catch (e) { /* skip this image */ }
        }
      }
      var payload = {
        text: text || '',
        contactName: channelUsername || tmActive,
        tmSource: tmSource || '',
        media: media
      };
      var r = await fetch('/api/saved/from-chat', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload)
      });
      if (r.status === 423) {
        tmToast(tmI18n('saved_locked', 'Saved Messages is locked'));
        if (btn) btn.disabled = false;
        return;
      }
      if (!r.ok) {
        tmToast(tmI18n('saved_save_failed', 'Could not save'));
        if (btn) btn.disabled = false;
        return;
      }
      tmToast(tmI18n('saved_toast', 'Forwarded to Saved'));
      if (btn) {
        btn.classList.add('tm-post-save-done');
        // Don't re-enable — visually indicate "already saved"
      }
      if (typeof updateSavedBadge === 'function') updateSavedBadge();
    } catch (e) {
      tmToast(tmI18n('saved_save_failed', 'Could not save'));
      if (btn) btn.disabled = false;
    }
  };

  // tmCloseThenSaved — leave the Mirror folder and open Saved Messages.
  // Saved renders into the feed shell's #messages, so we must drop tm-open.
  // We switch DIRECTLY (no closeTelemirror history.go(-N) cascade) — those
  // popstates were what made the view visibly "jump to the feed list" first.
  // We still reset the tm history flags so a later Back doesn't double-unwind.
  window.tmCloseThenSaved = function () {
    var lb = document.getElementById('tmLightbox'); if (lb) lb.remove();
    tmHistoryPushed = false; tmChannelViewPushed = false; tmLightboxPushed = false;
    // Render Saved into the still-hidden feed content FIRST (tm-open keeps
    // #messages hidden), then drop tm-open to reveal it — so the feed content
    // never flashes between Mirror and Saved.
    var reveal = function () {
      document.documentElement.classList.remove('tm-open');
      document.documentElement.classList.remove('tm-channel');
      if (typeof setActiveTab === 'function') setActiveTab('feed');
    };
    if (typeof openSavedMessages !== 'function') { reveal(); return; }
    var p;
    try { p = openSavedMessages(); } catch (e) { reveal(); return; }
    if (p && typeof p.then === 'function') p.then(reveal, reveal); else reveal();
  };
  function tmCopyFallback(text, done) {
    var ta = document.createElement('textarea');
    ta.value = text;
    ta.style.position = 'fixed'; ta.style.left = '-9999px';
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand('copy'); done(); } catch (e) { }
    document.body.removeChild(ta);
  }

  // ===== add / remove =====
  window.telemirrorAdd = async function () {
    var input = document.getElementById('tmAddInput');
    var u = (input.value || '').trim();
    if (!u) return;
    try {
      var r = await fetch('/api/telemirror/channels', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ action: 'add', username: u })
      });
      if (!r.ok) { tmToast(tmI18n('telemirror_invalid_user', 'Invalid username')); return; }
      input.value = '';
      var b = document.getElementById('tmAddBtn'); if (b) b.disabled = true;
      await tmLoadChannels();
    } catch (e) { tmToast((e && e.message) || 'failed'); }
  };

  window.tmRemove = async function (username) {
    try {
      var r = await fetch('/api/telemirror/channels', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ action: 'remove', username: username })
      });
      if (!r.ok) { tmToast(tmI18n('telemirror_remove_failed', 'Could not remove channel')); return; }
      if (tmActive.toLowerCase() === username.toLowerCase()) {
        tmActive = '';
        tmSaveActive();
      }
      await tmLoadChannels();
    } catch (e) { tmToast((e && e.message) || 'failed'); }
  };

  document.addEventListener('DOMContentLoaded', function () {
    var inp = document.getElementById('tmAddInput');
    var addBtn = document.getElementById('tmAddBtn');
    function syncAddBtn() { if (addBtn) addBtn.disabled = !(inp && inp.value.trim()); }
    if (inp) {
      inp.addEventListener('input', syncAddBtn);
      inp.addEventListener('keydown', function (e) {
        if (e.key === 'Enter') { e.preventDefault(); window.telemirrorAdd(); }
      });
      syncAddBtn();
    }

    // Intercept link clicks and long-press inside posts.
    var tmC = document.getElementById('tmContent');
    if (tmC) {
      function tmLinkHandler(e) {
        // Reply divs: scroll to replied-to post if loaded, else show link sheet
        var rep = e.target.closest('[data-reply-msgid]');
        if (rep) {
          e.preventDefault(); e.stopPropagation();
          var targetId = rep.getAttribute('data-reply-msgid');
          var parentPost = rep.closest('.tm-post');
          var currentId = parentPost ? parentPost.getAttribute('data-msgid') : '';
          if (targetId && targetId !== currentId && tmScrollToPost(targetId)) return;
          tmToast(tmI18n('telemirror_reply_not_loaded', 'Original message not loaded'));
          return;
        }
        // data-href on neutered <a> tags
        var a = e.target.closest('a[data-href]');
        if (a) { e.preventDefault(); e.stopPropagation(); tmHandleLink(a.getAttribute('data-href')); return; }
      }
      tmC.addEventListener('click', tmLinkHandler);
      tmC.addEventListener('contextmenu', tmLinkHandler);
    }
  });

  // Strip href from all <a> tags inside rendered posts so the browser/OS never
  // navigates or shows "Open in Telegram?". The real URL is kept in data-href;
  // the delegated click handler reads it. Also drop any inline on* handler the
  // ANCHOR carries (some channels ship onclick="return confirm(...)"); scope it
  // to anchors only — stripping on* from every element also killed the frontend's
  // own <img onclick="tmOpenLightbox()">, so images stopped opening. The backend
  // sanitises channel HTML server-side; this only covers posts cached before that.
  function tmNeuterLinks(container) {
    var links = container.querySelectorAll('a[href]');
    for (var i = 0; i < links.length; i++) {
      var a = links[i];
      if (a.classList.contains('tm-photo-dl')) continue;
      for (var k = a.attributes.length - 1; k >= 0; k--) {
        if (/^on/i.test(a.attributes[k].name)) a.removeAttribute(a.attributes[k].name);
      }
      var url = a.href;
      // Telegram double-encodes & as &amp;amp; in embed hrefs; the
      // browser decodes one level → &amp; remains in the resolved URL.
      url = url.replace(/&amp;/g, '&');
      var text = (a.textContent || '').trim();
      if (text && /^https?:\/\//i.test(text) && text.length > url.length) {
        url = text;
      }
      // Google Translate splits URLs at & boundaries — the <a> tag
      // wraps only the first param and the rest leaks as trailing
      // text/inline nodes. Stitch them back by consuming siblings
      // that look like query-param continuations (&key=value...).
      var sib = a.nextSibling;
      while (sib) {
        var chunk = '';
        if (sib.nodeType === 3) { chunk = sib.textContent || ''; }
        else if (sib.nodeType === 1 && sib.tagName !== 'BR') { chunk = sib.textContent || ''; }
        else break;
        // Must start with & and look like a query param continuation
        if (/^&[A-Za-z0-9_]+=/.test(chunk)) {
          // Might contain trailing text after the URL (newline, space, etc.)
          var paramMatch = chunk.match(/^(&[A-Za-z0-9_]+=[^\s<>]*)/);
          if (paramMatch) {
            url += paramMatch[1];
            var leftover = chunk.substring(paramMatch[1].length);
            if (leftover) {
              sib.textContent = leftover;
              break;
            }
            var next = sib.nextSibling;
            sib.parentNode.removeChild(sib);
            sib = next;
            continue;
          }
        }
        break;
      }
      a.setAttribute('data-href', url);
      a.removeAttribute('href');
      a.removeAttribute('target');
      a.style.cursor = 'pointer';
    }
  }

  // Parse a t.me URL → { user, postId } or null. Shared impl in ui.js
  // (handles /s/ links, trailing slashes, queries and special paths).
  function tmParseTgLink(url) { return parseTgLink(url); }

  // Scroll to a post by its numeric message ID. Returns true if found.
  // Instant jump — a smooth scroll over a long distance eats most of the
  // 2s highlight before arrival and gets mistargeted when images above
  // load mid-flight. After the jump, media above the post keeps expanding
  // and drags it back out of view, so keep re-centering until the layout
  // settles; any manual scroll from the user cancels that immediately.
  var _tmSettleGen = 0;
  function tmScrollToPost(msgId) {
    if (!msgId) return false;
    var el = document.querySelector('.tm-post[data-msgid="' + msgId + '"]');
    if (!el) return false;
    el.scrollIntoView({ block: 'center' });
    el.classList.remove('tm-post-highlight');
    void el.offsetWidth; // restart the animation on repeat jumps
    el.classList.add('tm-post-highlight');
    // A newer jump owns the viewport — bump the generation so an older
    // settle loop can't tug-of-war against this one for up to 2s.
    var gen = ++_tmSettleGen;
    var until = Date.now() + 2000;
    var cancelled = false;
    var cancel = function () { cancelled = true; };
    window.addEventListener('wheel', cancel, { once: true, passive: true });
    window.addEventListener('touchstart', cancel, { once: true, passive: true });
    (function settle() {
      if (cancelled || gen !== _tmSettleGen || Date.now() > until) return;
      var q = document.querySelector('.tm-post[data-msgid="' + msgId + '"]');
      var host = document.querySelector('.tm-content');
      if (!q || !host) return;
      var r = q.getBoundingClientRect(), h = host.getBoundingClientRect();
      var mid = (h.top + h.bottom) / 2, c = (r.top + r.bottom) / 2;
      // Only correct real drift (image loads shift by tens/hundreds of px);
      // small offsets are left alone so we don't fight normal jitter.
      if (Math.abs(c - mid) > 48) q.scrollIntoView({ block: 'center' });
      setTimeout(settle, 200);
    })();
    return true;
  }

  // Jump used after switching channels from the link sheet: posts render
  // asynchronously after tmSelect, so retry until the target appears
  // instead of a single 300ms shot (which often missed on slow loads).
  function tmGotoPost(msgId) {
    if (tmScrollToPost(msgId)) return; // already rendered — no toasts
    // Immediate feedback + a timer-guaranteed verdict: the error must fire
    // even if a retry step dies, else a lone "finding…" looks like a bug.
    // The toast is sticky for the whole wait — a 2.2s fade left a silent
    // gap before the verdict that read as "no error shown". 5s deadline:
    // tmSelect already fetched the posts, nothing new arrives later.
    var DEADLINE = 5000;
    tmToast(tmI18n('finding_post', 'Finding the post…'), DEADLINE + 500);
    // Bail (silently) if the user switches channels mid-search — both to
    // skip a stale verdict and because msg numbers are per-channel, so a
    // late retry could match a same-numbered post in the new channel.
    var wantCh = tmActive;
    var done = false;
    var failT = setTimeout(function () {
      done = true;
      if (tmActive === wantCh) tmToast(tmI18n('telemirror_post_not_loaded', 'Post not loaded'));
      else if (typeof hideToast === 'function') hideToast();
    }, DEADLINE);
    (function attempt() {
      if (done) return;
      if (tmActive !== wantCh || tmScrollToPost(msgId)) {
        done = true;
        clearTimeout(failT);
        if (typeof hideToast === 'function') hideToast(); // clear the sticky "finding…"
        return;
      }
      setTimeout(attempt, 400);
    })();
  }
  window.tmScrollToPost = tmScrollToPost;
  window.tmGotoPost = tmGotoPost;

  // Central link handler — decides what to do with a URL from a post.
  function tmHandleLink(url) {
    var tg = tmParseTgLink(url);
    // Same channel + post in view → just scroll to it
    if (tg && tg.postId && tmActive && tg.user.toLowerCase() === tmActive.toLowerCase()) {
      if (tmScrollToPost(tg.postId)) return;
    }
    tmShowLinkSheet(url);
  }

  window.tmShowLinkSheet = tmShowLinkSheet;
  function tmShowLinkSheet(url) {
    var old = document.getElementById('tmLinkSheet');
    if (old) old.remove();
    var tg = tmParseTgLink(url);
    // When the linked channel is already in the mirror list the button
    // opens it instead of re-adding. inList keeps the stored username's
    // canonical case for tmSelect.
    var inList = '';
    if (tg) {
      for (var ci = 0; ci < tmChannels.length; ci++) {
        var cu = tmChannels[ci].username || '';
        if (cu.toLowerCase() === tg.user.toLowerCase()) { inList = cu; break; }
      }
    }
    var overlay = document.createElement('div');
    overlay.id = 'tmLinkSheet';
    overlay.className = 'tm-link-overlay';
    var html = '<div class="tm-link-sheet">'
      + '<button class="tm-link-close" type="button" aria-label="' + tmEsc(tmI18n('close', 'Close')) + '">' + window.icon('x') + '</button>'
      + '<div class="tm-link-title">' + tmEsc(tmI18n('telemirror_open_this_link', 'Open this link?')) + '</div>'
      + '<div class="tm-link-url" dir="ltr">' + tmEsc(url) + '</div>'
      + '<div class="tm-link-actions">'
      + '<button class="tm-link-btn tm-link-copy">' + tmEsc(tmI18n('telemirror_copy_link', 'Copy link')) + '</button>'
      + '<button class="tm-link-btn tm-link-open">' + tmEsc(tmI18n('telemirror_open_link', 'Open in browser')) + '</button>'
      + '</div>';
    if (tg) {
      html += '<button class="tm-link-btn tm-link-add-ch">'
        + tmEsc((inList
          ? tmI18n('telemirror_open_channel', 'Open @{u}')
          : tmI18n('telemirror_add_channel', 'Add @{u} to channels')).replace('{u}', tg.user))
        + '</button>';
    }
    html += '</div>';
    overlay.innerHTML = html;
    overlay.addEventListener('click', function (e) {
      if (e.target === overlay) overlay.remove();
    });
    // Direct handler — a real click targets the svg inside the button, so
    // target-class matching on the overlay never fires (see core.js note).
    overlay.querySelector('.tm-link-close').onclick = function () { overlay.remove(); };
    overlay.querySelector('.tm-link-copy').onclick = function () {
      try {
        if (navigator.clipboard) { navigator.clipboard.writeText(url).catch(function () {}); }
        else { var ta = document.createElement('textarea'); ta.value = url; document.body.appendChild(ta); ta.select(); document.execCommand('copy'); ta.remove(); }
      } catch (e) { /* clipboard unavailable */ }
      tmToast(tmI18n('copied', 'Copied'));
      overlay.remove();
    };
    overlay.querySelector('.tm-link-open').onclick = function () {
      overlay.remove();
      openExternal(url); // shared impl in core.js — no double-open
    };
    if (tg) {
      overlay.querySelector('.tm-link-add-ch').onclick = async function () {
        overlay.remove();
        try {
          var user = inList;
          if (!user) {
            var r = await fetch('/api/telemirror/channels', {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ action: 'add', username: tg.user })
            });
            if (!r.ok) { tmToast(tmI18n('telemirror_invalid_user', 'Invalid username')); return; }
            await tmLoadChannels();
            user = tg.user;
          }
          // Switch to that channel and scroll to the post if specified
          tmActive = user;
          tmSaveActive();
          await tmSelect(user);
          if (tg.postId) setTimeout(function () { tmGotoPost(tg.postId); }, 300);
          if (!inList) tmToast(tmI18n('telemirror_channel_added', '@{u} added').replace('{u}', tg.user));
        } catch (e) { tmToast((e && e.message) || 'failed'); }
      };
    }
    document.body.appendChild(overlay);
  }
})();
