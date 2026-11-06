// e2e-seeded-daemon.mjs — a PERSISTING, hermetic beads daemon for the Phase 3
// task 3-6 (ka-0000006435) closing end-to-end verification.
//
// WHY a NEW daemon and not the 3-3 threads-mock-daemon.mjs. That mock is a
// READ-ONLY fixture server: its write path (POST /v1/beads,
// /v1/beads/{id}/close) is a no-op that returns {} and NEVER persists, so a
// composed message vanishes — fine for the 3-4 optimistic-reconcile visual cell
// (which injects the authoritative bead out-of-band via /__control/inject-
// message), but useless for a LIVE compose->poll->audit proof where the audit
// must actually SEE the bead the composer wrote. This daemon PERSISTS writes:
//   - POST /v1/beads         -> mint a real bead, return {id}, store it.
//   - POST /v1/beads/{id}/close -> flip status=closed, stamp closed_at/closed_by.
//   - POST /v1/beads/{id}       -> best-effort field/status patch (reopen path).
// so the exact bead the viewer's postThreadSayMessage writes is the bead the
// /threads/{id}/audit reconciliation reads back. Everything else mirrors the
// daemon wire contract internal/beadsapi/* depends on (verified against the
// 3-3 mock, which was itself verified against client.go / client_query.go /
// client_comments.go).
//
// SEED. One multi-member thread exercising every membership + closure shape the
// acceptance criteria enumerate, mirroring threads_test.go / threads_audit_test.go
// / threads_audit_3_4_test.go fixtures (addHead / addMemberByField / addChild /
// GetComments), with timestamps PINNED to fixed absolute UTC (not boot-relative):
//   - the wish HEAD (Slack-bound: channel + thread_ts, so the composer can post)
//   - a FIELD-ONLY member  (fields.thread_id, no parent-child dep)
//   - a CLOSURE-ONLY member in the HALF-LINK window (parent-child dep, NO
//     thread_id field — reached only by the reverse-dep BFS)
//   - a CLOSED member with a VALID ClosedAt (bead_closed, not imputed)
//   - a CLOSED member with an EMPTY/unparseable ClosedAt (bead_closed IMPUTED ->
//     audit surfaces an unparseable_closed_at WARNING, never a silent drop)
//   - a pre-seeded >500-char composer MESSAGE (type=message, fields.text) so the
//     C4/K4 long-body gate has a source even before the live compose
//   - comments scattered across the head + members (fused conversation)
// The live compose adds a SECOND >500-char message member at runtime.

import http from "node:http";
import { URL } from "node:url";

const PORT = parseInt(process.env.MOCK_DAEMON_PORT || "18080", 10);
const HEAD = "gd-wishE2E36";

// Pinned absolute UTC instants (RFC3339 — the daemon wire contract; beadsapi
// parseTimestamp accepts RFC3339). Chosen strictly increasing so the fused
// timeline is unambiguously ordered.
const T = {
  headCreated: "2026-06-20T09:00:00Z",
  epicCreated: "2026-06-20T09:30:00Z",
  halfCreated: "2026-06-20T10:00:00Z",
  fieldCreated: "2026-06-20T09:45:00Z",
  closedValidCreated: "2026-06-20T09:15:00Z",
  closedValidClosed: "2026-06-20T12:00:00Z",
  badCloseCreated: "2026-06-20T09:20:00Z",
  seedMsgCreated: "2026-06-20T10:30:00Z",
  c1: "2026-06-20T11:00:00Z",
  c2: "2026-06-20T11:05:00Z",
  c3: "2026-06-20T10:45:00Z",
};

// A >500-char body for the PRE-SEEDED message member (no leading/trailing
// whitespace: threadMessageText TrimSpaces the field by design, so a surrounding
// space would render trimmed and diverge from the untrimmed source).
const SEED_LONG_BODY = ("Seeded composer reply that is deliberately longer than the 500-character " +
  "bead-title cap so the audit's C4/K4 gate proves the full fields[text] renders " +
  "verbatim rather than the truncated Title. ").repeat(4).trim();

// beads: id -> wire JSON. revDeps: parentId -> [{bead_id, depends_on_id, type}].
// comments: id -> [comment]. All mutated in place by the write path.
const beads = {};
const comments = {};
const revDeps = {};

