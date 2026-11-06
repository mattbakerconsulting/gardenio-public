// constellation.js — interactive behaviors for the bead constellation SVG.
//
// Two responsibilities:
//
//  1. Persist the "hide untyped mentions" toggle in localStorage so a user
//     who finds untyped mention edges noisy can dismiss them across
//     sessions. Flips the `data-hide-untyped-mentions` attribute on every
//     `figure.constellation` on the page; CSS in layout.html drives the
//     actual hide via `figure.constellation[data-hide-untyped-mentions="1"]
//     line[data-origin="mention"]`.
//
//  2. Edge click handling (E2 — ka-0000000608): clicking a typed-mention
//     line (`<line data-origin="text">`) navigates to the source bead's
//     page with `?highlight=<target>&rel=<verb>` so the highlight script
//     (highlight.js) can scroll/flash the inline ka-XXX. Untyped mention
//     edges (`data-origin="mention"`) navigate without `rel`. Dep edges
//     (`data-origin="dep"`) and similarity edges keep their current no-op
//     behavior — the deps live in the deps section, no inline anchor to
//     highlight.
//
// htmx may swap a fresh constellation onto the page (e.g. on activity-feed
// updates), so handlers re-bind after each `htmx:afterSwap` event in
// addition to DOMContentLoaded.
(function () {
  'use strict';
  var STORAGE_KEY = 'constellation:hide-untyped-mentions';

  function readPref() {
    try {
      return window.localStorage.getItem(STORAGE_KEY) === '1';
    } catch (_) {
      return false;
    }
  }

  function writePref(hide) {
    try {
      if (hide) {
        window.localStorage.setItem(STORAGE_KEY, '1');
      } else {
        window.localStorage.removeItem(STORAGE_KEY);
      }
    } catch (_) {
      /* private mode etc. — silently no-op */
    }
  }

  function apply(root) {
    var hide = readPref();
    var figures = (root || document).querySelectorAll('figure.constellation');
    figures.forEach(function (fig) {
      if (hide) {
        fig.setAttribute('data-hide-untyped-mentions', '1');
      } else {
        fig.removeAttribute('data-hide-untyped-mentions');
      }
      var box = fig.querySelector('.constellation-hide-untyped');
      if (box) {
        box.checked = hide;
      }
    });
  }

  function bindToggles(root) {
    var boxes = (root || document).querySelectorAll('.constellation-hide-untyped');
    boxes.forEach(function (box) {
      if (box.dataset.constellationBound === '1') {
        return;
      }
      box.dataset.constellationBound = '1';
      box.addEventListener('change', function () {
        writePref(box.checked);
        apply(document);
      });
    });
  }

  function basePath() {
    if (typeof window.BEADS_VIEWER_BASE_PATH === 'string') {
      return window.BEADS_VIEWER_BASE_PATH;
    }
    return '';
  }

  function navigateForEdge(line) {
    var origin = line.getAttribute('data-origin') || '';
    if (origin !== 'text' && origin !== 'mention') {
      return false;
    }
    var source = line.getAttribute('data-source-bead') || '';
    var target = line.getAttribute('data-target-bead') || '';
    if (!source || !target) {
      return false;
    }
    var rel = line.getAttribute('data-relationship-type') || '';
    var url = basePath() + '/' + encodeURIComponent(source) +
      '?highlight=' + encodeURIComponent(target);
    if (rel) {
      url += '&rel=' + encodeURIComponent(rel);
    }
    window.location.href = url;
    return true;
  }

  function bindEdges(root) {
    // The actual click target is the <g class="constellation-edges">
    // wrapper — single delegated listener avoids per-line bind overhead and
    // survives htmx swaps within the wrapper itself.
    var groups = (root || document).querySelectorAll('.constellation-edges');
    groups.forEach(function (g) {
      if (g.dataset.constellationEdgesBound === '1') {
        return;
      }
      g.dataset.constellationEdgesBound = '1';
      g.addEventListener('click', function (evt) {
        var line = evt.target && evt.target.closest && evt.target.closest('line');
        if (!line) {
          return;
        }
        if (navigateForEdge(line)) {
          evt.preventDefault();
        }
      });
      // Help users discover that text-origin edges are clickable.
      var clickable = g.querySelectorAll('line[data-origin="text"], line[data-origin="mention"]');
      clickable.forEach(function (line) {
        line.style.cursor = 'pointer';
      });
    });
  }

  function bindTreeLevel(root) {
    var inputs = (root || document).querySelectorAll('.constellation-tree-level-input');
    inputs.forEach(function (input) {
      if (input.dataset.constellationBound === '1') {
        return;
      }
      input.dataset.constellationBound = '1';
      var label = input.parentElement && input.parentElement.querySelector('.constellation-tree-level-value');
      input.addEventListener('input', function () {
        if (label) {
          label.textContent = input.value;
        }
      });
      input.addEventListener('change', function () {
        var url = new URL(window.location.href);
        if (input.value === '1') {
          url.searchParams.delete('tree');
        } else {
          url.searchParams.set('tree', input.value);
        }
        window.location.href = url.toString();
      });
    });
  }

  // bindShowClosedAgents wires the "show closed agents" checkbox to the
  // ?show_closed_agents=1 URL param. Closed agent/session beads are hidden
  // by default (server filters in buildConstellation, ka-0000005724); the
  // toggle round-trips through the URL so the server can re-render with
  // the closed nodes restored.
  function bindShowClosedAgents(root) {
    var boxes = (root || document).querySelectorAll('.constellation-show-closed-agents');
    boxes.forEach(function (box) {
      if (box.dataset.constellationBound === '1') {
        return;
      }
      box.dataset.constellationBound = '1';
      box.addEventListener('change', function () {
        var url = new URL(window.location.href);
        if (box.checked) {
          url.searchParams.set('show_closed_agents', '1');
        } else {
          url.searchParams.delete('show_closed_agents');
        }
        window.location.href = url.toString();
      });
    });
  }

  // --- Hover highlight (ka-0000007349) -----------------------------------
  //
  // The constellation SVG is server-rendered with FIXED baked x/y coords
  // (not a live force sim), so we highlight by *dimming in place* — toggling
  // CSS-driven `data-focus-out` attributes — never by removing nodes from
  // the DOM or re-laying out. Positions are preserved by construction.
  //
  //  HOVER over a node: highlight its incident edges + immediate neighbors,
  //    dim everything else. Clears on mouseout. Instant, no prune.
  //  CLICK a node: navigate to that bead's page — the node is a plain <a>
  //    anchor, so we let the browser follow it (no preventDefault). Edge
  //    clicks keep their navigate behavior (bindEdges).
  //
  // The earlier staged click-to-focus subtree prune was removed (a click now
  // simply navigates); only the non-destructive hover highlight remains.
  var graphCache = new WeakMap();   // figure -> { adj, nodeEls, edges }

  function nodeIDFromHref(href) {
    var m = (href || '').match(/\/([A-Za-z0-9-]+)(?:[?#].*)?$/);
    return m ? m[1] : '';
  }

  // groupID resolves the bead id for a node/center <g>. Center groups have
  // no anchor href of their own that points elsewhere, so fall back to the
  // figure's data-bead-id.
  function groupID(fig, grp) {
    if (grp.classList.contains('constellation-center')) {
      return fig.getAttribute('data-bead-id') || '';
    }
    var a = grp.querySelector('a[href]');
    return a ? nodeIDFromHref(a.getAttribute('href')) : '';
  }

  // buildGraph (re)computes the undirected adjacency map from the edge
  // lines plus a map of bead-id -> [element] for every node/center group.
  function buildGraph(fig) {
    var adj = new Map();
    var nodeEls = new Map();

    function addNode(id, el) {
      if (!id) {
        return;
      }
      var arr = nodeEls.get(id);
      if (arr) {
        arr.push(el);
      } else {
        nodeEls.set(id, [el]);
      }
    }
    function addAdj(a, b) {
      var set = adj.get(a);
      if (!set) {
        set = new Set();
        adj.set(a, set);
      }
      set.add(b);
    }

    var center = fig.querySelector('.constellation-center');
    if (center) {
      addNode(fig.getAttribute('data-bead-id') || '', center);
    }
    fig.querySelectorAll('.constellation-node').forEach(function (g) {
      var a = g.querySelector('a[href]');
      addNode(a ? nodeIDFromHref(a.getAttribute('href')) : '', g);
    });

    var edges = [];
    fig.querySelectorAll('.constellation-edges line').forEach(function (line) {
      var s = line.getAttribute('data-source-bead');
      var t = line.getAttribute('data-target-bead');
      if (!s || !t) {
        return;
      }
      edges.push(line);
      addAdj(s, t);
      addAdj(t, s);
    });

    return { adj: adj, nodeEls: nodeEls, edges: edges };
  }

  function graphFor(fig) {
    var g = graphCache.get(fig);
    if (!g) {
      g = buildGraph(fig);
      graphCache.set(fig, g);
    }
    return g;
  }

  function mark(el, inSet) {
    if (inSet) {
      el.removeAttribute('data-focus-out');
    } else {
      el.setAttribute('data-focus-out', '1');
    }
  }

  // clearMarks strips all focus attributes so the figure renders the full
  // graph again. Cheap enough to run unconditionally.
  function clearMarks(fig) {
    fig.querySelectorAll('[data-focus-out]').forEach(function (el) {
      el.removeAttribute('data-focus-out');
    });
    fig.removeAttribute('data-focus-mode');
  }

  // applyHover dims everything outside the hovered node's immediate
  // neighborhood.
  function applyHover(fig, hoverID) {
    if (!hoverID) {
      clearMarks(fig);
      return;
    }
    var g = graphFor(fig);
    var inSet = new Set(g.adj.get(hoverID) || []);
    inSet.add(hoverID);
    fig.setAttribute('data-focus-mode', 'hover');
    g.nodeEls.forEach(function (els, id) {
      var inn = inSet.has(id);
      els.forEach(function (el) { mark(el, inn); });
    });
    g.edges.forEach(function (line) {
      var s = line.getAttribute('data-source-bead');
      var t = line.getAttribute('data-target-bead');
      mark(line, s === hoverID || t === hoverID);
    });
  }

  // bindFocus wires the hover-only highlight onto the bead-page constellation.
  // A plain node click is NOT intercepted — each node is a real <a href>, so
  // the browser navigates to the clicked bead's page natively (ka-0000007349
  // staged click-to-subtree prune was removed; a click now simply navigates).
  function bindFocus(root) {
    // Scope to the bead-page constellation only — the gardener map
    // (figure.cap-constellation) owns its own pan/zoom + shift-click
    // re-root in maps.js, so we must not hijack interactions there.
    var figs = (root || document).querySelectorAll('figure.constellation:not(.cap-constellation)');
    figs.forEach(function (fig) {
      if (fig.dataset.constellationFocusBound === '1') {
        return;
      }
      fig.dataset.constellationFocusBound = '1';
      var svg = fig.querySelector('.constellation-svg');
      if (!svg) {
        return;
      }

      svg.addEventListener('mouseover', function (evt) {
        var grp = evt.target.closest && evt.target.closest('.constellation-node, .constellation-center');
        if (!grp) {
          return;
        }
        var id = groupID(fig, grp);
        if (id) {
          applyHover(fig, id);
        }
      });
      svg.addEventListener('mouseout', function (evt) {
        var grp = evt.target.closest && evt.target.closest('.constellation-node, .constellation-center');
        if (!grp) {
          return;
        }
        // Ignore moves that stay within the same group (child elements).
        var to = evt.relatedTarget;
        if (to && grp.contains(to)) {
          return;
        }
        applyHover(fig, null);
      });
    });
  }

  // bindMapsHover wires a HOVER-ONLY edge highlight onto the gardener map
  // (figure.cap-constellation), which bindFocus deliberately excludes because
  // maps.js owns pan/zoom + shift-click server re-root there. We reuse the same
  // applyHover dim-in-place logic but bind ONLY mouseover/mouseout — no click
  // listener — so the map's own click semantics (open bead, shift-click re-root)
  // are untouched (bug ka-0000000364). While a pan drag is in flight maps.js
  // flags the figure with data-map-dragging; we skip re-applying the hover dim
  // then so it doesn't flicker as the cursor sweeps nodes during a drag.
  function bindMapsHover(root) {
    // Scope to the /maps stage only — the gardener page also renders a
    // figure.cap-constellation (via gardener_beads), but the hover-dim CSS is
    // .maps-stage-scoped, so toggling attributes there would be invisible churn.
    var figs = (root || document).querySelectorAll('.maps-stage figure.cap-constellation');
    figs.forEach(function (fig) {
      if (fig.dataset.constellationMapsHoverBound === '1') {
        return;
      }
      fig.dataset.constellationMapsHoverBound = '1';
      var svg = fig.querySelector('.constellation-svg');
      if (!svg) {
        return;
      }
      svg.addEventListener('mouseover', function (evt) {
        if (fig.hasAttribute('data-map-dragging')) {
          return; // pan in progress — don't fight the drag with hover dim
        }
        var grp = evt.target.closest && evt.target.closest('.constellation-node, .constellation-center');
        if (!grp) {
          return;
        }
        var id = groupID(fig, grp);
        if (id) {
          applyHover(fig, id);
        }
      });
      svg.addEventListener('mouseout', function (evt) {
        var grp = evt.target.closest && evt.target.closest('.constellation-node, .constellation-center');
        if (!grp) {
          return;
        }
        // Ignore moves that stay within the same group (child elements).
        var to = evt.relatedTarget;
        if (to && grp.contains(to)) {
          return;
        }
        applyHover(fig, null);
      });
    });
  }

  function init(root) {
    apply(root);
    bindToggles(root);
    bindEdges(root);
    bindTreeLevel(root);
    bindShowClosedAgents(root);
    bindFocus(root);
    bindMapsHover(root);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', function () { init(document); });
  } else {
    init(document);
  }
  document.addEventListener('htmx:afterSwap', function (evt) {
    var target = (evt && evt.detail && evt.detail.target) || document;
    init(target);
  });
})();
