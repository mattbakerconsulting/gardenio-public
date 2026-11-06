/* The teensy music player (gd-z8nt2TpS6o) on the Homestead HUD and the Lands pages. The host page inlines this and puts an
   empty <div id="tunes" data-base="<the hub's URL>"> where the player goes; mount() builds the widget inside it:
     the button   one small note; press it and a song plays, press again and it pauses. The pause glyph shows while on.
     the panel    only while a song plays (the thread's one rule): its title, a link to the song's page on the hub, a
                  scrubber with the time played and the length, and Next. Paused, it folds back into the button.
   The playlist is not in the page: the first press (its pointerdown or click) fetches data-base's playlist.json, once.
   If that fails or lists no songs, the button goes quietly disabled. Order is a shuffle made once the list arrives; a song
   that ends plays the next; one that fails to load is skipped (every song failing stops). sessionStorage remembers the
   song, the position and whether it was playing, so a Lands page's reload or the garden coming back picks up where it was
   (only a page where someone already pressed play fetches the list on load; a browser that refuses to resume without a
   gesture leaves the button idle). The lock screen gets the title and artwork through the Media Session API. Nothing is
   fetched before a press, so the page's first paint still makes no external request.
   Under node (the Go tests) only the pure part is built: fmt, shuffle, resolve, step, tracksOf and the panel's rule. */