function head(id, title, fields, extra = {}) {
  beads[id] = {
    id, title, kind: "issue", type: "task", status: extra.status || "open",
    assignee: "", priority: 1, labels: ["thread", "project:gardenio"],
    fields: fields || {}, created_at: extra.created_at, updated_at: extra.updated_at || extra.created_at,
    created_by: extra.created_by || "slack:matthew.baker",
    ...(extra.closed_at ? { closed_at: extra.closed_at, closed_by: extra.closed_by } : {}),
  };
  return beads[id];
}

function memberField(id, title, headID, createdAt, extra = {}) {
  beads[id] = {
    id, title, kind: extra.kind || "issue", type: extra.type || "task",
    status: extra.status || "open", assignee: "", priority: 2,
    labels: extra.labels || ["project:gardenio"],
    fields: { thread_id: headID, ...(extra.fields || {}) },
    created_at: createdAt, updated_at: extra.updated_at || createdAt,
    created_by: extra.created_by || "agent:gardenio-worker",
    ...(extra.closed_at !== undefined ? { closed_at: extra.closed_at, closed_by: extra.closed_by } : {}),
  };
  return beads[id];
}

function child(id, title, parentID, headID, createdAt, withField, extra = {}) {
  beads[id] = {
    id, title, kind: "issue", type: extra.type || "task", status: extra.status || "open",
    assignee: "", priority: 2, labels: ["project:gardenio"],
    fields: withField ? { thread_id: headID } : {},
    created_at: createdAt, updated_at: extra.updated_at || createdAt,
    created_by: extra.created_by || "agent:gardenio-worker",
    ...(extra.closed_at !== undefined ? { closed_at: extra.closed_at, closed_by: extra.closed_by } : {}),
  };
  (revDeps[parentID] ||= []).push({ bead_id: id, depends_on_id: parentID, type: "parent-child" });
  return beads[id];
}

// ---- seed ---------------------------------------------------------------
// Head: a Slack-bound wish so the composer's POST /b/{id}/thread-reply resolves
// a (channel, thread_ts) binding and posts (validThreadBinding requires a
// >=2-char channel and a "sec.usec" timestamp).
head(HEAD, "Make the E2E seeded verification thread green", {
  slack_root_text: "Prove the first-class /threads surface end-to-end: compose, poll, audit-PASS, visual-green.",
  slack_thread_channel: "C0AHE7C0E1G",
  slack_thread_ts: "1782600000.000100",
}, { created_at: T.headCreated, created_by: "slack:matthew.baker" });

// Closure epic (fully-linked: parent-child dep AND thread_id field).
child("gd-epicE2E36", "Triage + fix the seeded verification gate", HEAD, HEAD, T.epicCreated, true, { created_by: "agent:gardenio-worker" });
// HALF-LINK closure-only member: parent-child under the epic, NO thread_id field
// (the 1b-3 write window). Reached only by the reverse-dep BFS.
child("gd-rMd20UgxbS", "Stabilise the seeded smoke retry loop (half-link)", "gd-epicE2E36", HEAD, T.halfCreated, false, { created_by: "agent:gardenio-worker" });
// CLOSED member with a VALID ClosedAt (parent-child + field, closed cleanly).
child("gd-g9OfqUaeoF", "Implement the seeded fix and ship it", HEAD, HEAD, T.closedValidCreated, true, {
  status: "closed", closed_at: T.closedValidClosed, closed_by: "gardenio-worker", created_by: "agent:gardenio-worker",
});

// FIELD-ONLY member (fields.thread_id, no parent-child dep) — a cross-epic
// derivative bound to the wish by the pointer only.
memberField("gd-0YttqNEzLP", "Field-only derivative bound by the wish pointer", HEAD, T.fieldCreated, { created_by: "agent:gardenio-worker" });
// CLOSED FIELD-ONLY member with an EMPTY ClosedAt -> bead_closed IMPUTED to
// created_at, audit surfaces an unparseable_closed_at WARNING (C5/K3), NOT a
// silent drop.
memberField("gd-9m0U6OHUFv", "Closed derivative whose closed_at is empty/unparseable", HEAD, T.badCloseCreated, {
  status: "closed", closed_at: "", closed_by: "gardenio-worker", created_by: "agent:gardenio-worker",
});

