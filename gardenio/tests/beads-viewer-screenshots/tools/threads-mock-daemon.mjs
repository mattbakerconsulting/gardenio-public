// threads-mock-daemon.mjs — a hermetic, in-memory beads daemon that serves
// EXACTLY the HTTP surface beads-viewer's /threads index, /threads/{id} detail,
// and /{id}?bead=1 bead-show pages call. Task 3-3 (ka-0000004063).
//
// WHY a mock daemon (not a port-forward to the live cluster). The capture
// matrix must be DETERMINISTIC and REPRODUCIBLE in CI with no Postgres, no
// Keycloak, and no live data drift: pinned timestamps, a fixed set of wishes in
// every lifecycle state, and the ability to FORCE the index/timeline error
// states on demand. A port-forwarded prod daemon gives none of those. The mock
// implements the daemon wire contract that internal/beadsapi/* depends on
// (verified against client.go / client_query.go / client_comments.go /
// client_events.go / client_search.go / client_followers.go), so the REAL
// beads-viewer binary runs unmodified against it.
//
// Endpoints implemented (all GET unless noted):
//   /v1/health                              -> {}
//   /v1/beads?labels=thread,...&field=...   -> {beads:[...], total}
//   /v1/beads/{id}                          -> beadJSON (404 when absent)
//   /v1/beads/{id}/comments                 -> {comments:[...]}
//   /v1/beads/{id}/dependencies             -> {dependencies:[]}  (forward)
//   /v1/beads/{id}/dependents               -> {dependencies:[...]} (reverse)
//   /v1/beads/{id}/events                   -> {events:[]}
//   /v1/beads/{id}/mentions|mentioned-by    -> {references:[]}
//   /v1/beads/{id}/similar                  -> {results:[]}
//   /v1/beads/{id}/followers                -> {followers:[]}
//   /v1/batch/beads?ids=a,b                 -> {results:[{id,bead}]}
//   /v1/batch/dependencies?ids=&direction=  -> {results:[{id,deps}]}
//   POST /__control/fail {mode}             -> sets the forced-failure mode
//
// Forced-failure modes (set via POST /__control/fail before navigation):
//   "none"      — normal.
//   "list"      — /v1/beads label enumeration 500s (drives the INDEX error
//                 banner: handleThreads -> threadList -> ListBeadsFiltered err).
//   "timeline"  — the membership/timeline fetches 500 AFTER the head resolves
//                 (drives the DETAIL error banner: handleThreadDetail keeps the
//                 head, fuseThreadTimeline errors -> "Failed to load thread
//                 timeline."). The head GET still succeeds so the page renders.
//
// The fail mode is process-global and reset by the capture driver between cells.

import http from "node:http";
import { URL } from "node:url";
import { buildFixtures, PROJECT, EMPTY_PROJECT, IDS } from "./threads-fixtures.mjs";

// Default :18080, NOT :8080 — an agent pod typically already runs a real beads
// daemon on :8080, and the mock must never collide with or shadow it.
const PORT = parseInt(process.env.MOCK_DAEMON_PORT || "18080", 10);

// Freeze "now" to the top of the current hour so the fixed hour/day offsets in
// threads-fixtures land the humanizer on clean boundaries.
const nowMs = Math.floor(Date.now() / 3600_000) * 3600_000;
const { beads, comments, revDeps } = buildFixtures(nowMs);

let failMode = "none";

function json(res, code, body) {
  const payload = JSON.stringify(body);
  res.writeHead(code, { "content-type": "application/json", "content-length": Buffer.byteLength(payload) });
  res.end(payload);
}

function notFound(res, msg) {
  json(res, 404, { error: msg || "not found" });
}

function serverError(res, msg) {
  json(res, 500, { error: msg || "forced failure (mock daemon)" });
}

// labelMatch: bead carries ALL requested labels.
function hasAllLabels(have, want) {
  return want.every((w) => (have || []).includes(w));
}

