// Optional, removable backup feed UI. All globals here are namespaced
// with `tm` / `telemirror` so removing this file (plus the markup block
// in index.html) drops the feature without touching anything else.
(function () {
  var tmChannels = [];
  var tmActive = '';
  var tmPostText = {};    // pid -> plaintext (avoids huge data-text attrs)
  var tmLastFetchedAt = {}; // username (lower) -> last successful fetch ms

  try {
    tmActive = localStorage.getItem('tm_active') || '';
  } catch (e) { }

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
  function tmToast(msg) {
    if (typeof showToast === 'function') showToast(msg);
  }

  function tmInitial(name) {
    if (!name) return '?';
    var ch = name.replace(/^@/, '').charAt(0);
    return ch ? ch.toUpperCase() : '?';
  }

  // Scroll the active channel view to the latest (bottom) post.
  window.tmScrollToBottom = function () {
    var el = document.getElementById('tmContent');
    if (el) el.scrollTop = el.scrollHeight;
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

    // Anchor by scrollHeight delta. Prepending content makes the
    // scrollHeight grow by P; we keep the same viewport content visible
    // by setting scrollTop = oldScrollTop + (newScrollHeight - oldScrollHeight).
    var scroller = document.getElementById('tmContent');
    var oldHeight = scroller ? scroller.scrollHeight : 0;
    var oldTop = scroller ? scroller.scrollTop : 0;

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
        tmRenderPosts({ channel: window._tmCurrentChannel, posts: out });
        if (!scroller) return;
        // Restore on next frame so layout has flushed. Re-correct as
        // each prepended image loads, but only as long as the user
        // hasn't scrolled away from where we put them — otherwise we'd
        // keep yanking their scroll back for several seconds.
        var lastFixedTop = -1;
        var stopAdjusting = false;
        var fix = function () {
          if (stopAdjusting) return;
          if (lastFixedTop !== -1 && Math.abs(scroller.scrollTop - lastFixedTop) > 6) {
            stopAdjusting = true;
            return;
          }
          scroller.scrollTop = oldTop + (scroller.scrollHeight - oldHeight);
          lastFixedTop = scroller.scrollTop;
        };
        requestAnimationFrame(function () {
          fix();
          var imgs = scroller.querySelectorAll('img');
          for (var k = 0; k < imgs.length; k++) {
            if (!imgs[k].complete) {
              imgs[k].addEventListener('load', fix, { once: true });
              imgs[k].addEventListener('error', fix, { once: true });
            }
          }
        });
      })
      .catch(function () {
        if (btn) { btn.disabled = false; btn.textContent = origLabel; }
        tmToast(tmI18n('telemirror_load_older_failed', 'Failed to load older posts'));
      });
  };

  // Fullscreen image overlay. Tap the backdrop or X to close. Tap on
  // the image itself does nothing (avoids accidents on iOS double-tap
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
      if (e.target === d || e.target.classList.contains('tm-lightbox-close')) {
        window.tmCloseLightbox();
      }
    });
    document.body.appendChild(d);
    try { history.pushState({ view: 'tmLightbox' }, ''); tmLightboxPushed = true; } catch (e) { }
  };

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
      inner = '<img src="' + tmEscAttr(src) + '" loading="lazy" alt=""'
        + ' onerror="tmAvatarLoadFailed(this, \'' + tmEscAttr(key) + '\')">'
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
  // Track whether we pushed a history entry on open, so close() can
  // pop it without leaving phantom states behind. Without this the
  // Android hardware back button does nothing inside this modal.
  var tmHistoryPushed = false;

  window.openTelemirror = function () {
    document.getElementById('telemirrorModal').classList.add('active');
    document.body.classList.add('tm-no-scroll');
    if (!tmHistoryPushed) {
      try { history.pushState({ view: 'telemirror' }, ''); tmHistoryPushed = true; } catch (e) { }
    }
    // Defer the layout-mode check until after the modal is rendered, so
    // getComputedStyle on .tm-menu reflects the actual CSS state.
    requestAnimationFrame(function () {
      var sb = document.getElementById('tmSidebar');
      if (sb && tmIsMobileLayout()) sb.classList.add('open');
    });
    tmLoadChannels();
  };

  // Suppress N popstate events while we unwind the history layers
  // ourselves. Without this, history.go(-N) would fire popstates that
  // re-open the sidebar / lightbox we just closed.
  var tmSuppressPopstate = 0;

  window.closeTelemirror = function () {
    var modal = document.getElementById('telemirrorModal');
    if (!modal || !modal.classList.contains('active')) return;
    var lb = document.getElementById('tmLightbox');
    if (lb) lb.remove();
    modal.classList.remove('active');
    document.body.classList.remove('tm-no-scroll');
    var sb = document.getElementById('tmSidebar');
    if (sb) sb.classList.remove('open');
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

  // Hardware / browser back: if our modal is the top of the history
  // stack, intercept and close without re-popping (history already
  // popped us).
  window.addEventListener('popstate', function () {
    // Programmatic unwind from closeTelemirror — swallow these.
    if (tmSuppressPopstate > 0) { tmSuppressPopstate--; return; }
    // Layered back: lightbox → mobile sidebar reopen → modal close.
    if (tmLightboxPushed) {
      tmLightboxPushed = false;
      var lb = document.getElementById('tmLightbox');
      if (lb) lb.remove();
      return;
    }
    if (tmChannelViewPushed && tmIsMobileLayout()) {
      tmChannelViewPushed = false;
      var sb1 = document.getElementById('tmSidebar');
      if (sb1) sb1.classList.add('open');
      return;
    }
    if (tmHistoryPushed) {
      tmHistoryPushed = false;
      var modal = document.getElementById('telemirrorModal');
      if (modal) modal.classList.remove('active');
      document.body.classList.remove('tm-no-scroll');
      var sb = document.getElementById('tmSidebar');
      if (sb) sb.classList.remove('open');
    }
  });

  window.toggleTmSidebar = function () {
    var sb = document.getElementById('tmSidebar');
    if (sb) sb.classList.toggle('open');
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
    var msg, icon;
    if (tmIsMobileLayout()) {
      icon = '☰';
      msg = tmI18n('telemirror_first_hint_mobile',
        'Tap the menu button at the top to open the channel list, then pick or add a channel.');
    } else {
      icon = document.documentElement.dir === 'rtl' ? '→' : '←';
      msg = tmI18n('telemirror_first_hint',
        'Pick a channel from the list, or add a new one with the input above.');
    }
    content.innerHTML =
      '<div class="tm-first-hint">'
      +   '<div class="tm-first-hint-arrow">' + tmEsc(icon) + '</div>'
      +   '<div class="tm-first-hint-text">' + tmEsc(msg) + '</div>'
      + '</div>';
  }

  function tmRenderChannels() {
    var box = document.getElementById('tmChannelsList');
    var html = '';
    for (var i = 0; i < tmChannels.length; i++) {
      var c = tmChannels[i];
      var active = (c.username.toLowerCase() === tmActive.toLowerCase()) ? ' active' : '';
      html += '<div class="tm-channel-item' + active + '" data-u="' + tmEscAttr(c.username) + '" onclick="tmSelectFromClick(this.dataset.u)">'
        + tmAvatarHTML(c.username, c.username, 40)
        + '<div class="tm-channel-item-meta">'
        +   '<div class="tm-channel-item-name">' + tmEsc(c.username) + (c.pinned ? ' <span class="tm-pin">📌</span>' : '') + '</div>'
        + '</div>';
      if (!c.pinned) {
        html += '<button class="tm-x" data-u="' + tmEscAttr(c.username) + '" onclick="event.stopPropagation();tmRemove(this.dataset.u)">&times;</button>';
      }
      html += '</div>';
    }
    box.innerHTML = html || '<div class="tm-empty">' + tmEsc(tmI18n('telemirror_pick_channel', 'Pick a channel')) + '</div>';
  }

  // Mobile: track that the sidebar collapsed into channel view so the
  // back button reopens it instead of dismissing the whole modal.
  var tmChannelViewPushed = false;
  window.tmSelectFromClick = function (username) {
    tmSelect(username);
    var sb = document.getElementById('tmSidebar');
    if (sb) sb.classList.remove('open');
    if (tmIsMobileLayout() && !tmChannelViewPushed) {
      try { history.pushState({ view: 'tmChannel' }, ''); tmChannelViewPushed = true; } catch (e) { }
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
        var errBody = '';
        try { errBody = await r.text(); } catch (e2) { }
        tmShowError(errBody || ('HTTP ' + r.status));
        return;
      }
      var d = await r.json();
      tmLastFetchedAt[username.toLowerCase()] = Date.now();
      tmSaveActive();
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
      var modal = document.getElementById('telemirrorModal');
      if (!modal) { resolve(window.confirm(message)); return; }
      var overlay = document.createElement('div');
      overlay.className = 'tm-confirm-overlay';
      overlay.innerHTML =
        '<div class="tm-confirm-box">'
        +   '<p class="tm-confirm-msg"></p>'
        +   '<div class="tm-confirm-actions">'
        +     '<button class="btn btn-flat" data-tm-no></button>'
        +     '<button class="btn btn-primary" data-tm-yes></button>'
        +   '</div>'
        + '</div>';
      overlay.querySelector('.tm-confirm-msg').textContent = message;
      overlay.querySelector('[data-tm-yes]').textContent = yesText || tmI18n('ok', 'OK');
      overlay.querySelector('[data-tm-no]').textContent  = noText  || tmI18n('cancel', 'Cancel');
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
        +   '<div class="tm-channel-bio-label">'
        +     tmEsc(tmI18n('telemirror_about', 'About this channel'))
        +   '</div>'
        +   '<div class="tm-channel-bio-body">' + ch.description + '</div>'
        + '</div>';
    }
    // Load-older button if we have at least one post — anchored to the
    // smallest message id so each click steps back through history.
    var oldestId = (posts[0] && posts[0].id || '').split('/').pop();
    if (oldestId) {
      html += '<div class="tm-load-older-row">'
        +   '<button class="tm-load-older" onclick="tmLoadOlder(\'' + tmEscAttr(oldestId) + '\', this)">'
        +     tmEsc(tmI18n('telemirror_load_older', 'Load older posts'))
        +   '</button>'
        + '</div>';
    }
    // Stash plain text for each post in a JS map keyed by id, so the
    // copy button doesn't have to embed huge multiline strings into a
    // data-text attribute (which broke rendering on long Persian posts).
    tmPostText = {};

    for (var i = 0; i < posts.length; i++) {
      var p = posts[i];
      var when = tmFormatTime(p.time);
      var plain = tmPostPlainText(p);
      var pid = 'tmp_' + i;
      if (plain) tmPostText[pid] = plain;

      html += '<div class="tm-post" data-pid="' + pid + '">';

      // Head: author + msg id + edited + copy button. Time at the bottom.
      html += '<div class="tm-post-head">';
      if (ch.title) html += '<span class="tm-post-author">' + tmEsc(ch.title) + '</span>';
      // p.id looks like "channel/12345" — show only the numeric part.
      var msgNum = (p.id || '').split('/').pop();
      if (msgNum) html += '<span class="tm-post-msgid">#' + tmEsc(msgNum) + '</span>';
      if (p.edited) html += '<span class="tm-post-edited">' + tmEsc(tmI18n('telemirror_edited', 'edited')) + '</span>';
      if (plain) {
        html += '<button class="tm-post-copy"'
          + ' onclick="tmCopyPost(this)">'
          + tmEsc(tmI18n('copy', 'Copy'))
          + '</button>';
      }
      html += '</div>';

      if (p.forward && p.forward.author) {
        var fwdLabel = tmI18n('telemirror_forwarded_from', 'Forwarded from');
        var fwdName  = tmEsc(p.forward.author);
        if (p.forward.url) {
          fwdName = '<a href="' + tmEscAttr(p.forward.url) + '" target="_blank" rel="noopener noreferrer">'
            + fwdName + '</a>';
        }
        html += '<div class="tm-post-forward">↪ ' + tmEsc(fwdLabel) + ' ' + fwdName + '</div>';
      }

      if (p.reply) {
        var rAuth = p.reply.author ? tmEsc(p.reply.author) : '';
        var rText = p.reply.text || '';
        html += '<div class="tm-post-reply"'
          + (p.reply.url ? ' onclick="window.open(\'' + tmEscAttr(p.reply.url) + '\', \'_blank\')"' : '')
          + (p.reply.url ? ' style="cursor:pointer"' : '')
          + '>';
        if (rAuth) html += '<div class="tm-post-reply-author">' + rAuth + '</div>';
        if (rText) html += '<div class="tm-post-reply-text">' + tmStripEmojiSprites(rText) + '</div>';
        html += '</div>';
      }

      if (p.text) html += '<div class="tm-post-text">' + tmStripEmojiSprites(p.text) + '</div>';

      if (p.media && p.media.length) {
        var photoCount = 0;
        for (var k = 0; k < p.media.length; k++) {
          if (p.media[k].type === 'photo' && p.media[k].thumb) photoCount++;
        }
        var gridClass = 'tm-post-media tm-album-' + Math.min(Math.max(photoCount, 1), 3);
        html += '<div class="' + gridClass + '">';
        for (var j = 0; j < p.media.length; j++) {
          html += tmRenderMedia(p.media[j], i, j);
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
      if (p.views) html += '<span class="tm-views">👁 ' + tmEsc(p.views) + '</span>';
      if (when) html += '<span class="tm-post-time">' + tmEsc(when) + '</span>';
      html += '</div>';

      html += '</div>';
    }
    content.innerHTML = html;
    // Jump to the bottom (newest message), like Telegram does on load.
    requestAnimationFrame(function () {
      content.scrollTop = content.scrollHeight;
    });
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

  // Strip HTML and decode common entities for the copy button.
  function tmPostPlainText(p) {
    if (!p.text) return '';
    var s = String(p.text)
      .replace(/<br\s*\/?>/gi, '\n')
      .replace(/<\/(p|div|li)>/gi, '\n')
      .replace(/<[^>]+>/g, '');
    return s
      .replace(/&nbsp;/g, ' ')
      .replace(/&amp;/g, '&')
      .replace(/&lt;/g, '<')
      .replace(/&gt;/g, '>')
      .replace(/&quot;/g, '"')
      .replace(/&#39;/g, "'")
      .trim();
  }

  // Render one media tile based on its type.
  // postIdx/mediaIdx are used to build a sane filename for downloads.
  function tmRenderMedia(m, postIdx, mediaIdx) {
    if (m.type === 'photo' && m.thumb) {
      var fname = 'photo-' + (postIdx + 1) + '-' + (mediaIdx + 1) + '.jpg';
      return '<div class="tm-photo">'
        + '<img src="' + tmEscAttr(m.thumb) + '" loading="lazy" alt=""'
        + ' referrerpolicy="no-referrer"'
        + ' onclick="tmOpenLightbox(\'' + tmEscAttr(m.thumb) + '\')"'
        + ' style="cursor:zoom-in"'
        + ' onerror="this.parentNode.classList.add(\'tm-photo-failed\')">'
        + '<a class="tm-photo-dl" href="' + tmEscAttr(m.thumb) + '"'
        +   ' download="' + tmEscAttr(fname) + '"'
        +   ' data-fname="' + tmEscAttr(fname) + '"'
        +   ' title="' + tmEscAttr(tmI18n('download', 'Download')) + '"'
        +   ' onclick="event.stopPropagation();return tmDownloadPhoto(this, event)">⬇</a>'
        + '</div>';
    }
    if (m.type === 'video') {
      var bg = m.thumb ? 'background-image:url(\'' + tmEscAttr(m.thumb) + '\')' : '';
      var dur = m.duration ? '<span class="tm-vid-dur">' + tmEsc(m.duration) + '</span>' : '';
      return '<div class="tm-vid" style="' + bg + '">'
        + '<span class="tm-vid-play">&#9654;</span>' + dur + '</div>';
    }
    if (m.type === 'voice') {
      return '<div class="tm-media-tile"><span class="tm-media-icon">🎙️</span>'
        + '<div class="tm-media-meta"><div class="tm-media-title">'
        + tmEsc(tmI18n('telemirror_voice', 'Voice message'))
        + '</div><div class="tm-media-sub">' + tmEsc(m.duration || '') + '</div></div></div>';
    }
    if (m.type === 'audio') {
      return '<div class="tm-media-tile"><span class="tm-media-icon">🎵</span>'
        + '<div class="tm-media-meta"><div class="tm-media-title">'
        + tmEsc(m.title || tmI18n('telemirror_audio', 'Audio'))
        + '</div><div class="tm-media-sub">' + tmEsc(m.subtitle || m.duration || '') + '</div></div></div>';
    }
    if (m.type === 'document') {
      return '<div class="tm-media-tile"><span class="tm-media-icon">📄</span>'
        + '<div class="tm-media-meta"><div class="tm-media-title">'
        + tmEsc(m.title || tmI18n('telemirror_file', 'File'))
        + '</div><div class="tm-media-sub">' + tmEsc(m.subtitle || '') + '</div></div></div>';
    }
    if (m.type === 'sticker' && m.thumb) {
      return '<div class="tm-sticker">'
        + '<img src="' + tmEscAttr(m.thumb) + '" loading="lazy" alt=""'
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
      return '<div class="tm-media-tile tm-media-poll"><span class="tm-media-icon">📊</span>'
        + '<div class="tm-media-meta"><div class="tm-media-title">'
        + tmEsc(m.title || tmI18n('telemirror_poll', 'Poll'))
        + '</div><div class="tm-media-sub">' + tmEsc(m.subtitle || '') + '</div>'
        + optionsHtml
        + '</div></div>';
    }
    return '';
  }

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
      var objectUrl = URL.createObjectURL(blob);
      var a = document.createElement('a');
      a.href = objectUrl;
      a.download = fname;
      a.style.display = 'none';
      document.body.appendChild(a);
      a.click();
      setTimeout(function () { URL.revokeObjectURL(objectUrl); a.remove(); }, 100);
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
    try { window.open(url, '_blank'); } catch (e) { window.location.href = url; }
  }

  window.tmCopyPost = function (btn) {
    var post = btn.closest ? btn.closest('.tm-post') : null;
    var pid = post ? post.getAttribute('data-pid') : '';
    var text = (pid && tmPostText[pid]) || '';
    if (!text) return;
    var done = function () {
      var prev = btn.textContent;
      btn.textContent = '✓';
      btn.classList.add('tm-copied');
      setTimeout(function () { btn.textContent = prev; btn.classList.remove('tm-copied'); }, 1200);
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
      if (!r.ok) { tmToast(tmI18n('telemirror_remove_pinned', 'Cannot remove pinned')); return; }
      if (tmActive.toLowerCase() === username.toLowerCase()) {
        tmActive = '';
        tmSaveActive();
      }
      await tmLoadChannels();
    } catch (e) { tmToast((e && e.message) || 'failed'); }
  };

  // Allow Enter in the add input.
  document.addEventListener('DOMContentLoaded', function () {
    var inp = document.getElementById('tmAddInput');
    if (inp) inp.addEventListener('keydown', function (e) {
      if (e.key === 'Enter') { e.preventDefault(); window.telemirrorAdd(); }
    });
  });
})();
