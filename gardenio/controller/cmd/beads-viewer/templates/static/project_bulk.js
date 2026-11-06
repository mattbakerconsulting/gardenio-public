// project_bulk.js — multi-select state for any page that opts in via
// [data-bulk-scope]. Originally written for /p/<project>; generalized in
// ka-0000003995 so /triggers (and any future list view) can reuse the
// same selection model.
//
// Per-scope convention. A container declares its scope via
//   <div data-bulk-scope="project"> … </div>   (or "triggers", etc.)
// and the rows inside use class ".{scope}-row-select" for the checkbox.
// The bulk action bar lives at id="{scope}-bulk-bar".
//
// Selection model: each scope tracks its own selection. Selecting rows in
// one scope does not affect another. Click outside the scope clears the
// shift-extend anchor for that scope only.
//
// Keyboard:
//   Shift+Click extends from the anchor through the click target
//   (inclusive). Plain click toggles a single row.
//   Esc clears the selection in the active scope (the scope whose row
//   was last clicked), provided focus is not inside an editable element.
//
// Confirmation:
//   A form on the bar may declare data-bulk-confirm-over="N" together
//   with data-bulk-confirm-verb="<verb>" and (optionally)
//   data-bulk-confirm-noun="<noun>". When the current selection exceeds
//   N, htmx:confirm is intercepted and window.confirm() ("<Verb> <n>
//   <noun>?") must be accepted before the request goes out. The noun
//   defaults to "items" so a scope can opt into the threshold without
//   declaring one.
//
// Reset-after-bulk:
//   When the server returns the cleared bar via htmx swap, all rows are
//   refreshed via OOB swap — their fresh checkboxes are unchecked, so
//   selection state resets naturally. We additionally clear any
//   .is-selected highlight on htmx:afterSwap to be safe.