// listBeads answers the two query shapes threadList + threadMembers issue:
//   - label-scoped head enumeration (labels contains "thread"), and
//   - a thread_id field-equality query (?field=thread_id=<headID>).
// Plus the inbox-count queries (categories/labelInboxWorthy) — which return
// empty since dev-bypass has no resolver/operator id, but we answer safely.
function listBeads(u, res) {
  const params = u.searchParams;
  // field=thread_id=<id> membership query.
  const fields = params.getAll("field");
  const threadIDField = fields.map((f) => f.split(/=(.*)/s)).find(([k]) => k === "thread_id");
  if (threadIDField) {
    const wantID = threadIDField[1];
    const out = Object.values(beads).filter((b) => b.fields && b.fields.thread_id === wantID);
    return json(res, 200, { beads: out, total: out.length });
  }
  // label-scoped enumeration.
  const labels = (params.get("labels") || "").split(",").filter(Boolean);
  if (labels.includes("thread")) {
    // The INDEX list path. Forced "list" failure 500s here.
    if (failMode === "list") return serverError(res, "forced list failure");
    let out = Object.values(beads).filter((b) => hasAllLabels(b.labels, labels));
    // sort=-updated_at
    if (params.get("sort") === "-updated_at") {
      out = out.slice().sort((a, b) => (a.updated_at < b.updated_at ? 1 : -1));
    }
    return json(res, 200, { beads: out, total: out.length });
  }
  // Any other list query (inbox counts, agent-authored messages, etc.) — empty.
  return json(res, 200, { beads: [], total: 0 });
}

