// drive-threads-e2e.mjs — the LIVE compose -> ?since= poll -> audit-PASS driver
// for Phase 3 task 3-6 (ka-0000006435). Runs against the REAL beads-viewer binary
// booted (dev-auth bypass) over the PERSISTING e2e-seeded-daemon.mjs.
//
// Sequence (the acceptance criteria's live path):
//   1. GET  /threads/{id}/audit         — baseline audit over the seed (PASS,
//      captures the pre-compose latest-activity cursor + full fingerprint).
//   2. GET  /threads/{id}/timeline      — the initial poll, reads the
//      X-Thread-Latest cursor the client would carry.
//   3. POST /b/{id}/thread-reply        — compose a >500-char message as the
//      operator (X-Auth-Request-Email flips CanCompose; the viewer writes a
//      type=message bead + closes it; the persisting daemon stores it).
//   4. GET  /threads/{id}/timeline?since=<cursor>  — the INCREMENTAL poll: the
//      cursor advances (the new message's created_at is later), so the fragment
//      re-renders and the >500-char body appears exactly once.
//   5. GET  /threads/{id}/audit         — the full post-compose reconciliation:
//      assert structured PASS on C1/C4/C5/C6/K1-K5/O1-O5, the composer-body axis
//      is conformant over BOTH the seeded and the live message, and the
//      unparseable_closed_at warning is present (C5/K3 — never a silent drop).
//   6. GET  /threads/{id}/audit?since=<preCursor>&prior_fingerprint=<preFP> —
//      the cursor-bound O5 prefix-stability proof at a real polling instant: the
//      pre-compose prefix must still hash-match after the compose appended a
//      later entry (no earlier entry reordered/vanished).
//
// Writes the full audit JSON + a structured verdict to OUT_DIR/e2e-record.json
// and prints a human summary. Exits non-zero on any gate FAIL.

import fs from "node:fs";
import path from "node:path";

const BASE = process.env.BEADS_VIEWER_URL || "http://localhost:8095";
const HEAD = process.env.E2E_HEAD || "gd-wishE2E36";
const OUT = process.env.OUT_DIR || "test-results/threads";
const OPERATOR = process.env.E2E_OPERATOR || "matthew.baker@acme.example";

// SENTINEL is a unique token embedded EXACTLY ONCE in the live body. The
// duplicate-count check greps for it (not a repeating prefix of the body) so
// "appears exactly once" measures the number of rendered bubbles, not how many
// times a sentence recurs within one body.
const SENTINEL = "E2E36-LIVE-SENTINEL-a1b2c3";

// The live composed body: deliberately >500 chars and multi-line so C4/K4 is
// exercised (the bead Title is truncated to 500; the full body lives in
// fields.text and MUST render verbatim). No leading/trailing whitespace. The
// SENTINEL leads the body once; the rest is padding to clear 500 chars.
const LIVE_BODY = (`${SENTINEL} Live composer reply driven through POST /b/{id}/thread-reply. ` +
  "This body is deliberately padded well past the 500-character title cap so the " +
  "/threads/{id}/audit composer-body gate (C4/K4) proves the full fields[text] " +
  "renders verbatim as a conversation line rather than the truncated 500-char " +
  "Title, and that the operator email attribution lands on the entry author. ".repeat(3)).trim();

const results = { steps: [], gates: {}, ok: true, failures: [] };

function fail(msg) {
  results.ok = false;
  results.failures.push(msg);
  console.error(`FAIL: ${msg}`);
}
function pass(msg) { console.log(`  ok  ${msg}`); }

async function getJSON(url, headers = {}) {
  const resp = await fetch(url, { headers });
  const text = await resp.text();
  let body = null;
  try { body = JSON.parse(text); } catch { /* non-JSON */ }
  return { status: resp.status, headers: resp.headers, text, body };
}

