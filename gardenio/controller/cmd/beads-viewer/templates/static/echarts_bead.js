// echarts_bead.js — the fixed-window catalog-chart driver for the /costs page
// chart card (E6.4, ka-0000005703; epic ka-0000000596): catalog charts over a
// fixed 24h window with no range bar.
//
// (The per-bead chart panels on agent/account/project pages formerly used this
// driver too; they swapped to fleet-wide Grafana embeds in task ka-0000007859,
// so /costs is now the only remaining consumer.)
//
// Each [data-chart-catalog] node carries a catalog chart id (data-chart-id) and,
// optionally, a single resolved selector (data-chart-sel-key / data-chart-sel-val)
// scoping the chart to an entity — e.g. agent=gardenio-foo-123, project=gardenio,
// model=claude-opus. The cost card carries no selector (fleet-wide). This script
// fetches each panel through the shared renderer (window.BeadsCharts from
// echarts_catalog.js) over a fixed 24h window. Unlike the /charts dashboard
// there is no range bar and no polling: these are point-in-time views, and the
// catalog render is ETag-cacheable if the operator refreshes.
//
// /charts now embeds Grafana directly (no ECharts driver). The /costs page,
// however, still drives its catalog panels with its own range bar; to avoid two
// drivers fighting over the same nodes, this script bails when a range bar is
// present on the page.

(function () {
  'use strict';

  var FIXED_WINDOW_SECS = 24 * 3600;
  var FIXED_STEP_SECS = 900; // ~96 points over 24h

  function ready() {
    if (!window.BeadsCharts) {
      window.addEventListener('load', ready, { once: true });
      return;
    }
    // The dashboard's range bar owns its panels; don't double-drive them.
    if (document.querySelector('[data-charts-rangebar]')) return;

    var nodes = document.querySelectorAll('[data-chart-catalog]');
    if (!nodes.length) return;

    var end = Math.floor(Date.now() / 1000);
    var start = end - FIXED_WINDOW_SECS;
    var base = {
      since: new Date(start * 1000).toISOString(),
      until: new Date(end * 1000).toISOString(),
      step: String(FIXED_STEP_SECS),
    };

    nodes.forEach(function (node) {
      if (node.dataset.beadInit === '1') return;
      node.dataset.beadInit = '1';
      var chartID = node.getAttribute('data-chart-id') || '';
      var selKey = node.getAttribute('data-chart-sel-key') || '';
      var selVal = node.getAttribute('data-chart-sel-val') || '';
      var params = Object.assign({}, base);
      if (selKey && selVal) params[selKey] = selVal;
      window.BeadsCharts.fetchAndRender(node, chartID, params);
    });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', ready);
  } else {
    ready();
  }
})();
