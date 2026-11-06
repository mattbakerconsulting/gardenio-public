// threads-fixtures.mjs — the hermetic fixture dataset for the /threads visual
// capture matrix (task 3-3, ka-0000004063, epic ka-0000002477 / ka-0000000679).
//
// These fixtures mirror the shapes that threads_test.go exercises against the
// real aggregators (addHead / addMemberByField / addChild / GetComments):
//
//   - one OPEN multi-message wish with cross-epic work and a long conversation,
//   - one CLOSED / fulfilled wish (a granted wish, work done),
//   - one NEEDS-TRIAGE wish (a failed agent left it unfulfilled),
//   - a LONG-THREAD wish (many replies + many spawned beads) for scroll/sticky,
//   - a member bead carrying fields.thread_id (the part-of-wish chip resolves),
//   - a member bead pointing at a MISSING head (the chip's unresolved fallback),
//   - an EMPTY project scope (no heads at all),
//   - a head bead (carries NO thread_id field — the chip must be ABSENT).
//
// DETERMINISM. humanizeTime (beads-viewer server.go) is `time.Since`-relative,
// so a literal pinned wall-clock would humanize differently on every run. We
// instead pin every timestamp as a FIXED OFFSET from the daemon's boot instant
// (computed once in the mock daemon). The offsets are constant, so the rendered
// "N hours/days ago" strings are byte-stable across runs even though the
// absolute clock advances. All offsets are whole hours/days so the humanizer
// lands on a clean boundary ("3 days ago", never "2 days ago" one run / "3 days
// ago" the next) — every offset below is a multiple of an hour and the daemon
// freezes "now" to the top of the current hour before applying them.
//
// Every timestamp the daemon serializes is RFC3339 UTC, matching the daemon
// wire contract (beadsapi parseTimestamp accepts RFC3339).

// PROJECT is the project label scope the populated views capture against.
export const PROJECT = "gardenio";
// EMPTY_PROJECT is a project with zero thread heads — the index empty state.
export const EMPTY_PROJECT = "emptyland";

// Fixed offsets (in hours) back from the daemon's frozen "now". Chosen so the
// humanizer lands on stable, legible boundaries.
const H = (h) => ({ hours: h });
const D = (d) => ({ hours: d * 24 });

// --- IDs ---------------------------------------------------------------------
export const IDS = {
  openWish: "gd-0netj9sn9P",
  openEpic: "gd-JmtshmVyJE",
  openTask: "gd-SnLdwtjhWU", // half-link: dep present, thread_id field absent
  openMsg: "gd-FvegUIM4Am", // cross-epic field-only member (a reply bead)
  closedWish: "gd-6P3BWjN3Lm",
  closedTask: "gd-fsEtMt8vtM",
  triageWish: "gd-HSdfoSeixY",
  triageAgent: "gd-aOpCNrkkPa",
  longWish: "gd-xXhwE8B2mM",
  emptyWish: "gd-uvwwymdTju", // a minted wish with zero replies + zero work
  memberChip: "gd-xVUiQWGdYt", // bead with fields.thread_id -> openWish (resolves)
  orphanChip: "gd-Xq8xiPBDeB", // bead with fields.thread_id -> missing head (fallback)
};