var TUNES = (function () {
  'use strict';
  var KEY = 'tunes.player', LIST = 'playlist.json', ICON = {
    note: 'M12 3v10.55A4 4 0 1 0 14 17V7h4V3z',
    pause: 'M6 5h4v14H6zm8 0h4v14h-4z',
    next: 'M6 18l8.5-6L6 6v12zm2-8.14L11.03 12 8 14.14V9.86zM16 6h2v12h-2z',
  };

  /* fmt(seconds) -> "m:ss" or "h:mm:ss"; "" for nothing yet. */
  function fmt(s) {
    if (!isFinite(s) || s < 0) return '';
    s = Math.round(s);
    var h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), sec = (s % 60 < 10 ? '0' : '') + (s % 60);
    return h ? h + ':' + (m < 10 ? '0' : '') + m + ':' + sec : m + ':' + sec;
  }
  /* shuffle(n, rnd) -> the indexes 0..n-1 in a Fisher-Yates order drawn from rnd() in [0,1). */
  function shuffle(n, rnd) {
    var order = [], i, j, tmp;
    for (i = 0; i < n; i++) order.push(i);
    for (i = n - 1; i > 0; i--) {
      j = Math.floor(rnd() * (i + 1)); tmp = order[i]; order[i] = order[j]; order[j] = tmp;
    }
    return order;
  }
  /* resolve(base, path) -> the absolute URL of a playlist path; an absolute path is left alone. */
  function resolve(base, path) {
    if (/^[a-z][a-z0-9+.-]*:/i.test(path)) return path;
    return base.replace(/\/+$/, '') + '/' + String(path).replace(/^\/+/, '');
  }
  /* step(order, cur, d) -> the index d places on from cur in the play order, wrapping; the first when cur is not in it. */
  function step(order, cur, d) {
    if (!order.length) return -1;
    var pos = order.indexOf(cur);
    if (pos < 0) return order[0];
    return order[((pos + d) % order.length + order.length) % order.length];
  }
  /* showPanel(state) -> whether the panel is on screen: only while a song plays. */
  function showPanel(state) { return Boolean(state && state.playing); }
  /* tracksOf(playlist) -> the playable tracks of a fetched playlist.json ({tracks: [{id, title, src, ...}]}): those with a
     string id, title and src. Anything else (a failed parse, an error page, no tracks) gives []. */
  function tracksOf(p) {
    if (!p || !Array.isArray(p.tracks)) return [];
    return p.tracks.filter(function (t) {
      return t && typeof t.id === 'string' && t.id && typeof t.title === 'string' && t.title && typeof t.src === 'string' && t.src;
    });
  }

  var api = { KEY: KEY, LIST: LIST, fmt: fmt, shuffle: shuffle, resolve: resolve, step: step, showPanel: showPanel, tracksOf: tracksOf };
  if (typeof document === 'undefined') return api;

  function svg(d, cls) {
    return '<svg class="tn-ico ' + cls + '" viewBox="0 0 24 24" width="24" height="24" aria-hidden="true" focusable="false"><path d="' + d + '"/></svg>';
  }

  /* mount(root) builds the player in root. Returns the player, or null when this browser cannot play. */
  function mount(root) {
    if (!root || root.firstChild || !window.Audio || !window.fetch) return null;
    var base = root.getAttribute('data-base') || '', doc = document;
    root.innerHTML =
      '<button class="tn-btn" type="button" aria-label="Play music" aria-pressed="false" title="Music">' +
        svg(ICON.note, 'tn-note') + svg(ICON.pause, 'tn-pause') + '</button>' +
      '<div class="tn-panel" role="group" aria-label="Now playing" hidden>' +
        '<a class="tn-title" target="_blank" rel="noopener"></a>' +
        '<div class="tn-row"><span class="tn-cur"></span>' +
          '<input class="tn-range" type="range" min="0" max="1000" value="0" step="1" aria-label="Seek">' +
          '<span class="tn-dur"></span>' +
          '<button class="tn-next" type="button" aria-label="Next song" title="Next song">' + svg(ICON.next, '') + '</button>' +
        '</div>' +
      '</div>';
    var el = {
      btn: root.querySelector('.tn-btn'), panel: root.querySelector('.tn-panel'), title: root.querySelector('.tn-title'),
      range: root.querySelector('.tn-range'), cur: root.querySelector('.tn-cur'), dur: root.querySelector('.tn-dur'),
      next: root.querySelector('.tn-next'),
    };
    var audio = new Audio();
    audio.preload = 'none';
    var tracks = [], byId = {}, order = [], artist = '', loading = null, off = false;
    var cur = -1, seeking = false, lastSave = 0, failed = 0;

    /* load() -> a promise of the tracks, fetching the playlist the first time only. No songs: the button goes disabled. */
    function load() {
      if (loading) return loading;
      loading = fetch(resolve(base, LIST), { credentials: 'omit' })
        .then(function (r) { return r.ok ? r.json() : null; })
        .catch(function () { return null; })
        .then(function (p) {
          tracks = tracksOf(p);
          artist = p && typeof p.artist === 'string' ? p.artist : '';
          order = shuffle(tracks.length, Math.random);
          tracks.forEach(function (t, i) { byId[t.id] = i; });
          if (!tracks.length) unavailable();
          return tracks;
        });
      return loading;
    }
    function unavailable() {
      off = true;
      root.classList.add('off');
      el.btn.disabled = true;
      el.btn.setAttribute('aria-label', 'Music unavailable');
      el.btn.title = 'Music unavailable';
    }
    function duration() {
      if (isFinite(audio.duration) && audio.duration > 0) return audio.duration;
      var t = tracks[cur];
      return t && t.duration_s > 0 ? t.duration_s : 0;
    }
    function playing() { return cur >= 0 && !audio.paused && !audio.ended; }
    function save() {
      if (cur < 0) return; // nothing pressed yet: nothing to remember, and no list to fetch on the next page
      try {
        sessionStorage.setItem(KEY, JSON.stringify({ id: tracks[cur].id, t: audio.currentTime || 0, playing: playing() }));
      } catch (e) { /* storage blocked: the player still works on this page */ }
    }
    function restore() {
      try { return JSON.parse(sessionStorage.getItem(KEY) || 'null') || {}; } catch (e) { return {}; }
    }
    function render() {
      if (off) return;
      var on = playing();
      root.classList.toggle('on', on);
      el.btn.setAttribute('aria-pressed', on ? 'true' : 'false');
      el.btn.setAttribute('aria-label', on ? 'Pause music' : (cur >= 0 ? 'Play music (' + tracks[cur].title + ')' : 'Play music'));
      el.panel.hidden = !showPanel({ playing: on });
      if (!seeking) {
        var d = duration(), t = audio.currentTime || 0;
        el.range.value = d ? Math.round((t / d) * 1000) : 0;
        el.cur.textContent = fmt(t);
        el.dur.textContent = fmt(d);
        el.range.setAttribute('aria-valuetext', (fmt(t) || '0:00') + (d ? ' of ' + fmt(d) : ''));
      }
    }
    function mediaSession(t) {
      if (!('mediaSession' in navigator) || !window.MediaMetadata) return;
      try {
        navigator.mediaSession.metadata = new MediaMetadata({ title: t.title, artist: typeof t.artist === 'string' ? t.artist : artist, artwork: t.thumb ? [{ src: resolve(base, t.thumb) }] : [] });
      } catch (e) { /* optional */ }
    }
    /* opts: { time, play } */
    function setTrack(i, opts) {
      var t = tracks[i];
      if (!t) return;
      opts = opts || {};
      cur = i;
      var src = resolve(base, t.src);
      if (audio.src !== src) {
        audio.src = src;
        if (opts.time > 0) audio.currentTime = opts.time; // before metadata: becomes the start position
      } else if (opts.time >= 0 && Math.abs((audio.currentTime || 0) - opts.time) > 1) {
        audio.currentTime = opts.time;
      }
      el.title.textContent = t.title;
      if (t.href) { el.title.href = resolve(base, t.href); } else { el.title.removeAttribute('href'); }
      el.title.title = t.title;
      mediaSession(t);
      render();
      if (opts.play) play();
      save();
    }
    function play() {
      if (off) return;
      if (!tracks.length) { load().then(function (ts) { if (ts.length) play(); }); return; } // the first press
      if (cur < 0) { setTrack(order[0], { play: true }); return; }
      var p = audio.play();
      if (p && p.catch) p.catch(function () { render(); }); // refused (no gesture yet): the button stays idle
    }
    function toggle() { if (playing()) audio.pause(); else play(); }
    /* Next or previous in the play order, keeping the paused/playing state unless force says play (a song that ended). */
    function go(d, force) {
      if (!tracks.length) return;
      setTrack(step(order, cur, d), { play: force || playing() });
    }

    /* the press starts the fetch as early as it can, so the list is usually in by the click that plays */
    el.btn.addEventListener('pointerdown', load);
    el.btn.addEventListener('click', toggle);
    el.next.addEventListener('click', function () { go(1); });
    el.range.addEventListener('input', function () {
      seeking = true;
      el.cur.textContent = fmt((el.range.value / 1000) * duration());
    });
    el.range.addEventListener('change', function () {
      var d = duration();
      seeking = false;
      if (d) audio.currentTime = (el.range.value / 1000) * d;
      render();
    });
    audio.addEventListener('play', function () { failed = 0; render(); save(); });
    audio.addEventListener('playing', render);
    audio.addEventListener('pause', function () { render(); save(); });
    audio.addEventListener('ended', function () { if (tracks.length > 1) go(1, true); else render(); save(); });
    audio.addEventListener('timeupdate', function () {
      render();
      var now = Date.now();
      if (now - lastSave > 2000) { lastSave = now; save(); }
    });
    audio.addEventListener('durationchange', render);
    audio.addEventListener('error', function () {
      if (cur < 0) return;
      failed++;
      if (failed < tracks.length) { go(1, true); return; } // skip a song that will not load
      audio.pause();
      render();
    });
    window.addEventListener('pagehide', save);
    doc.addEventListener('visibilitychange', function () { if (doc.visibilityState === 'hidden') save(); });
    if ('mediaSession' in navigator) {
      [['play', play], ['pause', function () { audio.pause(); }], ['nexttrack', function () { go(1); }], ['previoustrack', function () { go(-1); }]].forEach(function (h) {
        try { navigator.mediaSession.setActionHandler(h[0], h[1]); } catch (e) { /* unsupported action */ }
      });
    }

    render();
    var st = restore();
    if (st.id) { // someone pressed play earlier this session: fetch the list now and pick up the same song
      load().then(function () {
        if (byId[st.id] !== undefined) setTrack(byId[st.id], { time: Number(st.t) || 0, play: Boolean(st.playing) });
      });
    }
    return {
      audio: audio, load: load, setTrack: setTrack, toggle: toggle, next: function () { go(1); },
      current: function () { return cur; }, track: function () { return tracks[cur] || null; },
    };
  }

  api.mount = mount;
  var root = document.getElementById('tunes');
  if (root) api.player = mount(root);
  return api;
})();
