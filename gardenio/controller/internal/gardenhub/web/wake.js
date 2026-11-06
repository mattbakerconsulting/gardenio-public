/* Wake or restart a gardener (gd-3xrmzPMA1Y, Homestead Phase 3 step 3c), its words: a sleeping or down gardener offers
   "Wake" or "Restart", asks once ("Restart <agent>? It
   will resume its task <title>."), then POST /garden/gardener/<id>/wake (wake.go), the same revive `gb wake` does, stamped
   with the viewer's own operator identity. The
   next farm snapshot shows the gardener starting; a refusal is said in place. A stale gardener (its pod up, no progress for an
   hour) is offered Restart too, and asked first that its session is interrupted: the route has the controller restart it in
   place, in the same pod, with its conversation and its task (gd-LUmZlr6CKM). The page offers it only to a viewer the
   server says may restart gardeners (body data-wake); the route checks again. Text goes in through textContent only.
   The workbench's agent head draws its lines under the gardener's reason (web/commons2.js wake, node.html's lazy
   page-wake.js; gd-7DyHdnx8fC). */
var WAKE = (function () {
  /* ---------- pure logic (also run under node by the Go tests) ---------- */
  var VERB = { sleeping: 'Wake', down: 'Restart', stale: 'Restart' };
  /* The verb a gardener's reason offers, or "": a sleeping gardener is woken, a down or stale one restarted. A working,
     parked or starting gardener offers neither (it is at work, its letter is answered in the Mailbox, or it is on its way). */
  function verb(g) { var r = g && g.reason; return (r && VERB[r.kind]) || ''; }
  function stale(g) { return !!(g && g.reason && g.reason.kind === 'stale'); }
  /* The question the card asks before it posts. A stale gardener is still running, so it is told its session is cut. */
  function ask(g) {
    var v = verb(g), who = g.agent || g.id, t = g.task_title || g.task || '';
    if (stale(g)) return v + ' ' + who + '? Its session is interrupted and starts again in the same pod, ' + (t ? 'resuming its task ' + t + '.' : 'resuming where it left off.');
    return v + ' ' + who + '? ' + (t ? 'It will resume its task ' + t + '.' : 'It will resume where it left off.');
  }
  /* What to tell the viewer after the route answers with status and its JSON body (null when it sent none). */
  var WHY = { 401: 'Your session ended. Reload the page to sign in again.', 403: 'You are not allowed to restart gardeners.', 404: 'That gardener is gone.', 503: 'Restarting is not enabled here.' };
  function outcome(status, body, g) {
    body = body || {};
    if (status === 202 && body.ok) return { ok: true, text: 'Asked. The controller has not confirmed the restart yet; the card shows it once it is back.' };
    if (status === 200 && body.ok && body.in_place) return { ok: true, text: 'Restarted. Its session started again in the same pod.' };
    if (status === 200 && body.ok) return { ok: true, text: (verb(g) === 'Wake' ? 'Woken' : 'Restarted') + '. It shows as starting on the next update.' };
    return { ok: false, text: body.error || WHY[status] || 'That did not go through (' + status + ').' };
  }
  /* The card's lines for gardener g in state s (this module's step for it, or undefined): the action as a button in the links
     row, then, once pressed, the question with Yes and Cancel, the wait, or what the route said. [] when g offers no verb. */
  function lines(g, s) {
    var v = verb(g);
    if (!v && !(s && s.text)) return [];
    if (s && s.step === 'ask') return [['wk', ask(g)], ['wk', [{ text: v, cls: 'btn gwake-yes', id: g.id, btn: 1 }, { text: 'Cancel', cls: 'btn gwake-no', id: g.id, btn: 1 }]]];
    if (s && s.step === 'busy') return [['wk', (v === 'Wake' ? 'Waking ' : 'Restarting ') + (g.agent || g.id) + '\u2026']];
    var out = [];
    if (s && s.text) out.push([s.ok ? 'wk' : 'wk err', s.text]);
    if (v && !(s && s.ok)) out.push(['wk', [{ text: v + '\u2026', cls: 'btn gwake', id: g.id, btn: 1 }]]);
    return out;
  }

  return { VERB: VERB, verb: verb, ask: ask, outcome: outcome, lines: lines, on: function () { return false; } };
})();
if (typeof module !== 'undefined') module.exports = WAKE;