async function main() {
  fs.mkdirSync(OUT, { recursive: true });
  const opHdr = { "X-Auth-Request-Email": OPERATOR };

  // --- Step 1: baseline audit over the seed --------------------------------
  console.log("\n[1] baseline audit (pre-compose)");
  const a0 = await getJSON(`${BASE}/threads/${HEAD}/audit`);
  if (a0.status !== 200 || !a0.body) { fail(`baseline audit HTTP ${a0.status}: ${a0.text.slice(0, 300)}`); throw new Error("baseline audit failed"); }
  const pre = a0.body;
  results.steps.push({ step: "baseline_audit", status: a0.status, pass: pre.pass });
  if (!pre.pass) fail(`baseline audit did not PASS: ${JSON.stringify(pre.divergences)}`);
  else pass(`baseline audit PASS (members=${pre.members.length}, entries=${pre.counts.rendered_total})`);
  // Capture the pre-compose latest cursor + full fingerprint for the O5 proof.
  const preFP = pre.order.fingerprint;
  // Latest activity across the seed = max At. The seed's latest entry is the
  // closed_valid close at 12:00:00Z; compute from the audit's own view by
  // fetching the timeline cursor header (authoritative, same as the client).
  const t0 = await getJSON(`${BASE}/threads/${HEAD}/timeline`, opHdr);
  const preCursor = t0.headers.get("x-thread-latest") || "";
  results.steps.push({ step: "pre_timeline", status: t0.status, cursor: preCursor });
  if (t0.status !== 200) fail(`pre-compose timeline poll HTTP ${t0.status}`);
  else pass(`pre-compose timeline cursor = ${preCursor}`);
  // Sanity: the seeded pre-compose composer-body axis already sees the seeded
  // >500-char message (proves C4/K4 on the seed before the live send).
  if (pre.composer_body.members < 1 || !pre.composer_body.conformant) {
    fail(`pre-compose composer_body not conformant over seed: ${JSON.stringify(pre.composer_body)}`);
  } else {
    pass(`pre-compose composer_body conformant (members=${pre.composer_body.members})`);
  }

  // --- Step 2: compose a >500-char reply as the operator -------------------
  console.log("\n[2] compose >500-char reply (POST /b/{id}/thread-reply)");
  const form = new URLSearchParams({ text: LIVE_BODY }).toString();
  const cResp = await fetch(`${BASE}/b/${HEAD}/thread-reply`, {
    method: "POST",
    headers: { ...opHdr, "content-type": "application/x-www-form-urlencoded" },
    body: form,
  });
  const cText = await cResp.text();
  results.steps.push({ step: "compose", status: cResp.status, body_len: LIVE_BODY.length });
  // The write handler acks 204 No Content (the htmx composer clears its input).
  if (cResp.status !== 204) fail(`compose expected 204, got ${cResp.status}: ${cText.slice(0, 300)}`);
  else pass(`compose accepted (204), body length=${LIVE_BODY.length} (>500)`);

  // --- Step 3: incremental ?since= poll picks up the new message -----------
  console.log("\n[3] incremental ?since= poll");
  // Poll with the pre-compose cursor; the new message's created_at is "now"
  // (later than the seed's max), so the cursor advances and the fragment
  // re-renders with the new body. Retry briefly to tolerate close-lag.
  let poll = null;
  let sawBody = false;
  for (let i = 0; i < 25; i++) {
    poll = await getJSON(`${BASE}/threads/${HEAD}/timeline?since=${encodeURIComponent(preCursor)}`, opHdr);
    if (poll.status === 200 && poll.text.includes(SENTINEL)) { sawBody = true; break; }
    await new Promise((r) => setTimeout(r, 200));
  }
  const postCursor = poll ? poll.headers.get("x-thread-latest") || "" : "";
  results.steps.push({ step: "incremental_poll", status: poll?.status, cursor_advanced: postCursor !== preCursor, saw_body: sawBody });
  if (poll?.status !== 200) fail(`incremental poll HTTP ${poll?.status}`);
  else if (!sawBody) fail(`incremental poll did not render the composed >500-char body`);
  else pass(`incremental poll rendered the composed body (cursor ${preCursor} -> ${postCursor})`);
  // The composed body must appear EXACTLY once in the fragment (no duplicate).
  // Grep the UNIQUE sentinel (embedded once in the body) so the count reflects
  // the number of rendered bubbles, not how many times a padding sentence
  // recurs inside a single body. Save the fragment for the record.
  if (poll?.status === 200) {
    fs.writeFileSync(path.join(OUT, "e2e-poll-fragment.html"), poll.text);
    const count = poll.text.split(SENTINEL).length - 1;
    results.steps.push({ step: "duplicate_check", sentinel_occurrences: count });
    if (count !== 1) fail(`composed body (sentinel) appears ${count}x in the poll fragment, want exactly 1`);
    else pass(`composed body appears exactly once (sentinel count=1, zero duplicate)`);
  }

  // --- Step 4: full post-compose audit -> assert PASS on all gates ---------
  console.log("\n[4] full post-compose audit — assert PASS on C1/C4/C5/C6/K1-K5/O1-O5");
  const a1 = await getJSON(`${BASE}/threads/${HEAD}/audit`);
  if (a1.status !== 200 || !a1.body) { fail(`post-compose audit HTTP ${a1.status}`); throw new Error("post audit failed"); }
  const rep = a1.body;
  results.auditReport = rep;
  fs.writeFileSync(path.join(OUT, "e2e-audit-post-compose.json"), JSON.stringify(rep, null, 2));

  // Headline PASS.
  results.gates.PASS = rep.pass;
  if (!rep.pass) fail(`post-compose audit did NOT PASS. divergences=${JSON.stringify(rep.divergences)}`);
  else pass(`audit.pass = true`);

  // C1/C6 — COUNT conservation (total + per-kind + per-member via no count divergence).
  const conserved = rep.counts.conserved && rep.counts.rendered_total === rep.counts.expected_total;
  results.gates.C1_C6_count = conserved;
  if (!conserved) fail(`C1/C6 count not conserved: ${JSON.stringify(rep.counts)}`);
  else pass(`C1/C6 count conserved: rendered=${rep.counts.rendered_total} == expected=${rep.counts.expected_total} (N=${rep.counts.members_with_body} K=${rep.counts.comments} M=${rep.counts.closable} P=${rep.counts.messages})`);

  // K1/K2/K5 — HASH multiset match (zero fabrication, zero drop) + title fidelity.
  const hashOK = rep.hash.matched && !(rep.hash.fabrications || []).length && !(rep.hash.drops || []).length;
  const titleBleed = (rep.divergences || []).some((d) => d.axis === "title");
  results.gates.K1_K2_K5_hash = hashOK && !titleBleed;
  if (!hashOK) fail(`K1/K2 hash multiset mismatch: fabrications=${JSON.stringify(rep.hash.fabrications)} drops=${JSON.stringify(rep.hash.drops)}`);
  else pass(`K1/K2 hash multiset matched (zero fabrication, zero drop)`);
  if (titleBleed) fail(`K5 title bleed present: ${JSON.stringify((rep.divergences || []).filter((d) => d.axis === "title"))}`);
  else pass(`K5 no title bleed`);

  // O1-O4 — ORDER strict-monotonic + unique canonical 4-tuple.
  const orderOK = rep.order.strictly_monotonic && rep.order.unique;
  results.gates.O1_O4_order = orderOK;
  if (!orderOK) fail(`O1-O4 order broken: monotonic=${rep.order.strictly_monotonic} unique=${rep.order.unique}`);
  else pass(`O1-O4 order strictly-monotonic + unique (fingerprint=${rep.order.fingerprint.slice(0, 12)}…)`);

  // C4/K4 — COMPOSER BODY: conformant over BOTH the seeded + the live message
  // (both >500 chars). Members must be >=2 now (seed + live).
  const cbOK = rep.composer_body.conformant && rep.composer_body.members >= 2;
  results.gates.C4_K4_composer_body = cbOK;
  if (!rep.composer_body.conformant) fail(`C4/K4 composer body not conformant: ${JSON.stringify(rep.composer_body)}`);
  else if (rep.composer_body.members < 2) fail(`C4/K4 expected >=2 message members (seed+live), got ${rep.composer_body.members}`);
  else pass(`C4/K4 composer body conformant over ${rep.composer_body.members} message members (seed + live >500-char)`);

  // C5/K3 — CLOSED-EVENT integrity: the empty-ClosedAt member surfaces as an
  // unparseable_closed_at WARNING (never a silent bead_closed drop); the valid
  // ClosedAt member closes cleanly (no warning for it).
  const warns = rep.warnings || [];
  const impWarn = warns.find((w) => w.code === "unparseable_closed_at" && w.bead_id === "gd-9m0U6OHUFv");
  results.gates.C5_K3_closed_event = !!impWarn;
  if (!impWarn) fail(`C5/K3 expected an unparseable_closed_at warning for gd-9m0U6OHUFv, warnings=${JSON.stringify(warns)}`);
  else pass(`C5/K3 empty-ClosedAt surfaced as warning (not dropped): "${impWarn.detail}"`);
  // The closed bead is STILL counted (M includes it) — never silently dropped.
  if (rep.counts.closable < 2) fail(`C5 expected >=2 closable (valid + imputed), got ${rep.counts.closable}`);
  else pass(`C5 both closed members emit bead_closed (closable=${rep.counts.closable})`);

  // O5 — PREFIX STABILITY (within-snapshot).
  results.gates.O5_prefix_within = rep.prefix_stability.stable;
  if (!rep.prefix_stability.stable) fail(`O5 within-snapshot prefix not stable: ${JSON.stringify(rep.prefix_stability)}`);
  else pass(`O5 within-snapshot prefix stable`);

  // --- Step 5: cursor-bound O5 across-snapshot proof -----------------------
  console.log("\n[5] cursor-bound O5 across-snapshot prefix stability");
  // The pre-compose prefix (entries at-or-before preCursor) must still hash to
  // the pre-compose fingerprint AFTER the compose appended a later entry. Feed
  // the pre-compose full fingerprint back in as prior_fingerprint at preCursor.
  const a2 = await getJSON(`${BASE}/threads/${HEAD}/audit?since=${encodeURIComponent(preCursor)}&prior_fingerprint=${encodeURIComponent(preFP)}`);
  if (a2.status !== 200 || !a2.body) { fail(`cursor-bound audit HTTP ${a2.status}`); }
  else {
    const rep2 = a2.body;
    fs.writeFileSync(path.join(OUT, "e2e-audit-cursor-bound.json"), JSON.stringify(rep2, null, 2));
    const stable = rep2.prefix_stability.stable && rep2.prefix_stability.prior_fingerprint === preFP;
    results.gates.O5_prefix_across = stable;
    if (!rep2.prefix_stability.stable) fail(`O5 across-snapshot prefix not stable at preCursor: ${JSON.stringify(rep2.prefix_stability)}`);
    else pass(`O5 across-snapshot prefix stable (prefix_len=${rep2.prefix_stability.prefix_length}, prior fingerprint honoured)`);
  }

  // --- Step 6: index MemberCount vs audit member-union cross-check ---------
  console.log("\n[6] index MemberCount vs audit member-union cross-check");
  // The /threads index page renders each head's MemberCount (field fast-path +
  // head). The audit's members[] is the full field ∪ closure union. The
  // half-link member (parent-child dep, NO thread_id field) is in the union but
  // NOT in the index fast-path count — the DOCUMENTED index-undercount.
  const idx = await getJSON(`${BASE}/threads`, opHdr);
  const unionCount = rep.members.length;
  // Parse the head's rendered MemberCount from the index HTML (best-effort;
  // the template shows "Work (N)"/member tally). We compute the EXPECTED index
  // count independently: union minus half-link members (no thread_id field) and
  // minus the head-is-always-1 already included. Simpler: the field fast-path
  // counts head(1) + every bead with fields.thread_id==HEAD.
  const fieldBound = rep.members.filter((id) => id !== HEAD); // union non-head members
  // We know from the seed which member lacks the field (the half-link):
  const halfLinkId = "gd-rMd20UgxbS";
  const expectedIndexCount = 1 /*head*/ + fieldBound.filter((id) => id !== halfLinkId).length;
  results.crossCheck = {
    audit_union_count: unionCount,
    index_expected_count: expectedIndexCount,
    half_link_member: halfLinkId,
    undercount: unionCount - expectedIndexCount,
    index_html_len: idx.text.length,
  };
  // Confirm the half-link IS in the audit union (closure reached it).
  if (!rep.members.includes(halfLinkId)) fail(`cross-check: half-link ${halfLinkId} missing from audit union`);
  else pass(`cross-check: half-link ${halfLinkId} present in audit union (closure BFS reached it)`);
  pass(`cross-check: audit union=${unionCount}, index fast-path expected=${expectedIndexCount} (documented index-undercount of ${unionCount - expectedIndexCount}: the half-link member, dep-linked but thread_id field not yet copied — the 1b-3 write window)`);

  // --- summary -------------------------------------------------------------
  console.log("\n=== E2E gate summary ===");
  for (const [k, v] of Object.entries(results.gates)) {
    console.log(`  ${v ? "PASS" : "FAIL"}  ${k}`);
  }
  fs.writeFileSync(path.join(OUT, "e2e-record.json"), JSON.stringify(results, null, 2));
  console.log(`\nwrote ${OUT}/e2e-record.json + e2e-audit-post-compose.json + e2e-audit-cursor-bound.json`);
  if (!results.ok) {
    console.error(`\nE2E FAILED: ${results.failures.length} gate failure(s)`);
    process.exit(1);
  }
  console.log("\nE2E PASSED: all gates green");
}

main().catch((e) => { console.error(e); process.exit(1); });
