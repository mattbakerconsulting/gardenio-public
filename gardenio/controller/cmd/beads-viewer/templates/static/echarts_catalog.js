// echarts_catalog.js — the shared ECharts renderer for the E5 chart catalog
// (E6.1, task ka-0000005063; epic ka-0000000596). One module, reused by all
// three E6 insertion points: the /charts dashboard (E6.2), the per-bead chart
// panels on agent/account/project pages (E6.3), and the /costs chart card
// (E6.4).
//
// Data contract (from the E5 backend, ka-0000001540 — served to the browser via
// the viewer's read-only proxy at <base>/api/v1/charts/{id}):
//
//   GET /api/v1/charts/{id}?since=…&until=…&step=…&<selector params>
//   → { chart_id, title, subtitle, params, data[], spec, ios_hint, meta }
//
// We consume `data` (long-format rows) + `ios_hint`, NOT `spec` (that is the
// Vega-Lite document the iOS app / vl-convert sidecar use). Each data row is
//   { t: "<RFC3339>", <y_field>: <number>, <series_field?>: "<series>" }
// where y_field / series_field come from ios_hint:
//   ios_hint = { kind, x_field, y_field, series_field, y_format, supports_native }
//   kind        ∈ "line" | "line-multi" | "area-stacked" | "bar" | "bar-stacked"
//   y_format    ∈ "currency_usd" | "rate" | "rate_per_hour" | "count" | ""
//   series_field is "" for single-series charts (no grouping).
//
// Every render goes through one of three explicit states — loading, empty,
// error — because the v1 catalog is sparse: an empty series is the common
// case, not an edge case, and an unconfigured VM backend (proxy 503) must read
// as "no data yet" rather than a broken page.
//
// Theming: ECharts has no CSS inheritance, so we read the ambient text colour
// off the container with getComputedStyle and pick a light/dark axis palette
// from the <html data-skin> attribute. This keeps charts legible on every skin
// (baseline / dark / dark-terminal / …) without a per-skin ECharts theme file.