// buildFixtures returns the full in-memory daemon state, with every timestamp
// resolved to an absolute RFC3339 string relative to `nowMs` (the daemon's
// frozen boot instant in epoch-ms).
export function buildFixtures(nowMs) {
  const at = (off) => new Date(nowMs - off.hours * 3600_000).toISOString();

  // beads: id -> bead JSON (daemon wire shape). comments: id -> [comment].
  // revDeps: parentId -> [{bead_id, depends_on_id, type}] (reverse: children).
  const beads = {};
  const comments = {};
  const revDeps = {};

  const head = (id, title, project, updatedOff, fields = {}, extra = {}) => {
    beads[id] = {
      id,
      title,
      kind: "issue",
      type: "task",
      status: "open",
      assignee: "",
      priority: 2,
      labels: ["thread", `project:${project}`],
      fields,
      created_at: at(updatedOff),
      updated_at: at(updatedOff),
      created_by: "slack:matthew.baker",
      ...extra,
    };
    return beads[id];
  };

  const memberField = (id, title, headID, createdOff, updatedOff, extra = {}) => {
    beads[id] = {
      id,
      title,
      kind: "issue",
      type: extra.type || "task",
      status: extra.status || "open",
      assignee: extra.assignee || "",
      priority: 2,
      labels: extra.labels || [`project:${PROJECT}`],
      fields: { thread_id: headID, ...(extra.fields || {}) },
      created_at: at(createdOff),
      updated_at: at(updatedOff),
      created_by: extra.created_by || "agent:gardenio-worker",
      ...(extra.closed_at ? { closed_at: extra.closed_at, closed_by: extra.closed_by } : {}),
    };
    return beads[id];
  };

  const child = (id, title, parentID, headID, createdOff, withField, extra = {}) => {
    const fields = withField ? { thread_id: headID } : {};
    beads[id] = {
      id,
      title,
      kind: "issue",
      type: extra.type || "task",
      status: extra.status || "open",
      assignee: "",
      priority: 2,
      labels: [`project:${PROJECT}`],
      fields,
      created_at: at(createdOff),
      updated_at: at(createdOff),
      created_by: extra.created_by || "agent:gardenio-worker",
      ...(extra.closed_at ? { closed_at: extra.closed_at, closed_by: extra.closed_by } : {}),
    };
    (revDeps[parentID] ||= []).push({ bead_id: id, depends_on_id: parentID, type: "parent-child" });
    return beads[id];
  };

  // ----- OPEN multi-message wish (the canonical populated detail) -----
  // Slack-origin wish: slack_root_text supplies the verbatim wish narrative.
  head(IDS.openWish, "Make the deploy pipeline turn green again", PROJECT, D(2), {
    slack_root_text:
      "Our prod deploy has been red for two days. Can the fleet get the pipeline green again and make sure it stays green? I keep getting paged.",
    slack_thread_channel: "C0AHE7C0E1G",
    slack_thread_ts: "1782600000.000100",
  });
  // Closure half: an epic child of the head, and a half-link task under it
  // (durable dep, thread_id field NOT yet copied — the 1b-3 write window).
  child(IDS.openEpic, "Triage and fix the flaky deploy gate", IDS.openWish, IDS.openWish, H(46), true);
  child(IDS.openTask, "Stabilise the e2e smoke retry loop (half-link)", IDS.openEpic, IDS.openWish, H(44), false);
  // Field-only member: a cross-epic reply/message bead bound by the wish
  // pointer only (NOT a parent-child descendant of the head).
  memberField(IDS.openMsg, "Posted an update on the smoke fix", IDS.openWish, H(40), H(40), {
    type: "message",
  });
  // Conversation: comments scattered across head + members interleave into the
  // fused timeline. Head comments become conversation bubbles.
  comments[IDS.openWish] = [
    { id: 1, author: "matthew.baker", text: "Thanks — paging has calmed down but I want it durable, not a one-off.", created_at: at(H(40)) },
    { id: 2, author: "gardenio-worker", text: "Root cause was a race in the smoke retry loop. Patch is up; running E2E now.", created_at: at(H(20)) },
    { id: 3, author: "matthew.baker", text: "Great. Let's keep this wish open until two consecutive green nightlies.", created_at: at(H(6)) },
  ];
  comments[IDS.openEpic] = [
    { id: 10, author: "gardenio-worker", text: "Splitting the gate fix from the retry-loop fix so they review independently.", created_at: at(H(45)) },
  ];

  // ----- CLOSED / fulfilled wish -----
  head(IDS.closedWish, "Add a dark-terminal skin to the beads viewer", PROJECT, D(5), {
    slack_root_text: "Could we get a proper dark terminal theme for the viewer? The white background is rough at night.",
  }, { status: "closed", closed_at: at(D(4)), closed_by: "matthew.baker" });
  child(IDS.closedTask, "Implement the dark-terminal skin + ship it", IDS.closedWish, IDS.closedWish, H(118), true, {
    status: "closed",
    closed_at: at({ hours: 4 * 24 + 2 }),
    closed_by: "gardenio-worker",
  });
  comments[IDS.closedWish] = [
    { id: 20, author: "gardenio-worker", text: "Shipped the dark-terminal skin; merged and deployed. Closing the wish as fulfilled.", created_at: at(D(4)) },
  ];

  // ----- NEEDS-TRIAGE wish (a failed agent left it unfulfilled) -----
  head(IDS.triageWish, "Migrate the legacy alerts to the new schema", PROJECT, D(1), {
    slack_root_text: "We need the legacy alert rules migrated to the new schema before the next on-call rotation.",
  }, { status: "needs_triage" });
  child(IDS.triageAgent, "agent: migrate-legacy-alerts", IDS.triageWish, IDS.triageWish, H(20), true, {
    type: "agent",
    status: "closed",
    closed_at: at(H(18)),
    closed_by: "controller",
  });
  comments[IDS.triageWish] = [
    { id: 30, author: "migrate-legacy-alerts", text: "Hit an unmapped rule type I can't translate safely. Stopping before I corrupt the ruleset — needs a human.", created_at: at(H(18)) },
  ];

  // ----- LONG-THREAD wish (many replies + many spawned beads) -----
  head(IDS.longWish, "Stand up the multi-region failover runbook", PROJECT, H(2), {
    slack_root_text:
      "Big one: I want a tested multi-region failover runbook — DB cutover, DNS, traffic shifting, and a rehearsal. This will span a lot of work; keep me in the loop.",
  });
  const longComments = [];
  // A long human/agent back-and-forth (16 replies) to force vertical scroll.
  const speakers = ["matthew.baker", "gardenio-gardener", "gardenio-worker"];
  const lines = [
    "Kicking this off — scoping the runbook into DB, DNS, and traffic phases.",
    "DB cutover rehearsal scheduled; freeze/dump/restore path drafted.",
    "Can we make the DNS TTL drop part of the runbook, not a manual step?",
    "Yes — folding the TTL pre-drop into phase 0 so propagation is ready.",
    "Traffic-shifting: weighted records or full cutover? I lean weighted.",
    "Weighted it is; 10/50/100 step plan with a hold gate between each.",
    "First rehearsal hit a snag on the restore verify — chasing it.",
    "Restore verify fixed (checksum mismatch was a stale dump). Re-running.",
    "Second rehearsal clean end-to-end. Capturing timings in the runbook.",
    "Love it. Add a rollback section — how do we abort mid-shift?",
    "Rollback section added: re-pin weights to origin, restore DNS TTL.",
    "On-call walked the runbook cold and got through it. Minor wording nits.",
    "Nits applied. Adding a 'who to page' table at the top.",
    "Page table in. I think we're one rehearsal away from calling this done.",
    "Final rehearsal booked for the maintenance window.",
    "Keeping this open until the final rehearsal signs off.",
  ];
  lines.forEach((text, i) => {
    longComments.push({ id: 100 + i, author: speakers[i % speakers.length], text, created_at: at(H(48 - i * 2)) });
  });
  comments[IDS.longWish] = longComments;
  // Many spawned beads (work) so the collapsed work pane has real volume.
  for (let i = 0; i < 9; i++) {
    const cid = `gd-longWORK${String(i).padStart(2, "0")}`;
    child(cid, `Failover work item ${i + 1}: ${["DB freeze", "dump", "restore", "verify", "DNS TTL", "weight 10%", "weight 50%", "weight 100%", "rollback drill"][i]}`,
      IDS.longWish, IDS.longWish, H(46 - i * 4), true,
      i < 6 ? { status: "closed", closed_at: at(H(44 - i * 4)), closed_by: "gardenio-worker" } : {});
  }

  // ----- EMPTY wish (a freshly-minted wish: no replies, no spawned work) -----
  // The detail empty-state cell: conversation lane shows only the "opened this
  // wish" marker, work lane shows the muted "no work yet" line. No members, no
  // comments. Proves V1 (no blank pane) + V2 on a genuinely empty wish.
  head(IDS.emptyWish, "Investigate the intermittent login redirect loop", PROJECT, H(3), {
    slack_root_text: "Some users hit a redirect loop on login about once a day. Can someone dig in? No repro steps yet.",
  });

  // ----- Member bead for the part-of-wish chip (RESOLVES to openWish) -----
  memberField(IDS.memberChip, "Stabilise the e2e smoke retry loop", IDS.openWish, H(44), H(20), {
    type: "task",
    labels: [`project:${PROJECT}`],
    created_by: "agent:gardenio-worker",
  });

  // ----- Member bead whose head is MISSING (chip UNRESOLVED fallback) -----
  // thread_id points at a head id that does NOT exist in the dataset, so the
  // chip cannot resolve a title and falls back to the bare id (still links).
  memberField(IDS.orphanChip, "Orphan derivative whose wish head was deleted", "gd-wishGONE999", H(30), H(30), {
    type: "task",
    labels: [`project:${PROJECT}`],
  });

  return { beads, comments, revDeps };
}