// PRE-SEEDED >500-char composer MESSAGE (type=message, fields.text). Exactly the
// shape postThreadSayMessage writes: type=message, closed, body in fields.text,
// authored by the operator, bound by thread_id.
memberField("gd-msgSEED36", SEED_LONG_BODY.slice(0, 500), HEAD, T.seedMsgCreated, {
  type: "message", kind: "data", status: "closed",
  labels: ["thread-say", `thread:${HEAD}`],
  fields: { text: SEED_LONG_BODY },
  created_by: "matthew.baker@acme.example",
  closed_at: T.seedMsgCreated, closed_by: "matthew.baker@acme.example",
});

// Conversation: comments on the head + a work member interleave into the fused
// timeline (proving the comment path is undisturbed by messages/closes).
comments[HEAD] = [
  { id: 1, author: "matthew.baker", text: "Kick this off — I want the audit to return a structured PASS.", created_at: T.c1 },
  { id: 2, author: "gardenio-worker", text: "On it. Seeding the multi-member thread and driving the live compose now.", created_at: T.c2 },
];
comments["gd-g9OfqUaeoF"] = [
  { id: 3, author: "gardenio-worker", text: "Shipped the fix; closing this work item.", created_at: T.c3 },
];

// ---- server -------------------------------------------------------------
let idSeq = 0;
function mintID() {
  idSeq++;
  return `gd-live${String(idSeq).padStart(6, "0")}36`;
}

function json(res, code, body) {
  const payload = JSON.stringify(body);
  res.writeHead(code, { "content-type": "application/json", "content-length": Buffer.byteLength(payload) });
  res.end(payload);
}
function notFound(res, msg) { json(res, 404, { error: msg || "not found" }); }
function hasAll(have, want) { return want.every((w) => (have || []).includes(w)); }

function readBody(req) {
  return new Promise((resolve) => {
    let b = "";
    req.on("data", (c) => (b += c));
    req.on("end", () => resolve(b));
  });
}

function listBeads(u, res) {
  const params = u.searchParams;
  const fields = params.getAll("field");
  const threadIDField = fields.map((f) => f.split(/=(.*)/s)).find(([k]) => k === "thread_id");
  if (threadIDField) {
    const wantID = threadIDField[1];
    const out = Object.values(beads).filter((b) => b.fields && b.fields.thread_id === wantID);
    return json(res, 200, { beads: out, total: out.length });
  }
  const labels = (params.get("labels") || "").split(",").filter(Boolean);
  let out = Object.values(beads).filter((b) => hasAll(b.labels, labels));
  if (params.get("sort") === "-updated_at") {
    out = out.slice().sort((a, b) => (a.updated_at < b.updated_at ? 1 : -1));
  }
  return json(res, 200, { beads: out, total: out.length });
}

