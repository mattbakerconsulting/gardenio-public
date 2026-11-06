/* Porch chat (Homestead World 1c, gd-AXfec5afZo; Lands Commons I step 10, gd-EwISaXh6Xk): a gardener's conversation is
   its thread in the Commons, so a Porch row opens the gardener's node in the workbench (/garden/c/n/{id}, web/commons2.js),
   whose transcript is that thread. There is no second conversation view: the viewer's old agent chat page is retired, and
   so is the band that showed the thread inside /garden (gd-ZeiSZV7j2Y). The thread is found by GET
   {base}/garden/c/bead/{agent id} as JSON (internal/commons place.go), on the viewer's own bearer and land cut. A gardener
   bound to no thread opens its own bead in the garden instead (the garden:open-bead event, else /garden?bead=<id>). The
   decisions and escalations it is parked on are answered from the Mailbox. A sprite click on the farm opens the gardener's
   node too (web/garden.js), the same on a desktop and a phone, and its Porch row when it has no thread. */
var CHAT = (function () {
  /* ---------- pure logic (also run under node by the Go tests) ---------- */
  /* agentOf: the agent a Porch row is for. A row mirrors a Ledger item, i-<agent bead id>. */
  function agentOf(src) { src = String(src || ''); return src.indexOf('i-') === 0 ? src.slice(2) : ''; }
  /* placeOf: the pane's {land, channel, thread} from the Commons' place answer, or null when it is not one. */
  function placeOf(p) {
    if (!p || typeof p.land !== 'string' || typeof p.channel !== 'string' || typeof p.thread !== 'string') return null;
    if (!p.land || !p.channel || !p.thread) return null;
    return { land: p.land, channel: p.channel, thread: p.thread };
  }

  var api = { agentOf: agentOf, placeOf: placeOf, open: function () {} };
  if (typeof document === 'undefined') return api;
  var B = document.body, pane = document.getElementById('pane-porch');
  if (!pane || typeof fetch === 'undefined') return api;
  var base = B.dataset.base || '', toastEl = document.getElementById('toast'), toastTimer = 0, seq = 0;

  function toast(msg) {
    if (!toastEl) return;
    toastEl.textContent = msg; if (typeof TITLES !== 'undefined') TITLES.dress(toastEl);   // an id reads as its title (web/titles.js)
    toastEl.hidden = false; clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { toastEl.hidden = true; }, 5000);
  }
  /* openBead: a bead stays in the garden. The in-garden bead view (web/workbench.js) takes it by cancelling the event; with
     nothing listening, the garden's own deep link opens it. */
  function openBead(id) {
    var ev;
    try { ev = new CustomEvent('garden:open-bead', { detail: { id: id }, cancelable: true }); } catch (e) { ev = null; }
    if (ev && !document.dispatchEvent(ev)) return;
    location.href = base + '/garden?bead=' + encodeURIComponent(id);
  }

  /* o.none: what a gardener with no thread opens instead of its bead (a sprite click: its Porch row). */
  function open(id, o) {
    if (!id) return;
    o = o || {};
    var my = ++seq;
    fetch(base + '/garden/c/bead/' + encodeURIComponent(id), { credentials: 'same-origin', redirect: 'manual', cache: 'no-store', headers: { Accept: 'application/json' } })
      .then(function (r) {
        if (r.type === 'opaqueredirect' || r.status === 401) throw new Error('Your session ended. Reload the page to sign in again.');
        if (r.status === 404) return null;
        if (!r.ok) throw new Error('The thread did not load (' + r.status + ').');
        return r.json();
      })
      .then(function (p) {
        if (my !== seq) return;
        if (placeOf(p)) location.assign(base + '/garden/c/n/' + encodeURIComponent(id));
        else if (o.none) o.none();
        else openBead(id);   // no thread in the Commons: the gardener's own bead
      }, function (e) { if (my === seq) toast(e.message || 'The thread did not load.'); });
  }

  document.getElementById('chat-open').onclick = function () {
    var first = document.querySelector('#d-porch > li[data-src]');   // past the plots' headings
    if (first) open(agentOf(first.dataset.src || first.id));
  };
  /* A Porch row opens its agent's thread: the whole row is the tap target, not just its 20 px title (captured, so a
     bead-card handler on .t never sees it). Enter on the focused title clicks it, so the keyboard path is the same. */
  document.addEventListener('click', function (e) {
    var li = e.target.closest ? e.target.closest('#d-porch > li') : null;
    if (!li || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey) return;
    var other = e.target.closest('a,button');
    if (other && !other.classList.contains('t')) return;
    var id = agentOf(li.dataset.src || li.id);
    if (!id) return;
    e.preventDefault(); e.stopPropagation(); open(id);
  }, true);

  api.open = open;
  return api;
})();
if (typeof module !== 'undefined') module.exports = CHAT;