// URL plan: every (view,state) cell the matrix captures, plus which themes and
// breakpoints apply. The capture driver iterates SKINS x WIDTHS over this.
//
// Each cell may carry:
//   - `auth: true` — set the X-Auth-Request-Email header so the viewer's
//     EmailFromRequest returns a non-empty operator email and the composer
//     renders (CanCompose = viewerCanOperate && email != ""). Under
//     BEADS_VIEWER_DEV_AUTH_BYPASS, viewerCanOperate returns true (no roles in
//     context), so the only missing piece for composer-reachable cells is a
//     non-empty email.
//   - `interaction: <name>` — drives the capture-threads.mjs driver to perform
//     a live interaction (focus / type / send / reconcile) and capture the
//     resulting state. The name controls the per-cell capture script.
export function urlPlan() {
  return [
    // INDEX — populated (fleet scope shows every wish head incl. fulfilled +
    // needs-triage, so one populated index doubles as the fulfilled/triage cell).
    { view: "index", state: "populated", path: "/threads" },
    // INDEX — empty (a project with zero heads).
    { view: "index", state: "empty", path: `/threads?project=${EMPTY_PROJECT}` },
    // INDEX — error (daemon forced to fail the head enumeration). `fail` tells
    // the driver to POST /__control/fail {mode:"list"} before navigating, so the
    // head enumeration 500s and handleThreads renders the degraded error banner.
    { view: "index", state: "error", path: "/threads", fail: "list" },
    // DETAIL — populated (open multi-message wish; work <details> CLOSED).
    { view: "detail", state: "populated", path: `/threads/${IDS.openWish}` },
    // DETAIL — fulfilled (closed wish + close marker).
    { view: "detail", state: "fulfilled", path: `/threads/${IDS.closedWish}` },
    // DETAIL — needs-triage (amber banner).
    { view: "detail", state: "needstriage", path: `/threads/${IDS.triageWish}` },
    // DETAIL — empty (a freshly-minted wish: no replies, no work).
    { view: "detail", state: "empty", path: `/threads/${IDS.emptyWish}` },
    // DETAIL — long-thread scroll (many replies + many work beads).
    { view: "detail", state: "longthread", path: `/threads/${IDS.longWish}` },
    // DETAIL — error (timeline fetch forced to fail; head still renders). The
    // head GET succeeds, but the membership/comment fetches 500, so
    // handleThreadDetail keeps the head and shows "Failed to load thread
    // timeline." — the degraded-but-not-blank detail (rubric V1).
    { view: "detail", state: "error", path: `/threads/${IDS.openWish}`, fail: "timeline" },
    // BEAD_SHOW chip — resolved (member bead w/ fields.thread_id -> live head).
    { view: "beadchip", state: "resolved", path: `/${IDS.memberChip}?bead=1` },
    // BEAD_SHOW chip — unresolved fallback (head missing).
    { view: "beadchip", state: "fallback", path: `/${IDS.orphanChip}?bead=1` },
    // BEAD_SHOW chip — absent (a thread HEAD carries no thread_id field).
    { view: "beadchip", state: "absent", path: `/${IDS.openWish}?bead=1` },

    // --- Phase 3 task 3-4 (ka-0000003162): composer + optimistic-send cells.
    //
    // V5 COMPOSER REACHABLE: the operator-gated composer must render on a
    // Slack-bound wish, with a labelled textarea, a visible focus ring, and
    // keyboard reachability. Three captures per (skin, width):
    //   - empty    : the resting composer (placeholder visible).
    //   - focused  : focus on the textarea (the focus ring must be visible).
    //   - typed    : a typed multi-line message (the textarea grows; Send
    //                button enabled).
    //
    // V10 OPTIMISTIC SEND + ?since= RECONCILE: click Send on a typed message,
    // capture the optimistic "sending..." bubble before the server echo, then
    // inject the authoritative message bead via the mock daemon's
    // /__control/inject-message control endpoint, trigger the timeline refresh,
    // and capture the reconciled state — proving the bubble does NOT duplicate
    // after the server-rendered message arrives.
    //
    // Both rely on auth: true so EmailFromRequest returns a non-empty operator
    // email and CanCompose flips true (the dev-bypass middleware has no roles,
    // so viewerCanOperate is already true).
    { view: "composer", state: "empty", path: `/threads/${IDS.openWish}`, auth: true, interaction: "composer-empty" },
    { view: "composer", state: "focused", path: `/threads/${IDS.openWish}`, auth: true, interaction: "composer-focused" },
    { view: "composer", state: "typed", path: `/threads/${IDS.openWish}`, auth: true, interaction: "composer-typed" },
    { view: "composer", state: "optimistic", path: `/threads/${IDS.openWish}`, auth: true, interaction: "composer-optimistic" },
    { view: "composer", state: "reconciled", path: `/threads/${IDS.openWish}`, auth: true, interaction: "composer-reconciled" },
  ];
}

// COMPOSER_BODY is the message body the V10 optimistic-send + reconcile cells
// type into the composer and then inject via /__control/inject-message. It is
// deliberately multi-line so the wrap behaviour is captured. Exported so the
// capture driver and the grade doc both reference the same canonical string.
export const COMPOSER_BODY =
  "Acknowledged — pulling on the smoke retry loop now. Will report once the patch is in CI and the first nightly comes back green.";