const server = http.createServer(async (req, res) => {
  const u = new URL(req.url, `http://localhost:${PORT}`);
  const p = u.pathname;

  // --- WRITE path (the whole point of this daemon) -----------------------
  // POST /v1/beads — mint + persist a real bead, return {id}. This is the bead
  // postThreadSayMessage writes for a composed reply.
  if (req.method === "POST" && p === "/v1/beads") {
    const raw = await readBody(req);
    let reqBody = {};
    try { reqBody = JSON.parse(raw || "{}"); } catch { /* tolerate */ }
    const id = mintID();
    // The viewer sends fields as a JSON object (json.RawMessage of a
    // map[string]string). Accept object or stringified object.
    let fields = reqBody.fields || {};
    if (typeof fields === "string") { try { fields = JSON.parse(fields); } catch { fields = {}; } }
    const nowISO = new Date().toISOString();
    beads[id] = {
      id,
      title: reqBody.title || "",
      kind: reqBody.kind || "issue",
      type: reqBody.type || "task",
      status: reqBody.status || "open",
      assignee: reqBody.assignee || "",
      priority: reqBody.priority || 2,
      labels: reqBody.labels || [],
      fields: fields || {},
      created_at: nowISO,
      updated_at: nowISO,
      created_by: reqBody.created_by || "",
    };
    return json(res, 200, { id });
  }

  // POST /v1/beads/{id}/close — flip status=closed, stamp closed_at/closed_by.
  const closeM = p.match(/^\/v1\/beads\/([^/]+)\/close$/);
  if (req.method === "POST" && closeM) {
    const id = decodeURIComponent(closeM[1]);
    const raw = await readBody(req);
    let body = {};
    try { body = JSON.parse(raw || "{}"); } catch { /* tolerate */ }
    const b = beads[id];
    if (b) {
      const nowISO = new Date().toISOString();
      b.status = "closed";
      b.closed_at = nowISO;
      b.closed_by = body.closed_by || b.created_by || "";
      b.updated_at = nowISO;
    }
    return json(res, 200, {});
  }

  // POST/PATCH /v1/beads/{id} — best-effort update (the reopen path uses this).
  const updM = p.match(/^\/v1\/beads\/([^/]+)$/);
  if ((req.method === "POST" || req.method === "PATCH" || req.method === "PUT") && updM) {
    const id = decodeURIComponent(updM[1]);
    const raw = await readBody(req);
    let body = {};
    try { body = JSON.parse(raw || "{}"); } catch { /* tolerate */ }
    const b = beads[id];
    if (b && body.status) { b.status = body.status; b.updated_at = new Date().toISOString(); }
    return json(res, 200, b || {});
  }

  // POST /v1/beads/{id}/comments|labels|state — accept + ignore (not exercised
  // by the thread-reply path, but keep them 200 so nothing 500s mid-run).
  if (req.method === "POST" && /^\/v1\/beads\/[^/]+\/(comments|labels|state)/.test(p)) {
    await readBody(req);
    return json(res, 200, {});
  }

  // --- READ path ---------------------------------------------------------
  if (p === "/v1/health") return json(res, 200, {});

  if (p === "/v1/batch/beads") {
    const ids = (u.searchParams.get("ids") || "").split(",").filter(Boolean);
    const results = ids.map((id) => (beads[id] ? { id, bead: beads[id] } : { id, error: "not found" }));
    return json(res, 200, { results });
  }
  if (p === "/v1/batch/dependencies") {
    const ids = (u.searchParams.get("ids") || "").split(",").filter(Boolean);
    const direction = u.searchParams.get("direction") || "forward";
    const results = ids.map((id) => ({ id, deps: direction === "reverse" ? revDeps[id] || [] : [] }));
    return json(res, 200, { results });
  }

  if (p === "/v1/beads" && req.method === "GET") return listBeads(u, res);

  const m = p.match(/^\/v1\/beads\/([^/]+)(\/(.*))?$/);
  if (m && req.method === "GET") {
    const id = decodeURIComponent(m[1]);
    const sub = m[3] || "";
    if (sub === "") {
      if (!beads[id]) return notFound(res, "bead not found");
      return json(res, 200, beads[id]);
    }
    if (sub === "comments") return json(res, 200, { comments: comments[id] || [] });
    if (sub === "dependencies") return json(res, 200, { dependencies: [] });
    if (sub === "dependents") return json(res, 200, { dependencies: revDeps[id] || [] });
    if (sub === "events") return json(res, 200, { events: [] });
    if (sub === "mentions" || sub === "mentioned-by") return json(res, 200, { references: [] });
    if (sub === "similar") return json(res, 200, { source_id: id, results: [] });
    if (sub === "followers") return json(res, 200, { followers: [] });
    if (sub === "operator-history") return json(res, 200, { history: [] });
    return json(res, 200, {});
  }

  if (req.method === "GET") return json(res, 200, {});
  await readBody(req);
  json(res, 200, {});
});

server.listen(PORT, "127.0.0.1", () => {
  console.error(`[e2e-seeded-daemon] listening on 127.0.0.1:${PORT}`);
  console.error(`[e2e-seeded-daemon] head=${HEAD}  members(field∪closure) seeded; write path persists`);
  console.error(`[e2e-seeded-daemon] seed long-body length=${SEED_LONG_BODY.length} (>500)`);
});
for (const sig of ["SIGINT", "SIGTERM"]) process.on(sig, () => server.close(() => process.exit(0)));