const server = http.createServer((req, res) => {
  const u = new URL(req.url, `http://localhost:${PORT}`);
  const p = u.pathname;

  // --- control plane -------------------------------------------------------
  if (req.method === "POST" && p === "/__control/fail") {
    let body = "";
    req.on("data", (c) => (body += c));
    req.on("end", () => {
      try {
        failMode = (JSON.parse(body || "{}").mode) || "none";
      } catch {
        failMode = "none";
      }
      json(res, 200, { mode: failMode });
    });
    return;
  }

  // POST /__control/reset-injected
  //
  // Removes every bead the harness injected via /__control/inject-message,
  // restoring the fixture set to its boot state. The capture driver calls this
  // before each composer-reconciled cell so the previous cell's injected
  // message doesn't bleed into the next — the V10 "zero duplicate" check on the
  // next cell would otherwise count both the prior + the fresh server-rendered
  // message in the conversation lane and fail.
  if (req.method === "POST" && p === "/__control/reset-injected") {
    let removed = 0;
    for (const id of Object.keys(beads)) {
      if (id.startsWith("gd-msgINJECT")) {
        delete beads[id];
        removed++;
      }
    }
    return json(res, 200, { removed });
  }

  // POST /__control/inject-message {head, text, author, ts_offset_h?}
  //
  // Injects a closed type=message bead bound to `head` — exactly the shape
  // postThreadSayMessage writes. The capture harness uses this to simulate the
  // server-side bridge-forwarded message echo for the V10 optimistic-send +
  // ?since= reconcile proof (ka-0000003162 3-4): paint the composer's
  // optimistic bubble (the JS path), inject the authoritative message bead via
  // this endpoint, trigger the next timeline poll, and verify the bubble
  // reconciles with NO duplicate after the server-rendered message arrives.
  //
  // The injected bead participates in the same threadMembers/timeline path the
  // real composer flow would: the thread_id field-equality query picks it up,
  // the type=message branch in fuseThreadTimeline emits one message entry, the
  // composer-body audit gate sees the same fields[text]. So the visual capture
  // is exercising the real rendering path, not a fake.
  if (req.method === "POST" && p === "/__control/inject-message") {
    let body = "";
    req.on("data", (c) => (body += c));
    req.on("end", () => {
      let req2;
      try {
        req2 = JSON.parse(body || "{}");
      } catch {
        return json(res, 400, { error: "bad JSON" });
      }
      const head = String(req2.head || "");
      const text = String(req2.text || "");
      const author = String(req2.author || "matt@example.com");
      if (!head || !text) return json(res, 400, { error: "head + text required" });
      // Offset (hours back from frozen now) so the message lands at a
      // predictable timeline position; default to 0 (the latest entry).
      const off = typeof req2.ts_offset_h === "number" ? req2.ts_offset_h : 0;
      const at = new Date(nowMs - off * 3600_000).toISOString();
      const msgID = `gd-msgINJECT${Object.keys(beads).filter((k) => k.startsWith("gd-msgINJECT")).length + 1}`;
      beads[msgID] = {
        id: msgID,
        title: text.length > 500 ? text.slice(0, 500) : text,
        kind: "data",
        type: "message",
        status: "closed",
        assignee: "",
        priority: 2,
        labels: ["thread-say", `thread:${head}`],
        fields: { thread_id: head, text },
        created_at: at,
        updated_at: at,
        closed_at: at,
        closed_by: author,
        created_by: author,
      };
      json(res, 200, { id: msgID, at });
    });
    return;
  }

  // The composer write path (POST /v1/beads, POST /v1/beads/{id}/state/closed,
  // POST /v1/beads/{id}/comments) — the viewer issues these on a real composer
  // send. The mock answers 200 with an empty body so the viewer's handler
  // does not error mid-capture; the AUTHORITATIVE message bead lands via the
  // /__control/inject-message control endpoint above, which is what the next
  // ?since= poll picks up. Without this passthrough the viewer's CreateBead
  // would parse an empty `id` and CloseBeadAs would 200 anyway, but the
  // intercept here makes the intent legible.
  if (req.method === "POST" && (p === "/v1/beads" || p.match(/^\/v1\/beads\/[^/]+\/(state|comments|labels)/))) {
    let body = "";
    req.on("data", (c) => (body += c));
    req.on("end", () => json(res, 200, {}));
    return;
  }

  if (p === "/v1/health") return json(res, 200, {});

  // --- batch endpoints (checked before the single-bead {id} routes) --------
  if (p === "/v1/batch/beads") {
    if (failMode === "timeline") return serverError(res, "forced timeline failure (batch beads)");
    const ids = (u.searchParams.get("ids") || "").split(",").filter(Boolean);
    const results = ids.map((id) => (beads[id] ? { id, bead: beads[id] } : { id, error: "not found" }));
    return json(res, 200, { results });
  }
  if (p === "/v1/batch/dependencies") {
    if (failMode === "timeline") return serverError(res, "forced timeline failure (batch deps)");
    const ids = (u.searchParams.get("ids") || "").split(",").filter(Boolean);
    const direction = u.searchParams.get("direction") || "forward";
    const results = ids.map((id) => ({
      id,
      deps: direction === "reverse" ? revDeps[id] || [] : [],
    }));
    return json(res, 200, { results });
  }

  // --- /v1/beads list ------------------------------------------------------
  if (p === "/v1/beads" && req.method === "GET") return listBeads(u, res);

  // --- /v1/beads/{id}/... --------------------------------------------------
  const m = p.match(/^\/v1\/beads\/([^/]+)(\/(.*))?$/);
  if (m && req.method === "GET") {
    const id = decodeURIComponent(m[1]);
    const sub = m[3] || "";
    if (sub === "") {
      // single bead fetch.
      if (!beads[id]) return notFound(res, "bead not found");
      return json(res, 200, beads[id]);
    }
    if (sub === "comments") {
      // The detail timeline pulls comments per member. Forced "timeline"
      // failure 500s here so fuseThreadTimeline errors after the head resolved.
      if (failMode === "timeline") return serverError(res, "forced timeline failure (comments)");
      return json(res, 200, { comments: comments[id] || [] });
    }
    if (sub === "dependencies") return json(res, 200, { dependencies: [] });
    if (sub === "dependents") {
      if (failMode === "timeline") return serverError(res, "forced timeline failure (dependents)");
      return json(res, 200, { dependencies: revDeps[id] || [] });
    }
    if (sub === "events") return json(res, 200, { events: [] });
    if (sub === "mentions" || sub === "mentioned-by") return json(res, 200, { references: [] });
    if (sub === "similar") return json(res, 200, { source_id: id, results: [] });
    if (sub === "followers") return json(res, 200, { followers: [] });
    if (sub === "operator-history") return json(res, 200, { history: [] });
    // Unknown sub-resource: empty object keeps best-effort handlers happy.
    return json(res, 200, {});
  }

  // Anything else the viewer might probe (operators, configs, …): empty 200 so
  // no page 500s on an unmocked best-effort call.
  if (req.method === "GET") return json(res, 200, {});
  json(res, 200, {});
});

server.listen(PORT, "127.0.0.1", () => {
  // eslint-disable-next-line no-console
  console.error(`[mock-daemon] listening on 127.0.0.1:${PORT}  (frozen now=${new Date(nowMs).toISOString()})`);
  console.error(`[mock-daemon] heads: ${Object.values(beads).filter((b) => (b.labels || []).includes("thread")).map((b) => b.id).join(", ")}`);
});

// Clean shutdown so the orchestrator can kill us.
for (const sig of ["SIGINT", "SIGTERM"]) process.on(sig, () => server.close(() => process.exit(0)));