(function () {
  'use strict';

  var BASE = (window.BEADS_VIEWER_BASE_PATH || '').replace(/\/$/, '');

  // Series colour palette — shared with the legacy uPlot panels (charts.js) so
  // the two surfaces look consistent during the migration. Rotated by series
  // index; ECharts also falls back to its own palette past the end.
  var COLORS = ['#5b9bd5', '#e07b00', '#7fa869', '#b07cc6', '#d96c6c', '#3f9c8a', '#a07f4f', '#888888'];

  // darkSkins are the data-skin values that want a dark axis/grid palette. Any
  // other skin (baseline, minimal-refined, docs-notion, tufte, …) gets light.
  var DARK_SKINS = { dark: 1, 'dark-terminal': 1 };

  function isDark() {
    var skin = document.documentElement.getAttribute('data-skin') || '';
    return !!DARK_SKINS[skin];
  }

  // axisPalette returns the colours ECharts can't inherit from CSS: the axis
  // line, split (grid) line, and label colours. Label colour is pulled from the
  // container's computed colour so it tracks the page's text tone; axis/grid
  // are derived as translucent neutrals that read on both light and dark.
  function axisPalette(node) {
    var label = '#444';
    try {
      var c = getComputedStyle(node).color;
      if (c) label = c;
    } catch (e) { /* getComputedStyle can throw on detached nodes */ }
    var dark = isDark();
    return {
      label: label,
      axisLine: dark ? 'rgba(200,200,200,0.35)' : 'rgba(60,60,60,0.35)',
      splitLine: dark ? 'rgba(200,200,200,0.12)' : 'rgba(60,60,60,0.10)',
      tooltipBg: dark ? 'rgba(30,30,30,0.95)' : 'rgba(255,255,255,0.96)',
      tooltipText: dark ? '#eee' : '#222',
    };
  }

  // formatValue turns a numeric y value into a short axis/tooltip string per
  // the chart's y_format. Compact magnitudes (1.2k, 3.4M) keep axis labels
  // narrow; currency prefixes a $; rates suffix the unit.
  function formatValue(v, yFormat) {
    if (v == null || isNaN(v)) return '–';
    var compact = compactNumber(v);
    switch (yFormat) {
      case 'currency_usd': return '$' + compact;
      case 'rate': return compact + '/s';
      case 'rate_per_hour': return '$' + compact + '/h';
      case 'count': return compact;
      default: return compact;
    }
  }

  // tooltipAxisLabel renders the x (time) header line for a multi-axis tooltip.
  // ECharts gives us the category value on axisValueLabel; fall back to a parsed
  // timestamp so the header is never blank.
  function tooltipAxisLabel(item) {
    if (item.axisValueLabel) return item.axisValueLabel;
    var v = (item.value && item.value.length > 1) ? item.value[0] : item.axisValue;
    var d = new Date(v);
    return isNaN(d.getTime()) ? '' : d.toLocaleString();
  }

  function compactNumber(v) {
    var abs = Math.abs(v);
    if (abs >= 1e9) return (v / 1e9).toFixed(2) + 'B';
    if (abs >= 1e6) return (v / 1e6).toFixed(2) + 'M';
    if (abs >= 1e3) return (v / 1e3).toFixed(2) + 'k';
    if (abs >= 1) return v.toFixed(2);
    if (abs === 0) return '0';
    // Sub-unit values (small spend, fractional rates) need more precision.
    return v.toFixed(4);
  }

  // toSeries reshapes the catalog's long-format `data` rows into ECharts series.
  // Single-series charts (ios_hint.series_field === "") yield one series; multi
  // -series charts group rows by the series field. Each point is [tsMillis, y]
  // so ECharts' time axis spaces them by real time, not row index.
  //
  // Overlay series (ios_hint.overlays, the multi-query extension ka-0000006625)
  // live in the SAME `data` array but read a different value field per row (e.g.
  // the active-agent count alongside spend). When overlays are present, each
  // overlay contributes one extra series carrying its y_axis_index/y_format so
  // buildOption can bind it to the secondary axis. Rows lacking an overlay's
  // value field simply don't contribute a point to that series.
  function toSeries(data, hint) {
    var xField = hint.x_field || 't';
    var yField = hint.y_field;
    var seriesField = hint.series_field || '';
    var overlays = hint.overlays || [];
    var groups = {}; // name -> [[tsMillis, y], ...]
    var order = [];  // preserve first-seen series order for stable colours
    var meta = {};   // name -> { yAxisIndex, yFormat } (overlays only)
    // When overlays are present the primary series needs a stable, named legend
    // entry; fall back to the backend-provided primary_name (then "value").
    var primaryName = overlays.length ? (hint.primary_name || 'value') : '';

    function push(name, ts, y) {
      if (!(name in groups)) { groups[name] = []; order.push(name); }
      groups[name].push([ts, isNaN(y) ? null : y]);
    }

    for (var i = 0; i < data.length; i++) {
      var row = data[i];
      var ts = Date.parse(row[xField]);
      if (isNaN(ts)) continue;
      // Primary value field: present on primary rows. With overlays, name the
      // primary series explicitly so the legend distinguishes it from the count.
      if (row[yField] != null) {
        var name = seriesField ? String(row[seriesField] == null ? '' : row[seriesField]) : primaryName;
        push(name, ts, Number(row[yField]));
      }
      // Overlay value fields: each overlay reads its own column from the row.
      for (var k = 0; k < overlays.length; k++) {
        var ov = overlays[k];
        if (row[ov.y_field] == null) continue;
        meta[ov.name] = { yAxisIndex: ov.y_axis_index || 0, yFormat: ov.y_format || '' };
        push(ov.name, ts, Number(row[ov.y_field]));
      }
    }
    return order.map(function (name) {
      // ECharts wants points time-sorted for a smooth line.
      groups[name].sort(function (a, b) { return a[0] - b[0]; });
      var s = { name: name, points: groups[name] };
      if (meta[name]) { s.yAxisIndex = meta[name].yAxisIndex; s.yFormat = meta[name].yFormat; }
      return s;
    });
  }

  // buildOption assembles the ECharts option from a ChartRender payload. The
  // mark (line vs stacked area vs stacked bar) follows ios_hint.kind;
  // multi-series charts get a legend; everything reads colours from the
  // skin-aware palette.
  function buildOption(render, node) {
    var hint = render.ios_hint || {};
    var kind = hint.kind || 'line';
    var yFormat = hint.y_format || '';
    var overlays = hint.overlays || [];
    var hasSecondary = overlays.some(function (o) { return (o.y_axis_index || 0) === 1; });
    var pal = axisPalette(node);
    var series = toSeries(render.data || [], hint);
    var multi = series.length > 1 || (hint.series_field || '') !== '' || overlays.length > 0;
    var isBar = kind === 'bar' || kind === 'bar-stacked';
    // Stack both the area chart and the stacked-bar chart from a common total,
    // so each x bucket's segments sum to that bucket's height (per-agent spend).
    // Overlay charts (line + secondary-axis line) are never stacked — the two
    // series measure different things on different axes.
    var stacked = (kind === 'area-stacked' || kind === 'bar-stacked') && overlays.length === 0;

    // seriesFormat maps each series name to its y_format so the tooltip can
    // format every point in its own units when axes differ (spend in $/h, the
    // overlay count as a bare integer).
    var seriesFormat = {};
    var echSeries = series.map(function (s, idx) {
      var name = s.name || render.title || 'value';
      seriesFormat[name] = s.yFormat || yFormat;
      var spec = {
        name: name,
        type: isBar ? 'bar' : 'line',
        data: s.points,
        showSymbol: false,
        smooth: false,
        lineStyle: { width: 1.5 },
        itemStyle: { color: COLORS[idx % COLORS.length] },
        emphasis: { focus: 'series' },
      };
      // Bind a secondary-axis overlay series (the active-agent count) to the
      // right-hand yAxis; everything else stays on the primary (index 0).
      if (s.yAxisIndex === 1) { spec.yAxisIndex = 1; }
      if (stacked) {
        spec.stack = 'total';
        if (kind === 'area-stacked') {
          spec.areaStyle = { opacity: 0.5 };
        }
      }
      return spec;
    });

    // Build the y-axis (or axes). The primary axis keeps the chart's y_format
    // and title; when an overlay binds to the secondary axis, add a right-hand
    // axis carrying that overlay's own format/title so the count reads in its
    // own units (e.g. "agents") rather than being squashed onto the $/h scale.
    var primaryAxis = {
      type: 'value',
      name: hint.y_title || '',
      nameTextStyle: { color: pal.label, fontSize: 10 },
      axisLine: { show: false },
      axisLabel: {
        color: pal.label,
        fontSize: 10,
        formatter: function (v) { return formatValue(v, yFormat); },
      },
      splitLine: { lineStyle: { color: pal.splitLine } },
    };
    var yAxis = primaryAxis;
    if (hasSecondary) {
      var secOv = overlays.filter(function (o) { return (o.y_axis_index || 0) === 1; })[0] || {};
      var secFormat = secOv.y_format || 'count';
      yAxis = [primaryAxis, {
        type: 'value',
        name: secOv.y_title || 'agents',
        nameTextStyle: { color: pal.label, fontSize: 10 },
        axisLine: { show: false },
        axisLabel: {
          color: pal.label,
          fontSize: 10,
          formatter: function (v) { return formatValue(v, secFormat); },
        },
        // Only the primary axis draws grid lines; a second set would clutter.
        splitLine: { show: false },
      }];
    }

    return {
      animation: false,
      color: COLORS,
      grid: { left: 8, right: hasSecondary ? 16 : 12, top: multi ? 36 : 12, bottom: 8, containLabel: true },
      legend: multi ? {
        type: 'scroll',
        top: 4,
        textStyle: { color: pal.label, fontSize: 11 },
        icon: 'roundRect',
        itemWidth: 12,
        itemHeight: 8,
      } : undefined,
      tooltip: {
        trigger: 'axis',
        backgroundColor: pal.tooltipBg,
        borderWidth: 0,
        textStyle: { color: pal.tooltipText, fontSize: 12 },
        // With a secondary axis the two series carry different units, so format
        // each point by its own series' format (via a per-row formatter) rather
        // than one shared valueFormatter. Single-axis charts keep the simple
        // shared formatter.
        valueFormatter: hasSecondary ? undefined : function (v) { return formatValue(v, yFormat); },
        formatter: hasSecondary ? function (items) {
          if (!items || !items.length) return '';
          var lines = [tooltipAxisLabel(items[0])];
          for (var i = 0; i < items.length; i++) {
            var it = items[i];
            var v = (it.value && it.value.length > 1) ? it.value[1] : it.value;
            lines.push(it.marker + it.seriesName + ': ' + formatValue(v, seriesFormat[it.seriesName] || yFormat));
          }
          return lines.join('<br/>');
        } : undefined,
      },
      xAxis: {
        type: 'time',
        axisLine: { lineStyle: { color: pal.axisLine } },
        axisLabel: { color: pal.label, fontSize: 10, hideOverlap: true },
        splitLine: { show: false },
      },
      yAxis: yAxis,
      series: echSeries,
    };
  }

  // ── explicit state rendering ───────────────────────────────────────────────
  // The three states each replace the node's content with a centred message,
  // disposing any live ECharts instance first so we never leak a chart behind
  // a "loading" overlay.

  function clearChart(node) {
    if (node.__ec) {
      try { node.__ec.dispose(); } catch (e) { /* already disposed */ }
      node.__ec = null;
    }
  }

  function showState(node, cls, text) {
    clearChart(node);
    node.innerHTML = '';
    var span = document.createElement('span');
    span.className = 'meta ' + cls;
    span.textContent = text;
    node.appendChild(span);
  }

  function showLoading(node) { showState(node, 'charts-panel-loading', 'loading…'); }
  function showEmpty(node) { showState(node, 'charts-panel-empty', 'no data yet'); }
  function showError(node, msg) { showState(node, 'charts-panel-error', msg || 'failed to load'); }

  // drawChart (re)creates the ECharts instance for a render. A non-empty render
  // replaces any prior state node; an empty render shows the empty state. The
  // optional onClick maps a data-point click to a drill-down (E6.2 spend slice
  // → /agents/<id>) — the handler receives the clicked series name.
  function drawChart(node, render, onClick) {
    if (typeof window.echarts === 'undefined') {
      showError(node, 'chart engine unavailable');
      return;
    }
    var meta = render.meta || {};
    var empty = meta.empty || !render.data || render.data.length === 0;
    if (empty) { showEmpty(node); return; }

    // ECharts needs a clean element with explicit height. The state spans we
    // injected earlier are not valid chart containers, so reset before init.
    if (!node.__ec) {
      node.innerHTML = '';
      node.__ec = window.echarts.init(node, null, { renderer: 'canvas' });
      if (typeof onClick === 'function') {
        node.__ec.on('click', function (params) {
          if (params && params.seriesName) onClick(params.seriesName, params);
        });
      }
    }
    node.__ec.setOption(buildOption(render, node), { notMerge: true });
    node.__ec.resize();
  }

  // buildURL composes the proxy URL for a chart id + params object. Array values
  // (multi-value selectors like agent=[a,b]) are repeated; null/empty values are
  // dropped so an absent selector doesn't send an empty matcher.
  function buildURL(chartID, params) {
    var qs = [];
    params = params || {};
    Object.keys(params).forEach(function (k) {
      var v = params[k];
      if (v == null || v === '') return;
      if (Array.isArray(v)) {
        v.forEach(function (item) {
          if (item != null && item !== '') qs.push(encodeURIComponent(k) + '=' + encodeURIComponent(item));
        });
      } else {
        qs.push(encodeURIComponent(k) + '=' + encodeURIComponent(v));
      }
    });
    var url = BASE + '/api/v1/charts/' + encodeURIComponent(chartID);
    if (qs.length) url += '?' + qs.join('&');
    return url;
  }

  // fetchAndRender is the public one-shot: show loading, fetch the render via
  // the proxy, then draw or surface the appropriate state. Returns a Promise so
  // callers can sequence (e.g. a dashboard awaiting all panels). A 503 (proxy /
  // VM backend unconfigured) and a 404 (unknown chart) read as honest states,
  // not console errors.
  function fetchAndRender(node, chartID, params, opts) {
    opts = opts || {};
    showLoading(node);
    var url = buildURL(chartID, params);
    return fetch(url, { headers: { Accept: 'application/json' }, credentials: 'same-origin' })
      .then(function (resp) {
        if (resp.status === 503) { showEmpty(node); return null; }
        if (resp.status === 404) { showError(node, 'unknown chart'); return null; }
        if (!resp.ok) { showError(node, 'error ' + resp.status); return null; }
        return resp.json();
      })
      .then(function (render) {
        if (render) drawChart(node, render, opts.onClick);
        return render;
      })
      .catch(function (err) {
        // Network/parse failure. If a chart is already drawn (a transient poll
        // error), leave it; only replace a non-chart node with the error state.
        if (!node.__ec) showError(node, 'failed to load');
        if (window.console && console.debug) console.debug('chart fetch failed', chartID, err);
        return null;
      });
  }

  // Resize all live charts on window resize (debounced) so panels stay crisp
  // through layout changes. One document-level listener refs every instance via
  // the node.__ec back-pointer.
  var resizeTimer = null;
  window.addEventListener('resize', function () {
    if (resizeTimer) clearTimeout(resizeTimer);
    resizeTimer = setTimeout(function () {
      var nodes = document.querySelectorAll('[data-chart-catalog]');
      for (var i = 0; i < nodes.length; i++) {
        if (nodes[i].__ec) { try { nodes[i].__ec.resize(); } catch (e) { /* disposed */ } }
      }
    }, 150);
  });

  // Public surface. Kept small and stable so the three E6 surfaces depend only
  // on these entry points.
  window.BeadsCharts = {
    fetchAndRender: fetchAndRender,
    drawChart: drawChart,
    buildURL: buildURL,
    showLoading: showLoading,
    showEmpty: showEmpty,
    showError: showError,
    isDark: isDark,
    _toSeries: toSeries,        // exported for unit tests
    _formatValue: formatValue,  // exported for unit tests
    _buildOption: buildOption,  // exported for unit tests
  };
})();
