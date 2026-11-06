// incident_intel_uplot.js — Phase 7. Lazy bootstrap of uPlot charts and
// hover-sync bus shared by sparklines + event timelines on the same page.
// Bead: ka-0000005419.
//
// "Lazy" here means: the static asset is deferred at page load (cheap), but
// uPlot is only constructed against the DOM when at least one
// [data-uplot-config] node is present. Pages without time-series widgets do
// nothing beyond a single querySelector call.
//
// Hover sync uses an in-memory pub/sub keyed by [data-ii-ts-group]. Each
// chart subscribes; the source chart broadcasts {x, source}. We avoid
// re-broadcasting from the same chart so no infinite loop.

(function () {
  'use strict';

  if (window.IncidentIntel && window.IncidentIntel.timeseries) return;
  var IncidentIntel = window.IncidentIntel = window.IncidentIntel || {};

  // ----- hover-sync bus -----
  var groups = {}; // groupID -> [subscribers]
  function subscribe(groupID, fn) {
    (groups[groupID] = groups[groupID] || []).push(fn);
    return function unsubscribe() {
      var arr = groups[groupID] || [];
      var i = arr.indexOf(fn);
      if (i >= 0) arr.splice(i, 1);
    };
  }
  function broadcast(groupID, payload) {
    var subs = groups[groupID];
    if (!subs) return;
    for (var i = 0; i < subs.length; i++) {
      try { subs[i](payload); } catch (e) { /* ignore listener errors */ }
    }
  }
  IncidentIntel.timeseries = {
    subscribe: subscribe,
    broadcast: broadcast,
    // Exposed for tests/inspection.
    _groups: groups,
  };

  // ----- main bootstrap -----
  function ready() {
    var nodes = document.querySelectorAll('[data-uplot-config]');
    if (!nodes.length) return; // page has no time-series widgets
    if (typeof uPlot === 'undefined') {
      // uPlot bundle hasn't loaded yet (defer ordering). Try again on load.
      window.addEventListener('load', ready, { once: true });
      return;
    }
    nodes.forEach(function (n) {
      if (n.dataset.uplotInit === '1') return; // already initialized
      var kind = n.getAttribute('data-uplot-kind');
      var cfg;
      try { cfg = JSON.parse(n.getAttribute('data-uplot-config') || '{}'); }
      catch (e) { console.warn('ii-uplot: bad config', e); return; }
      try {
        if (kind === 'sparkline') initSparkline(n, cfg);
        else if (kind === 'timeline') initTimeline(n, cfg);
      } catch (e) {
        console.warn('ii-uplot init failed for', kind, e);
      }
      n.dataset.uplotInit = '1';
    });
  }
  IncidentIntel.timeseries.bootstrap = ready;

  // Run on DOMContentLoaded; htmx swaps re-trigger via afterSwap below.
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', ready);
  } else {
    ready();
  }
  document.body && document.body.addEventListener
    && document.body.addEventListener('htmx:afterSwap', ready);

  // ----- sparkline -----
  function initSparkline(host, cfg) {
    var rect = host.getBoundingClientRect();
    var w = Math.max(120, Math.floor(rect.width || 220));
    var h = 32;
    var data = [cfg.x || [], cfg.y || []];
    if (!data[0].length) {
      host.innerHTML = '<span class="meta">no data</span>';
      return;
    }
    var opts = {
      width: w,
      height: h,
      cursor: { y: false, drag: { x: false, y: false } },
      legend: { show: false },
      scales: { x: { time: true } },
      axes: [{ show: false }, { show: false }],
      series: [
        {},
        { stroke: '#5b9bd5', width: 1.2, points: { show: false } },
      ],
      hooks: {
        setCursor: [function (u) {
          if (u.cursor.left < 0) return;
          var idx = u.cursor.idx;
          if (idx == null) return;
          broadcast(cfg.group_id, { x: data[0][idx], source: host.id });
        }],
      },
    };
    var u = new uPlot(opts, data, host);
    subscribe(cfg.group_id, function (msg) {
      if (msg.source === host.id) return;
      var idx = nearestIdx(data[0], msg.x);
      if (idx < 0) return;
      var left = u.valToPos(data[0][idx], 'x');
      u.setCursor({ left: left, top: h / 2 });
    });
  }

  // ----- event timeline -----
  function initTimeline(host, cfg) {
    var rect = host.getBoundingClientRect();
    var w = Math.max(360, Math.floor(rect.width || 720));
    var h = Math.max(80, 28 * Math.max(1, (cfg.lanes || []).length) + 36);
    var lanes = cfg.lanes || [];
    var events = cfg.events || [];
    if (!events.length) return; // <ol> fallback already rendered

    // Build per-lane series: x=unix array (only events in this lane), y=lane idx.
    var xs = events.map(function (e) { return e.unix; });
    var data = [xs];
    var seriesOpts = [{}]; // x axis placeholder
    var laneColors = ['#5b9bd5', '#e07b00', '#7fa869', '#b07cc6', '#d96c6c', '#3f9c8a', '#a07f4f', '#aaaaaa'];
    lanes.forEach(function (lane, li) {
      var ys = events.map(function (e) { return e.lane === lane.index ? lane.index + 1 : null; });
      data.push(ys);
      seriesOpts.push({
        label: lane.label,
        stroke: laneColors[li % laneColors.length],
        width: 0,
        points: { show: true, size: 6, fill: laneColors[li % laneColors.length] },
      });
    });

    var opts = {
      width: w,
      height: h,
      legend: { show: true },
      scales: {
        x: { time: true, range: cfg.min_unix && cfg.max_unix ? [cfg.min_unix, cfg.max_unix] : null },
        y: { range: [0, lanes.length + 1] },
      },
      axes: [
        {},
        {
          show: true,
          values: function (_u, splits) {
            return splits.map(function (s) {
              var lane = lanes[Math.round(s) - 1];
              return lane ? lane.label : '';
            });
          },
        },
      ],
      series: seriesOpts,
      hooks: {
        setCursor: [function (u) {
          if (u.cursor.left < 0) return;
          var idx = u.cursor.idx;
          if (idx == null) return;
          broadcast(cfg.group_id, { x: xs[idx], source: host.id });
        }],
      },
    };
    var u = new uPlot(opts, data, host);
    subscribe(cfg.group_id, function (msg) {
      if (msg.source === host.id) return;
      var idx = nearestIdx(xs, msg.x);
      if (idx < 0) return;
      u.setCursor({ left: u.valToPos(xs[idx], 'x'), top: h / 2 });
    });
  }

  function nearestIdx(arr, x) {
    if (!arr || !arr.length) return -1;
    var best = 0;
    var bestDiff = Math.abs(arr[0] - x);
    for (var i = 1; i < arr.length; i++) {
      var d = Math.abs(arr[i] - x);
      if (d < bestDiff) { best = i; bestDiff = d; }
    }
    return best;
  }
})();
