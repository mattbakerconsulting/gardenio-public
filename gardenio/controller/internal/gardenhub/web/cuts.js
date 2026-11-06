/* A cut text opens in place, never by a link away (gd-Fmq3fgqFTm; rubric C1, gd-r7JiBQdZNS). A cut is a .cm-cut holding
   what is shown (.cm-cut-head), its whole (.cm-cut-whole: hidden in the page, or built on its first open) and one
   button.cm-cut-go whose aria-expanded says which is shown: its press swaps them, and the address and the scroll stay as
   they are. The server draws one with its whole in the page (commons stewardline, node face); CUTS.make draws one here. A
   view marked data-cuts carries one Expand all (CUTS.all), which opens every cut in it at once, and every Markdown fold
   (gardenmd Block's details.md-more, which opens in place by itself), and is then Collapse all. The presses are taken on
   the document, so a cut drawn, cloned or patched in later needs no wiring. */
var CUTS = (function () {
  var lazy = new WeakMap();
  function part(cut, cls) { return [].filter.call(cut.children, function (x) { return x.classList.contains(cls); })[0]; }
  /* cut opened (open) or shut; false when it is no cut */
  function set(cut, open) {
    var go = part(cut, 'cm-cut-go'), head = part(cut, 'cm-cut-head'), whole = part(cut, 'cm-cut-whole');
    if (!go || !head) return false;
    if (!whole && open) {
      var build = lazy.get(cut);
      if (!build) return false;
      whole = build(); whole.classList.add('cm-cut-whole'); lazy.delete(cut);
      cut.insertBefore(whole, go);
    }
    if (!go.dataset.shut) go.dataset.shut = go.textContent;
    head.hidden = open;
    if (whole) whole.hidden = !open;
    go.setAttribute('aria-expanded', open ? 'true' : 'false');
    go.textContent = open ? 'Show less' : go.dataset.shut;
    return true;
  }
  /* The view scope's Expand all, put in host the first time scope has a cut (gone when it has none), its words and
     aria-expanded said from the cuts: Collapse all once none is shut. */
  function all(scope, host) {
    var b = scope.querySelector('.cm-cutall');
    scope.dataset.cuts = '';
    if (!scope.querySelector('.cm-cut, details.md-more')) { if (b) b.remove(); return null; }
    if (!b) { b = document.createElement('button'); b.type = 'button'; b.className = 'btn small ghost cm-cutall'; (host || scope).appendChild(b); }
    sync(scope);
    return b;
  }
  function sync(scope) {
    var b = scope.querySelector('.cm-cutall'), shut = !!scope.querySelector('.cm-cut-go[aria-expanded=false], details.md-more:not([open])');
    if (!b) return;
    b.textContent = shut ? 'Expand all' : 'Collapse all';
    b.setAttribute('aria-expanded', shut ? 'false' : 'true');
  }
  document.addEventListener('click', function (e) {
    var t = e.target && e.target.closest ? e.target.closest('.cm-cut-go, .cm-cutall') : null, scope = t && t.closest('[data-cuts]');
    if (!t) return;
    if (t.classList.contains('cm-cut-go')) set(t.closest('.cm-cut'), t.getAttribute('aria-expanded') !== 'true');
    else if (scope) {
      var open = t.getAttribute('aria-expanded') !== 'true';
      [].forEach.call(scope.querySelectorAll('.cm-cut'), function (c) { set(c, open); });
      [].forEach.call(scope.querySelectorAll('details.md-more'), function (d) { d.open = open; });
    }
    if (scope) sync(scope);
  });
  document.addEventListener('toggle', function (e) {   // a Markdown fold opened or shut by itself (toggle does not bubble)
    var scope = e.target.matches && e.target.matches('details.md-more') && e.target.closest('[data-cuts]');
    if (scope) sync(scope);
  }, true);
  /* A cut drawn on the page: head shown, its whole built by make() on its first open (so a long text is laid out once it is
     asked for), behind a button that says word. */
  function make(head, build, word) {
    var cut = document.createElement('div'), go = document.createElement('button');
    cut.className = 'cm-cut'; head.classList.add('cm-cut-head');
    go.type = 'button'; go.className = 'btn small ghost bv-more cm-cut-go'; go.textContent = word; go.setAttribute('aria-expanded', 'false');
    cut.append(head, go);
    lazy.set(cut, build);
    return cut;
  }
  return { all: all, set: set, make: make };
})();
