// Home Live-agents card RECENT-EVENT-STREAM coverage backfill (task
// ka-0000007613, coverage-gap sprint cycle 25; parent epic ka-0000000958).
//
// Merge 18bfb1336 (MR !6697, 2026-08-05T15:47Z) replaced the single one-line
// squawk preview on each Home (`/index`) "Live agents" card with a per-agent
// RECENT EVENT STREAM: index_rail.go's recentSquawksByAgent() +
// recentCommentsByAgent() feed buildAgentEventStream(), which merges both
// sources newest-first, previews each to one truncated line, de-dups on the
// RENDERED preview, and caps at eventStreamRowsPerAgent (3). templates/index.html
// renders it as <ul class="index-active-card-events"> with per-row
// index-active-card-event-{squawk|comment} + a 📣/💬 glyph + an -ago stamp.
//
// HEAD-VERIFIED ZERO COVERAGE (task audit, re-confirmed at HEAD 0d6049827):
// `index-active-card-events`, `index-active-card-squawk` /
// `index-rail-agent-squawk`, and `buildAgentEventStream` all had 0 hits across
// gardenio/tests/ — coverage was unit-only (index_rail_event_stream_test.go).
//
// THE NAME-MATCH≠COVERAGE TRAP this bead names: test-agents-live-view.sh DOES
// assert `agent-event-closed` / `agent-event-meta">by …` — but that is a
// DIFFERENT surface (agent_events.go, the per-agent activity lane at
// GET /agents/{id}/events). The Home rail card is index_rail.go +
// templates/index.html, a distinct handler with distinct selectors
// (index-active-card-*). Same feature noun ("agent events"), two unrelated
// renderers. There is NO e2e touching index-rail-* / index-active-card-*
// selectors at HEAD, so this is a new module (the task's predicted
// test-viewer-home-rail.sh, realized as a Playwright spec — the vehicle the
// FRESHEST sibling backfill plans_signal_strip.spec.js uses for exactly this
// class of fixture-driven render assertion).
//
// ANTI-VACUITY (the guardrail the origin task names explicitly): a selector
// assertion passes trivially if the fixture never produces the state. Every
// assertion below pins the VALUE, not mere presence:
//   1. INTERLEAVE + ORDERING — one agent seeded with a squawk AND a comment at
//      KNOWN, different timestamps; both rows render and the NEWER (the comment)
//      is FIRST in document order. buildAgentEventStream appends squawks BEFORE
//      comments, so a newer comment leading proves the load-bearing
//      sort.SliceStable re-sort actually ran (not just insertion order).
//   2. KIND DISCRIMINATION — the comment row carries index-active-card-event-comment
//      (💬) and the squawk row index-active-card-event-squawk (📣); a bug that
//      mislabels kind is invisible to a presence-only check.
//   3. DE-DUP — a squawk AND a mirror-comment with identical text collapse to
//      exactly ONE row (the documented squawk-mirror filter + preview de-dup).
//   4. CAP — more than eventStreamRowsPerAgent(3) distinct events render exactly 3.
//   5. FALLBACK / EMPTY — an agent with a squawk but no comments still renders a
//      stream; an agent with NO events renders NEITHER the stream <ul> NOR the
//      legacy index-active-card-squawk div. (See DEAD-BRANCH note below.)
//
// DEAD-BRANCH NOTE (index-active-card-squawk / index-rail-agent-squawk). The
// template's `{{else if .Squawk}}<div class="index-active-card-squawk
// index-rail-agent-squawk">` fallback is UNREACHABLE at HEAD: index_rail.go:441
// sets row.Squawk ONLY from row.Events[0].Text, inside `if len(row.Events) > 0`,
// so .Squawk is non-empty IFF .Events is non-empty — and the template renders
// .Events first, so the else-if can never fire. This spec therefore covers those
// two selectors NEGATIVELY (they must NEVER render on any live card), which is
// the correct coverage for currently-dead markup and pins the invariant so a
// future refactor that revives .Squawk independently of .Events trips this test.
// Follow-up bead kd-<filed at close> records the dead branch for a separate
// cleanup MR (out of scope for a test-only backfill).
//
// LOCAL-RUN NOTE (mirrors plans_signal_strip.spec.js / plans_mr_overflow_ci.spec.js):
// this pod has no Postgres/pgvector and no docker daemon, and localhost:8080 is
// the PROD daemon (which must never be seeded), so the running-spec leg is
// verified in CI's hermetic `test-beads-viewer-e2e` task (apt postgresql-16 +
// pgvector → kd serve → beads-viewer auto-boot). See the suite README.
//
// ISOLATION: every agent / squawk / comment here is EPHEMERAL — unique suffix
// per test, its own fixture:e2e:home_rail_* labels, in project:homerail — so
// under fullyParallel:true these never race the shared seed or each other. The
// Home Live-agents panel is FLEET-WIDE (computeIndexLiveAgents has no project
// filter, sorts -updated_at, cap 60): a freshly-seeded agent carries the newest
// updated_at, so it surfaces at the top of the grid regardless of the shared
// fleet. Each card is located by its foot agent-link (href ending /{agentId}),
// the one selector that uniquely keys a card to its seeded agent bead.
//
// AUTH / AUTHORSHIP: creating beads via `kd` attributes them to
// BEADS_OPERATED_BY; the index reads with no per-me filter so the card renders
// regardless of viewer identity. Comment authorship is the load-bearing join
// key — recentCommentsByAgent keys comments by comment.Author. The daemon
// derives comment.Author from the credential and honours no X-Actor override
// (the shared static token that did is retired, gd-ke6PC7bRto), so
// seedComment writes on the agent's OWN per-agent fleet token (mintAgentToken,
// subject <agentName>), which stores Author=<agentName> — the same name the
// agent bead's fields.agent and the squawk's source_agent carry, joining all
// three feeds onto one card. Minting needs the harness OIDC issuer, so the
// describes that seed a comment call requireOIDCStub.

