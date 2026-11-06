/* What each gardener is doing right now (gd-H4jk9KgUAG, step 4a of plan gd-gAKb8Bf0Nm; gardenio/docs/homestead-farm-read-model.md,
   "Activity"). The farm document's gardeners[].doing {class, tool, at, calls} is the last thing the hub heard; the SSE `activity`
   events {id, doing, tool, at, dur, n?} on /garden/farm/events move it on between documents (web/plans.js hands each one over as a
   `farmactivity` event, so this file and the birds and poses that come after it never read the stream). A patch never carries doing,
   so it is kept here beside the gardener, not in the document. Only the tool word and the class come from the server, never what
   the tool was given. The card's line: "doing: go test · 4 s ago · 212 calls this session". */
var ACTIVITY = (function () {
  'use strict';
  /* ---------- pure logic (also run under node by the Go tests) ---------- */
  var WAITING_S = 5;   // a call started this long ago with no finish reads as waiting (activityWaiting)
  /* Calls an event says started: its n when it folds several, else 1 for a call's start (dur 0) and 0 for a finish, a thought
     or a subagent's start or stop (the hawk). The birds (4b) fly this many, a say (dur 0) one too. */
  function started(ev) {
    if (ev.n != null) return ev.n;
    return ev.dur === 0 && ev.doing !== 'think' && ev.tool !== 'subagent' ? 1 : 0;
  }
  /* A gardener's held doing {class, tool, at, calls, open} after ev, from was (null when nothing was held). A say is never open. */
  function next(was, ev) {
    var say = ev.doing === 'say';
    return { class: ev.doing, tool: ev.tool || '', at: ev.at || 0, calls: (was ? was.calls : 0) + (say ? 0 : started(ev)), open: ev.dur === 0 && ev.doing !== 'think' && !say };
  }
  /* The held doing of a gardeners[].doing {class, tool, at, calls} (the document's, or a `doing` event's as the gardener comes into
     the page's view, web/viewcut.js). The document's delegate is a subagent out (or one just back, until the next call). */
  function held1(d) { return { class: d.class, tool: d.tool || '', at: d.at || 0, calls: d.calls || 0, open: d.class === 'waiting' || d.class === 'delegate' }; }
  /* The doing the document says, for each gardener it lists: kept where it is newer than what is held, and only for gardeners still
     in the document. A patch's entries have no doing, so what is held stays. */
  function seed(held, doc) {
    var out = {};
    ((doc && doc.gardeners) || []).forEach(function (g) {
      var d = g.doing, h = held[g.id];
      if (d && (!h || (d.at || 0) >= h.at)) out[g.id] = held1(d);
      else if (h) out[g.id] = h;
    });
    return out;
  }
  /* The class at now (unix s): a call still out after WAITING_S is waiting. */
  function classAt(d, now) { return d.open && d.class !== 'delegate' && now - d.at >= WAITING_S ? 'waiting' : d.class; }
  /* The wire's tool word in words, the one map: a Bash class reads as its command ("go_test" "go test", "git_other" "git",
     "other" "a shell command", or "shell" short, for a bird's label, 4k), a tool as its name ("mcp__x__y" "mcp x y"). */
  function toolWord(t, short) {
    if (t === 'subagent') return short ? 'subagent' : 'a subagent';
    if (t === 'other') return short ? 'shell' : 'a shell command';
    return (t || '').replace(/_other$/, '').replace(/_+/g, ' ').trim();
  }
  /* The tool in words for the card's line: thinking, a subagent, waiting on one. */
  function toolWords(d, now) {
    var c = classAt(d, now), t = d.tool || '';
    if (c === 'think') return 'thinking';
    if (c === 'say') return 'saying';
    if (t === 'subagent' || c === 'delegate') return 'a subagent';
    t = toolWord(t);
    return c === 'waiting' ? 'waiting on ' + (t || 'a tool') : t || c;
  }
  function agoWords(s) { s = Math.max(0, Math.round(s)); return s < 60 ? s + ' s ago' : s < 3600 ? Math.floor(s / 60) + ' min ago' : Math.floor(s / 3600) + ' h ago'; }
  /* The card's line for d at now, or "" with nothing heard. */
  function line(d, now) {
    if (!d) return '';
    return 'doing: ' + toolWords(d, now) + ' · ' + agoWords(now - d.at) + ' · ' + d.calls + (d.calls === 1 ? ' call' : ' calls') + ' this session';
  }

  /* The pose a gardener at work wears for d at now (step 4c, gd-kxCviRUVA2; ART.drawGardener's doing poses), "" for the plain
     hoe. By tool, never by purpose: the hook events are the only per-action signal, so this is the division they allow. edit
     and a shell command keep the hoe. A subagent back (its stop: delegate, not open) is the hoe again, and so is a doing last
     heard STALE_S ago with no call out: no longer news. Phase 2's reason poses win over all of it (WORLD.gardenerState lookOf). */
  var STALE_S = 120;
  var POSE = { read: 'read', test: 'test', build: 'build', git: 'git', beads: 'beads', deploy: 'deploy', web: 'web', delegate: 'delegate', think: 'think', waiting: 'waiting' };
  var WORD = { read: 'reading', edit: 'editing', shell: 'running a command', test: 'running tests', build: 'building', git: 'using git', beads: 'writing beads', deploy: 'deploying', web: 'on the web', delegate: 'delegating to a subagent', think: 'thinking', say: 'saying', waiting: 'waiting on a tool' };
  function live(d, now) { return !!d && (d.open || (d.class !== 'delegate' && now - d.at < STALE_S)); }
  function pose(d, now) { return live(d, now) ? POSE[classAt(d, now)] || '' : ''; }
  /* The same in words, for the outline (the card has its line): "reading", "" when nothing current is heard. Never colour alone. */
  function word(d, now) { return live(d, now) ? WORD[classAt(d, now)] || '' : ''; }

  /* A transcript row (transcript.go CallRow) as a panel shows it (spec 11.4): {class, time "14:07:32", words (caption, else the
     tool word), dur ("820 ms", "5.1 s", "2 min 4 s"), failed (ok false), open (ok absent: never finished)}. */
  function two(n) { return (n < 10 ? '0' : '') + n; }
  function clock(at) { var d = new Date(at * 1000); return two(d.getHours()) + ':' + two(d.getMinutes()) + ':' + two(d.getSeconds()); }
  function durWords(ms) {
    if (!(ms > 0)) return '';
    if (ms < 1000) return Math.round(ms) + ' ms';
    if (ms < 60000) return (Math.round(ms / 100) / 10) + ' s';
    var s = Math.round(ms / 1000);
    return Math.floor(s / 60) + ' min' + (s % 60 ? ' ' + s % 60 + ' s' : '');
  }
  function callRow(r) {
    var w = String(r.cap || '').replace(/\s+/g, ' ').trim() || (r.class === 'delegate' ? 'a subagent' : toolWord(r.tool)) || r.class || 'a call';
    return { class: r.class || '', time: clock(r.at || 0), words: w, dur: durWords(r.dur), failed: r.ok === false, open: r.ok == null && r.class !== 'say' };
  }

  var api = { WAITING_S: WAITING_S, STALE_S: STALE_S, started: started, next: next, held1: held1, seed: seed, classAt: classAt, toolWord: toolWord, toolWords: toolWords, line: line, pose: pose, word: word,
    clock: clock, durWords: durWords, callRow: callRow };
  /* ---------- the page's held map ---------- */
  var held = {};
  api.of = function (id) { return held[id] || null; };
  api.lineOf = function (id) { return line(held[id], Date.now() / 1000); };
  api.poseOf = function (id) { return pose(held[id], Date.now() / 1000); };
  api.wordOf = function (id) { return word(held[id], Date.now() / 1000); };
  if (typeof document === 'undefined' || !document.addEventListener) return api;
  document.addEventListener('farmdoc', function (e) { held = seed(held, e.detail); });
  document.addEventListener('farmactivity', function (e) { var ev = e.detail; if (ev && ev.id) held[ev.id] = next(held[ev.id], ev); });
  document.addEventListener('farmdoing', function (e) { var d = e.detail, h = d && d.id && held[d.id]; if (d && d.id && (!h || (d.at || 0) >= h.at)) held[d.id] = held1(d); });
  return api;
})();
if (typeof module !== 'undefined') module.exports = ACTIVITY;