(function () {
  'use strict';

  // Per-scope last-click anchor for shift-extend.
  var anchors = {};

  function scopeNames() {
    return Array.prototype.slice.call(
      document.querySelectorAll('[data-bulk-scope]')
    ).map(function (el) {
      return el.getAttribute('data-bulk-scope');
    });
  }

  function scopeOfNode(node) {
    if (!node || !node.closest) return null;
    var el = node.closest('[data-bulk-scope]');
    if (!el) return null;
    return el.getAttribute('data-bulk-scope');
  }

  function bar(scope) {
    return document.getElementById(scope + '-bulk-bar');
  }

  function checkboxesIn(scope) {
    var container = document.querySelector('[data-bulk-scope="' + scope + '"]');
    if (!container) return [];
    return Array.prototype.slice.call(
      container.querySelectorAll('.' + scope + '-row-select')
    );
  }

  function checkedIn(scope) {
    var container = document.querySelector('[data-bulk-scope="' + scope + '"]');
    if (!container) return [];
    return Array.prototype.slice.call(
      container.querySelectorAll('.' + scope + '-row-select:checked')
    );
  }

  function refresh(scope) {
    var b = bar(scope);
    if (!b) return;
    var checked = checkedIn(scope);
    var n = checked.length;
    var counter = b.querySelector('[data-bulk-count]');
    if (counter) counter.textContent = n + ' selected';
    if (n > 0) {
      b.removeAttribute('hidden');
    } else {
      b.setAttribute('hidden', '');
    }
    checkboxesIn(scope).forEach(function (cb) {
      var row = cb.closest('.bead-row, .trig-row, .plan-card');
      if (!row) return;
      if (cb.checked) row.classList.add('is-selected');
      else row.classList.remove('is-selected');
    });
    refreshSelectAllButton(scope);
  }

  function refreshAll() {
    scopeNames().forEach(refresh);
  }

  function clearAll(scope) {
    var changed = false;
    checkboxesIn(scope).forEach(function (cb) {
      if (cb.checked) {
        cb.checked = false;
        changed = true;
      }
    });
    anchors[scope] = null;
    if (changed) refresh(scope);
    return changed;
  }

  function selectAll(scope) {
    var changed = false;
    checkboxesIn(scope).forEach(function (cb) {
      if (!cb.checked) {
        cb.checked = true;
        changed = true;
      }
    });
    if (changed) refresh(scope);
    return changed;
  }

  function refreshSelectAllButton(scope) {
    var container = document.querySelector('[data-bulk-scope="' + scope + '"]');
    if (!container) return;
    var btn = container.querySelector('[data-bulk-select-all]') ||
              document.querySelector('[data-bulk-select-all][data-bulk-scope="' + scope + '"]');
    if (!btn) return;
    var boxes = checkboxesIn(scope);
    var total = boxes.length;
    var checkedCount = checkedIn(scope).length;
    var allChecked = total > 0 && checkedCount === total;
    btn.textContent = allChecked ? 'Deselect all' : 'Select all';
    btn.setAttribute('aria-pressed', allChecked ? 'true' : 'false');
    if (total === 0) {
      btn.setAttribute('disabled', '');
    } else {
      btn.removeAttribute('disabled');
    }
  }

  function isRowSelectCheckbox(t) {
    if (!t || !t.classList) return null;
    var classes = t.className.split(/\s+/);
    for (var i = 0; i < classes.length; i++) {
      var m = /^(.+)-row-select$/.exec(classes[i]);
      if (m && scopeOfNode(t) === m[1]) return m[1];
    }
    return null;
  }

  document.addEventListener('click', function (e) {
    var t = e.target;
    if (!t) return;
    // Clear button on a scoped bar.
    var clearBtn = t.closest && t.closest('[data-bulk-clear]');
    if (clearBtn) {
      var clearScope = scopeOfNode(clearBtn) || scopeOfNode(clearBtn.closest('[id$="-bulk-bar"]'));
      if (!clearScope) {
        var clearBar = clearBtn.closest('[id$="-bulk-bar"]');
        if (clearBar) {
          var idm = /^(.+)-bulk-bar$/.exec(clearBar.id);
          if (idm) clearScope = idm[1];
        }
      }
      if (clearScope) {
        e.preventDefault();
        clearAll(clearScope);
      }
      return;
    }
    // Select-all toggle inside a scope.
    var selBtn = t.closest && t.closest('[data-bulk-select-all]');
    if (selBtn) {
      var selScope = scopeOfNode(selBtn);
      if (selScope) {
        e.preventDefault();
        var boxes = checkboxesIn(selScope);
        var checkedCount = checkedIn(selScope).length;
        if (boxes.length > 0 && checkedCount === boxes.length) {
          clearAll(selScope);
        } else {
          selectAll(selScope);
        }
      }
      return;
    }
    var scope = isRowSelectCheckbox(t);
    if (!scope) return;
    if (e.shiftKey && anchors[scope] && anchors[scope] !== t) {
      var boxes2 = checkboxesIn(scope);
      var i1 = boxes2.indexOf(anchors[scope]);
      var i2 = boxes2.indexOf(t);
      if (i1 >= 0 && i2 >= 0) {
        var lo = Math.min(i1, i2);
        var hi = Math.max(i1, i2);
        var target = t.checked;
        for (var i = lo; i <= hi; i++) {
          boxes2[i].checked = target;
        }
      }
    }
    anchors[scope] = t;
    refresh(scope);
  });

  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Escape') return;
    if (e.defaultPrevented) return;
    var ae = document.activeElement;
    if (ae && ae.matches && ae.matches('input, textarea, select, [contenteditable="true"]')) {
      return;
    }
    var cleared = false;
    scopeNames().forEach(function (scope) {
      if (checkedIn(scope).length > 0) {
        if (clearAll(scope)) cleared = true;
      }
    });
    if (cleared) e.preventDefault();
  });

  // Confirmation gate. Triggered before htmx issues a request; if the form
  // declares data-bulk-confirm-over with a numeric threshold and the
  // selection in the form's scope exceeds it, window.confirm() must
  // succeed before the request goes out.
  document.body.addEventListener('htmx:confirm', function (e) {
    var elt = e.detail && e.detail.elt;
    if (!elt || !elt.getAttribute) return;
    var overStr = elt.getAttribute('data-bulk-confirm-over');
    if (!overStr) return;
    var over = parseInt(overStr, 10);
    if (!(over > 0)) return;
    var scope = scopeOfNode(elt);
    if (!scope) return;
    var n = checkedIn(scope).length;
    if (n <= over) return;
    var verb = elt.getAttribute('data-bulk-confirm-verb') || 'apply';
    // Noun is scope-declared (data-bulk-confirm-noun) so the same confirm gate
    // reads naturally on every list — "5 triggers", "5 plan gardeners", etc. —
    // instead of hardcoding one page's noun. Defaults to "items" for any scope
    // that opts into the threshold without declaring a noun.
    var noun = elt.getAttribute('data-bulk-confirm-noun') || 'items';
    e.preventDefault();
    var msg = verb.charAt(0).toUpperCase() + verb.slice(1) + ' ' + n + ' ' + noun + '?';
    if (window.confirm(msg)) {
      e.detail.issueRequest(true);
    }
  });

  document.body.addEventListener('htmx:afterSwap', function () {
    refreshAll();
  });

  document.addEventListener('DOMContentLoaded', refreshAll);
})();