import { execFileSync } from 'node:child_process';

import { test, expect } from '@playwright/test';

import { mintAgentToken, requireOIDCStub } from './_oidc.js';

// The Home landing page carrying the Live-agents rail. `/` is the feed
// (server.go: GET /{$} → handleFeed); the rail lives at /index
// (handleIndex → computeIndexRail → computeIndexLiveAgents).
const HOME = '/index';

const PROJECT = 'homerail';

// kd shells out to the CLI (mirrors plans_signal_strip.spec.js /
// plans_mr_overflow_ci.spec.js) to build fixtures against the same daemon the
// viewer reads. Surfaces the daemon's one-line error rather than kd's usage
// banner on failure.
function kd(args) {
  const beadsHTTPAddr = process.env.BEADS_HTTP_ADDR || 'http://localhost:8080';
  const env = { ...process.env, BEADS_HTTP_ADDR: beadsHTTPAddr };
  try {
    return execFileSync('kd', args, { env, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
  } catch (err) {
    const stderr = err.stderr ? err.stderr.toString() : '';
    const firstLine = stderr.split('\n').find((l) => l.trim().length > 0) || err.message;
    throw new Error(`kd ${args[0]} failed (exit ${err.status}): ${firstLine}`);
  }
}

function kdCreateJSON(args) {
  const raw = kd([...args, '--json']);
  // Strip any deprecation banner kd prints before the JSON payload (matches
  // seed.mjs / the sibling plans specs).
  const i = raw.search(/[{[]/);
  const bead = JSON.parse(i === -1 ? raw : raw.slice(i));
  if (!bead.id) throw new Error(`kd create returned no id: ${raw}`);
  return bead.id;
}

function uniqueSuffix() {
  return `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
}

// createLiveAgent mints an ephemeral ALIVE agent bead (agent_state=working is in
// agentStateAlive {working,spawning,yielding}) with the schema-required
// [agent,role,project] fields (type:agent 400s a bare create). The agent NAME is
// the join key: recentSquawksByAgent keys on squawk source_agent, and
// recentCommentsByAgent keys on comment author, both matched to fields.agent.
// Returns { agentId, agentName }.
function createLiveAgent(suffix) {
  const agentName = `e2e-home-rail-${suffix}`;
  const agentId = kdCreateJSON([
    'create',
    `[E2E ephemeral] home-rail agent ${suffix}`,
    '--type', 'agent',
    '--label', `project:${PROJECT}`,
    '--label', `fixture:e2e:home_rail_agent_${suffix}`,
    '--field', `agent=${agentName}`,
    '--field', 'role=crew',
    '--field', `project=${PROJECT}`,
    '--field', 'agent_state=working',
  ]);
  return { agentId, agentName };
}

// seedSquawk creates a closed type=message squawk bead attributed to agentName
// via source_agent (the exact shape `gb squawk` writes — cmd/gb/squawk.go). The
// `squawk` label + source_agent field are what recentSquawksByAgent joins on.
// Returns the squawk bead id.
function seedSquawk(agentName, suffix, tag, text) {
  return kdCreateJSON([
    'create',
    `[E2E ephemeral] squawk ${tag} ${suffix}`,
    '--type', 'message',
    '--label', `project:${PROJECT}`,
    '--label', 'squawk',
    '--label', `fixture:e2e:home_rail_squawk_${tag}_${suffix}`,
    '--field', `source_agent=${agentName}`,
    '--field', `text=${text}`,
  ]);
}

// seedComment adds a comment authored by agentName (written on the agent's own
// per-agent token → comment.Author=agentName). It emits a durable
// beads.comment.added event that recentCommentsByAgent joins on by author. The
// comment is hung on the agent's OWN bead — the target bead is irrelevant to
// the join (author is the only key), the agent bead is a guaranteed-present,
// ephemeral, self-cleaning target.
async function seedComment(agentId, agentName, text) {
  const token = await mintAgentToken(agentId, agentName);
  kd(['--token', token, 'comment', 'add', agentId, '--body', text]);
}

// agentCard locates the ONE Live-agents card whose foot agent-link points at the
// seeded agent bead — the only selector that uniquely keys a card to its bead
// (the card itself carries no data-bead-id). Scopes every downstream assertion to
// this card so the shared fleet's other cards can't false-positive a match.
function agentCard(page, agentId) {
  return page.locator('li.index-active-card', {
    has: page.locator(`a.index-active-card-agentlink[href$="/${agentId}"]`),
  });
}

// gotoHomeUntilRows reloads /index until the seeded agent's event stream renders
// exactly `want` rows, then returns the card locator. WHY the reload loop: the
// squawk feed reads the beads table (visible the instant `kd create` returns),
// but the comment feed reads the durable EVENTS table via GET /v1/events with an
// `until=<viewer-now>` upper bound. A comment written microseconds before the
// page load can carry a daemon-clock created_at a hair AHEAD of the viewer
// process's `now`, so the first render's time-window query legitimately excludes
// it — the row appears on the very next load. /index is a fully static
// server-render (no rail poller, auto-refresh off by default), so a single-shot
// toHaveCount can't observe that catch-up; a reload can. This waits for the
// eventually-consistent join WITHOUT weakening what we assert once it lands
// (interleave order, kind glyphs, and the -ago value are all still checked
// strictly below). Squawk-only / eventless tests don't touch the events table,
// so they don't need this and call page.goto directly.
async function gotoHomeUntilRows(page, agentId, want) {
  await expect(async () => {
    await page.goto(HOME);
    const rows = agentCard(page, agentId).locator('ul.index-active-card-events li.index-active-card-event');
    await expect(rows).toHaveCount(want);
  }).toPass({ timeout: 20000 });
  return agentCard(page, agentId);
}

// -----------------------------------------------------------------------------
// 1. Interleave + ordering — the load-bearing sort.SliceStable behavior.
//    Seed one agent with a squawk and a comment at KNOWN, different times, the
//    comment NEWER. buildAgentEventStream appends squawks before comments, so if
//    the newer comment renders FIRST, the newest-first re-sort demonstrably ran.
// -----------------------------------------------------------------------------
test.describe('home rail — recent event stream interleave (index-active-card-events)', () => {
  requireOIDCStub(test);

  test('a squawk and a newer comment render newest-first on one card', async ({ page }) => {
    const suffix = uniqueSuffix();
    const { agentId, agentName } = createLiveAgent(suffix);
    // Squawk first (older), then a comment (newer). The comment is authored
    // AFTER the squawk bead is created, so its created_at is strictly later —
    // no backdating needed for a deterministic newest-first ordering.
    const squawkText = `home-rail squawk body ${suffix}`;
    const commentText = `home-rail comment body ${suffix}`;
    seedSquawk(agentName, suffix, 'interleave', squawkText);
    await seedComment(agentId, agentName, commentText);

    // Reload until BOTH feeds have joined onto the card (squawk is immediate;
    // the comment's durable event catches up within a reload — see
    // gotoHomeUntilRows). The card + stream <ul> (the selector the task audit
    // counts) are proven present by the row-count wait.
    const card = await gotoHomeUntilRows(page, agentId, 2);
    const stream = card.locator('ul.index-active-card-events');
    await expect(stream, 'the recent-event stream <ul> renders').toBeVisible();

    // Both rows appear …
    const rows = stream.locator('li.index-active-card-event');
    await expect(rows).toHaveCount(2);
    await expect(stream).toContainText(squawkText);
    await expect(stream).toContainText(commentText);

    // … and the NEWER comment is FIRST — the anti-vacuous ordering assertion.
    // buildAgentEventStream appends squawks before comments; only the
    // sort.SliceStable(newest-first) re-sort can put the comment ahead of the
    // squawk, so this pins that behavior, not insertion order.
    await expect(rows.nth(0)).toHaveClass(/index-active-card-event-comment/);
    await expect(rows.nth(0).locator('.index-active-card-event-text')).toHaveText(commentText);
    await expect(rows.nth(1)).toHaveClass(/index-active-card-event-squawk/);
    await expect(rows.nth(1).locator('.index-active-card-event-text')).toHaveText(squawkText);

    // Each row carries a relative-time stamp (fmtTimeAgo: Ns/Nm/Nh/Nd) — a real
    // value, not an empty node — proving the -ago span is data-driven.
    await expect(rows.nth(0).locator('.index-active-card-event-ago')).toHaveText(/^\d+[smhd]$/);
    await expect(rows.nth(1).locator('.index-active-card-event-ago')).toHaveText(/^\d+[smhd]$/);
  });
});

// -----------------------------------------------------------------------------
// 2. Kind discrimination — the glyph + class channel.
//    A mislabeled kind is invisible to a presence-only check; assert the comment
//    row carries 💬 and the squawk row 📣, keyed by the -comment / -squawk class.
// -----------------------------------------------------------------------------
test.describe('home rail — event kind discrimination (glyph + class)', () => {
  requireOIDCStub(test);

  test('the comment row is 💬 and the squawk row is 📣', async ({ page }) => {
    const suffix = uniqueSuffix();
    const { agentId, agentName } = createLiveAgent(suffix);
    const squawkText = `kind squawk ${suffix}`;
    const commentText = `kind comment ${suffix}`;
    seedSquawk(agentName, suffix, 'kind', squawkText);
    await seedComment(agentId, agentName, commentText);

    // Reload until both the squawk and the (eventually-consistent) comment have
    // joined onto the card — 2 rows — before discriminating their kinds.
    const card = await gotoHomeUntilRows(page, agentId, 2);

    // The squawk row: index-active-card-event-squawk + the 📣 glyph.
    const squawkRow = card.locator('li.index-active-card-event-squawk', { hasText: squawkText });
    await expect(squawkRow).toHaveCount(1);
    await expect(squawkRow.locator('.index-active-card-event-glyph')).toHaveText('📣');

    // The comment row: index-active-card-event-comment + the 💬 glyph. The
    // DIFFERENT glyph on a co-rendered pair proves the kind drives the glyph,
    // not a constant.
    const commentRow = card.locator('li.index-active-card-event-comment', { hasText: commentText });
    await expect(commentRow).toHaveCount(1);
    await expect(commentRow.locator('.index-active-card-event-glyph')).toHaveText('💬');
  });
});

// -----------------------------------------------------------------------------
// 3. De-dup — a squawk and its `_squawk kd-… from …_`-footered mirror comment
//    collapse to ONE row. recentCommentsByAgent's squawkMirrorFooter regex drops
//    the mirror at the source (buildAgentEventStream's preview de-dup is the
//    backup guard), so the squawk+mirror pair renders exactly one row — a bug
//    that stopped dropping the mirror would read as two identical lines.
// -----------------------------------------------------------------------------
test.describe('home rail — squawk/comment de-dup', () => {
  requireOIDCStub(test);

  test('a squawk and an identical-text comment render exactly one row', async ({ page }) => {
    const suffix = uniqueSuffix();
    const { agentId, agentName } = createLiveAgent(suffix);
    // A squawk plus its MIRROR comment — the exact shape `gb squawk` writes when
    // it copies a squawk onto the agent's claimed bead: the squawk body followed
    // by the `_squawk kd-… from <actor>_` footer. recentCommentsByAgent's
    // squawkMirrorFooter regex drops that comment at the source, so only the
    // squawk bead reaches buildAgentEventStream → one row. (buildAgentEventStream
    // additionally de-dups on the rendered preview, a second independent guard
    // for a mirror that ever slips the footer regex — but the footer drop is what
    // fires here, and it is the documented squawk-mirror filter this item covers.)
    const body = `dedup shared line ${suffix}`;
    seedSquawk(agentName, suffix, 'dedup', body);
    await seedComment(agentId, agentName, `${body}\n\n_squawk gd-fake from ${agentName}_`);

    await page.goto(HOME);
    const card = agentCard(page, agentId);
    await expect(card).toBeVisible();

    const stream = card.locator('ul.index-active-card-events');
    await expect(stream).toBeVisible();
    // Exactly one row survives, and it renders the shared body exactly once.
    await expect(stream.locator('li.index-active-card-event')).toHaveCount(1);
    await expect(stream.locator('.index-active-card-event-text', { hasText: body })).toHaveCount(1);
  });
});

// -----------------------------------------------------------------------------
// 4. Cap — more than eventStreamRowsPerAgent(3) distinct events render exactly 3.
//    Seed 4 distinct squawks; the stream caps the merge at 3. Asserting the
//    rendered count equals the cap (not ≥1) is what guards the cap regressing to
//    "render everything" and blowing out card height in the two-up grid.
// -----------------------------------------------------------------------------
test.describe('home rail — event stream cap (eventStreamRowsPerAgent)', () => {
  test('more than the cap of distinct events renders exactly the cap', async ({ page }) => {
    const suffix = uniqueSuffix();
    const { agentId, agentName } = createLiveAgent(suffix);
    // Four DISTINCT squawk bodies so de-dup can't mask the cap. recentSquawksByAgent
    // itself keeps only the newest 3/agent, and buildAgentEventStream caps the
    // merge at 3 — either way the rendered count is the cap, never 4.
    for (let n = 1; n <= 4; n += 1) {
      seedSquawk(agentName, suffix, `cap${n}`, `cap event number ${n} ${suffix}`);
    }

    await page.goto(HOME);
    const card = agentCard(page, agentId);
    await expect(card).toBeVisible();

    const stream = card.locator('ul.index-active-card-events');
    await expect(stream).toBeVisible();
    // Exactly the cap (3), never all four — the value the selector guards.
    await expect(stream.locator('li.index-active-card-event')).toHaveCount(3);
  });
});

// -----------------------------------------------------------------------------
// 5a. Fallback branch — a squawk-only agent still renders a stream (no comments).
//     Proves the stream renders from squawks alone (the {{if .Events}} branch),
//     not only when both feeds are present.
// -----------------------------------------------------------------------------
test.describe('home rail — squawk-only fallback', () => {
  test('an agent with a squawk but no comments still renders the stream', async ({ page }) => {
    const suffix = uniqueSuffix();
    const { agentId, agentName } = createLiveAgent(suffix);
    const squawkText = `squawk only ${suffix}`;
    seedSquawk(agentName, suffix, 'only', squawkText);
    // Deliberately NO comment.

    await page.goto(HOME);
    const card = agentCard(page, agentId);
    await expect(card).toBeVisible();

    const stream = card.locator('ul.index-active-card-events');
    await expect(stream).toBeVisible();
    const rows = stream.locator('li.index-active-card-event');
    await expect(rows).toHaveCount(1);
    await expect(rows.nth(0)).toHaveClass(/index-active-card-event-squawk/);
    await expect(rows.nth(0).locator('.index-active-card-event-text')).toHaveText(squawkText);

    // The legacy single-line fallback div is NOT used even for a squawk-only
    // agent (see the DEAD-BRANCH note): .Events is non-empty, so the template
    // renders the <ul> and the {{else if .Squawk}} div never fires.
    await expect(card.locator('.index-active-card-squawk')).toHaveCount(0);
  });
});

// -----------------------------------------------------------------------------
// 5b. Empty branch + dead legacy selectors — an agent with NO events renders
//     NEITHER the stream <ul> NOR the unreachable index-active-card-squawk /
//     index-rail-agent-squawk fallback div. This is the correct (negative)
//     coverage for the currently-dead markup: row.Squawk is only ever set from
//     row.Events[0], so the fallback can never fire; this pins that invariant so
//     a refactor reviving .Squawk independently of .Events trips here.
// -----------------------------------------------------------------------------
test.describe('home rail — no-events card + dead fallback selectors', () => {
  test('an agent with no squawks or comments renders no stream and no legacy squawk div', async ({ page }) => {
    const suffix = uniqueSuffix();
    const { agentId } = createLiveAgent(suffix);
    // No squawk, no comment — the agent produced nothing in the lookback windows.

    await page.goto(HOME);
    const card = agentCard(page, agentId);
    await expect(card, 'the eventless live-agent card still renders').toBeVisible();

    // Neither the stream nor the legacy fallback renders.
    await expect(card.locator('ul.index-active-card-events')).toHaveCount(0);
    await expect(card.locator('.index-active-card-squawk')).toHaveCount(0);
    await expect(card.locator('.index-rail-agent-squawk')).toHaveCount(0);
  });
});
