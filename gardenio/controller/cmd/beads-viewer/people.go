package main

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
)

// peopleRecentBeadCap bounds the per-operator "recently created" list rendered
// under each directory row. Small on purpose — the directory is a check-in
// surface, not a feed; the per-user homepage (/u/{slug}) is the full view.
const peopleRecentBeadCap = 5

// peopleRecentMessageCap bounds the per-operator "recent requests" preview
// list rendered under each directory row (ka-0000005614). A few lines of the
// human's most-recent thread-request messages so a glance at /people shows
// "what they're up to" — the operator's ask. Small on purpose (a preview, not
// a transcript); each preview links to the full /threads/{id} conversation.
const peopleRecentMessageCap = 3

// peopleMessagePreviewLen caps the rendered length of a single thread-request
// message preview. Long asks are truncated on a word boundary (truncate) with
// an ellipsis appended by the template.
//
// This cap MUST fit inside the CSS two-line clamp on .people-request-text
// (people.html) at the NARROWEST card — the binding case is the 17rem grid
// track on a multi-column desktop, not a wide viewport. If the cap is larger
// than what two lines can hold, `-webkit-line-clamp: 2` does the actual
// truncation, and it clips at a CHARACTER boundary (mid-word) with its own
// ellipsis — the exact "…should be I…" / "…ability to do a…" clip the
// Users-CAMP audit flagged (ka-0000003211). At the 0.82rem preview font a
// word-truncated ask stays within two lines up to ~40 chars in the tightest
// real card — the ~170px .people-request-text box in a 17rem grid track on a
// 1280px desktop (the flex recency tag + padding eat the rest of the ~296px
// card), measured against an adversarial wide-word battery in a headless
// Chromium render of the real /people page. 38 sits just below that ceiling for
// cross-platform font-metric headroom, so the visible ellipsis is always
// truncate()'s word-boundary one and the CSS clamp is only a safety net.
const peopleMessagePreviewLen = 38

// peopleActivityWindow is the age bound on every "what has this person done
// lately" signal the directory renders — the recently-created chips, the
// recent-request previews, and (through them) the quiet/active partition
// (ka-0000001991).
//
// Before this the activity strips carried NO time window at all: a bead raised
// in March and a Slack ask from five weeks ago sat on a card looking exactly
// like today's work, so the page could not answer the question it exists to
// answer — "what have people actually done recently". Measured on prod at the
// time of the fix, 18 of the 22 rendered request strips led with an entry more
// than a day old and 12 led with one more than a week old.
//
// 24h is the default scope because the directory is a check-in surface and the
// operator ask was literally "I want to see what users have actually done in
// the last day". It is deliberately TIGHTER than the daemon's own 7d fallback:
// a too-wide window silently restores the stale-reads-as-current bug, while a
// too-tight one merely shows an empty strip — which is honest, and the card
// still carries the 7d throughput counts and the /u/{slug} link to the full
// history. Widening this to a user-selectable scope toggle is the natural
// follow-up; the plumbing (recent_window_hours, CreatedAfter) already takes an
// arbitrary window.
const peopleActivityWindow = 24 * time.Hour

// peopleRecentMessagesScanLimit bounds the SINGLE thread-request-message query
// backing the "recent requests" strip. Like peopleStatsRecentCeiling it caps
// ONE page-level round-trip: the type=message slack-thread-message beads
// created inside peopleActivityWindow, newest first, grouped by operator in
// memory and capped at peopleRecentMessageCap each.
//
// Since ka-0000001991 this is a BACKSTOP rather than the primary bound — the
// window is what keeps the result set small. That matters because the scan is
// ONE shared newest-first page: anything over the limit is dropped from the
// oldest end GLOBALLY, which starves the operators who post least rather than
// trimming each evenly. Keep it generous relative to the window so truncation
// stays hypothetical.
const peopleRecentMessagesScanLimit = 2000

// peopleListLimit caps how many operators the /people directory renders.
// Mirrors userHomeListLimit's spirit — directory is a single-page browsing
// surface, not a paginated feed.
const peopleListLimit = 500

// peopleHumanKinds is the set of operator kinds that surface on /people.
// Mirrors fleetSentinelKinds (fleet.go) — that page filters TO {system, fleet}
// for sentinel bridge accounts, and the inverse filter belongs here so the
// human directory does not double-count synthetic rows. Bot/sentinel kinds
// (system, fleet, gitlab, jira, agent, email) are out of scope: they ship
// noreply.kbeads.local emails, never share an identity key with a human row,
// and otherwise pollute the directory and the aggregate header chips with
// rows like op-9 (system:legacy-pre-attribution, 240k+ open beads).
var peopleHumanKinds = map[string]struct{}{
	"oidc":  {},
	"slack": {},
}

// machineSubjectRe matches a subject shaped like a service/bot account even on
// an otherwise human kind (oidc/slack): GitLab group/project bot subjects
// (`group_<n>_bot_<hash>`, `project_<n>_bot_<hash>`, any `_bot_` token) and a
// standalone "bot" word. A real human OIDC subject is an email
// (`first.last@domain`) and a Slack subject is a `U…` id, so `\bbot\b` and the
// prefixes never fire on a genuine person — this only rescues a human-KIND row
// whose subject is actually a service account (ka-0000002163 Fix spec:
// "subject matching bot/group_/project_ → machine").
var machineSubjectRe = regexp.MustCompile(`(?i)^group_|^project_|_bot_|\bbot\b`)

// hasMachineSubject reports whether subject looks like a service/bot account.
func hasMachineSubject(subject string) bool {
	return machineSubjectRe.MatchString(strings.TrimSpace(subject))
}

// isMachineOperator classifies one operator identity as a machine (service /
// bridge / fleet / CI) rather than a human. An identity is a machine when its
// kind is NOT a human login kind (peopleHumanKinds = {oidc, slack}) — i.e. a
// sentinel kind (system, fleet, gitlab, jira, agent, email) — OR when a human
// kind carries a machine-shaped subject (a stray gitlab bot logged in as oidc).
// isMachineOperator and the human path are exact complements over every
// operator row, so no identity is dropped from both the human directory and the
// service-accounts section.
func isMachineOperator(op beadsapi.OperatorDirectoryEntry) bool {
	if _, human := peopleHumanKinds[op.Kind]; !human {
		return true
	}
	return hasMachineSubject(op.Subject)
}

// filterHumanOperators returns only the HUMAN entries in ops — a human login
// kind (peopleHumanKinds) AND not a machine-shaped subject. Order-preserving.
// Used by both handlePeople and handleOperatorHoverCard so the hover card
// resolves the same set of rows the directory renders (a sentinel or bot slug
// must not surface a human card). Exact complement of filterMachineOperators.
func filterHumanOperators(ops []beadsapi.OperatorDirectoryEntry) []beadsapi.OperatorDirectoryEntry {
	out := make([]beadsapi.OperatorDirectoryEntry, 0, len(ops))
	for _, op := range ops {
		if isMachineOperator(op) {
			continue
		}
		out = append(out, op)
	}
	return out
}

// filterMachineOperators returns only the MACHINE (service / bridge / fleet /
// CI) entries in ops — the exact complement of filterHumanOperators.
// Order-preserving. Drives the collapsed "Service & machine accounts" section
// on /people (ka-0000002163): machine identities used to be silently dropped by
// the human filter; now they render in a separate collapsed section below the
// human directory and are counted in the header, so the split is visible
// without being intermixed with the roster.
func filterMachineOperators(ops []beadsapi.OperatorDirectoryEntry) []beadsapi.OperatorDirectoryEntry {
	out := make([]beadsapi.OperatorDirectoryEntry, 0, len(ops))
	for _, op := range ops {
		if isMachineOperator(op) {
			out = append(out, op)
		}
	}
	return out
}

// peopleStatsRecentCeiling is a hard ceiling on the TOTAL number of
// "recently created" chip rows the daemon returns across all operators for
// one /people render (ka-0000001390). It bounds ONLY the recent-chips query
// so it can never fan out unbounded — it is NOT a cap on the open-bead counts
// or the last-active recency, which the daemon now computes via a true
// full-table GROUP BY operated_by_id.
//
// Before this fix the directory pulled the 5000 most-recently-updated beads
// and grouped them in memory; against a 245K+ open-bead fleet every per-row
// "open" count and the header total were sampling artifacts. The real
// aggregate (open/in_progress count + max(updated_at) last-active across ALL
// statuses, so operators with only closed work still sort by genuine
// recency) is computed server-side and is not subject to any sample cap.
const peopleStatsRecentCeiling = 5000

// peopleRow is the row view-model rendered by /people and by the operator
// hover-card partial (one source of truth so the standalone card matches the
// directory row exactly).
type peopleRow struct {
	OperatorID    int64
	Slug          string
	Display       string
	Email         string
	Kind          string
	StatusMessage string
	UserHomeHref  string
	HoverHref     string
	// OpenBeads is the count of open/in_progress beads attributed to this
	// operator (or, after dedup, across every identity collapsed into this
	// row) via beads.operated_by_id. Zero when there's no active work
	// (rendered as em-dash in the template).
	OpenBeads int
	// The richer per-row stat set (§2.2) — each summed across the operator
	// group's members exactly like OpenBeads, rendered as a compact stat-chip
	// strip with em-dash for zero. AgentsActive+GardenersActive fold into one
	// 🤖 chip on the row; the header breaks gardeners out.
	InProgress       int
	Created7d        int
	Closed7d         int
	MRsOpen          int
	MRsMerged7d      int
	DecisionsPending int
	AgentsActive     int
	GardenersActive  int
	// FleetActive is AgentsActive+GardenersActive — the single 🤖 chip value
	// the row renders (the header breaks gardeners out separately). Computed in
	// handlePeople so the template needs no arithmetic helper.
	FleetActive int
	// PlansCount is the number of active plans this human CREATED — a discovery
	// teaser + a floor (§4 Step 4, ka-0000000236). It is deliberately the
	// created-count, not the full involvement set: an involvement count
	// (creator ∪ step-owner ∪ decision-resolver ∪ gardener-owner, decision
	// ka-0000000098) needs a per-operator 4-facet fan-out (plansInvolvingOperator),
	// and running that for every one of up to peopleListLimit rows is exactly the
	// per-row fan-out this task forbids. Instead the count comes from ONE bounded
	// plan-universe scan grouped by creator email (lookupPlanCountsByEmail) —
	// a single page-level round-trip — and the chip LINKS to PlansHref, which
	// lands on the person's full INVOLVED set (Step 1's query resolves it live
	// for that single operator on the click). So the number is a floor and the
	// link is the real involvement pivot — the same created-floor-teaser + involved-
	// on-click split Step 5's browse-by-person strip (plans_people_strip.go) uses.
	PlansCount int
	// PlansHref is the ?operated_by=<alias> pivot to this person's INVOLVED plans
	// on /plans — the same operator-scope param Step 3's Their-plans "See all" and
	// the S1 header chip emit, so a click runs plansInvolvingOperator for this one
	// human. Empty when no canonical alias resolves (a human with no email), in
	// which case the chip renders unlinked.
	PlansHref string
	// LastActiveAgo is a humanized "ago" string derived from the most recent
	// updated_at across the operator's HUMAN-authored beads of any status, so
	// recency stays meaningful even for operators with no open work. Machine
	// scaffolding (agent/session/trigger) is excluded daemon-side — those
	// heartbeat on their own and used to render "active 2m ago" for a person
	// who had done nothing for days (ka-0000001991). Empty when there's no
	// human activity.
	LastActiveAgo string
	// RecentBeads is the operator's ~5 most-recently-created beads within
	// peopleActivityWindow (grouped by operated_by_id), rendered as compact
	// linked chips under the row. Empty when the operator has created nothing
	// in that window.
	RecentBeads []peopleRecentBead
	// RecentMessages is the operator's ~3 most-recent thread-request messages
	// within peopleActivityWindow (type=message slack-thread-message beads
	// keyed by OperatedByID), rendered as a few truncated preview lines under
	// the row so a glance at /people shows "what they're up to" — the
	// operator's asks (ka-0000005614). Each preview links to the full
	// /threads/{thread_id} conversation. Empty when the operator has sent no
	// thread-request messages inside the window.
	RecentMessages []peopleThreadMessage
	// Identities is the FULL set of operator identities folded into this human
	// — every (kind, subject-or-handle) the human logs in or acts as, primary
	// first. Always populated (at least the primary), not just when display-time
	// dedup collapsed extra rows: the directory's job is to disambiguate which
	// handles map to one person, so every linked identity is rendered as a
	// labeled chip (slack U-id, gitlab handle, agent name, …). The grouping is
	// render-time only — never a destructive operator merge.
	Identities []peopleIdentity
	// groupLastActive is the most-recent LastActive across every identity in
	// this row's group. Unexported: used only for the activity sort, never
	// rendered (the humanized form is LastActiveAgo).
	groupLastActive time.Time
}

// IsQuiet reports whether this row is a "near-empty" card — an operator that
// shows no activity signal at all: every stat count is zero (so each chip
// renders the em-dash placeholder), no recently-created beads, no recent
// thread-request previews, and no status message. The directory de-emphasizes
// these cards (subtle opacity + lighter border, template data-quiet flag) so an
// operator's eye lands on high-activity colleagues first, raising signal-per-
// pixel (ka-0000005627). It is a WEIGHTING signal only — the card stays in
// place, fully reachable, and hover/focus restores full weight, so nothing is
// hidden or collapsed. A single count, a recent bead/request, or a set status
// message is enough to keep the card at full weight (it carries real content).
//
// The RecentBeads / RecentMessages arms are only a meaningful liveness signal
// because both are now age-bounded to peopleActivityWindow (ka-0000001991) —
// previously an unbounded scan let a bead from March hold a dormant operator
// at full weight forever, which is precisely how stale work kept reading as
// current on this page.
func (r peopleRow) IsQuiet() bool {
	return r.OpenBeads == 0 &&
		r.InProgress == 0 &&
		r.Created7d == 0 &&
		r.Closed7d == 0 &&
		r.MRsOpen == 0 &&
		r.MRsMerged7d == 0 &&
		r.DecisionsPending == 0 &&
		r.FleetActive == 0 &&
		r.PlansCount == 0 &&
		len(r.RecentBeads) == 0 &&
		len(r.RecentMessages) == 0 &&
		strings.TrimSpace(r.StatusMessage) == ""
}

// SearchText builds the lowercased substring haystack the client-side directory
// filter matches a typed query against (ka-0000000978). It joins every
// operator-meaningful, jump-to-a-colleague field — display name, canonical
// email, status message, and every folded identity's provider (oidc / slack /
// gitlab / agent — the "role" the operator types) plus its handle and slug — so
// a per-keystroke indexOf test never has to touch the DOM text or re-query the
// daemon. Mirrors extern.go's externSearchText: the server computes the
// haystack once per render and the template stamps it into data-search; the
// filter JS reads only that attribute. Kept to the fields a human would type to
// find a colleague, not the numeric stat chips (which carry no name/role token).
func (r peopleRow) SearchText() string {
	parts := make([]string, 0, 4+3*len(r.Identities))
	parts = append(parts, r.Display, r.Email, r.StatusMessage, r.Slug)
	for _, id := range r.Identities {
		parts = append(parts, id.Kind, id.Handle, id.Slug)
	}
	return strings.ToLower(strings.Join(parts, " "))
}

// peopleRecentBead is a compact chip view-model for one recently-created
// bead rendered under a people-row.
type peopleRecentBead struct {
	ID    string
	Title string
}

// peopleThreadMessage is the preview view-model for one recent thread-request
// message rendered under a people-row (ka-0000005614). Text is the truncated
// message body (the operator's ask); ThreadHref links to the full
// /threads/{thread_id} conversation; Ago is the humanized recency of the
// message. Truncated is true when the body was clipped so the template can
// append an ellipsis.
type peopleThreadMessage struct {
	// Text is the (possibly-truncated) message body — the human's ask, e.g.
	// "the users index page, sorry". Never empty (messages with no body are
	// skipped upstream).
	Text string
	// Truncated marks that Text was clipped at peopleMessagePreviewLen; the
	// template appends an ellipsis so a long ask reads as a preview.
	Truncated bool
	// ThreadHref is the /threads/{thread_id} link to the full conversation the
	// message belongs to. Empty when the message carries no thread_id (then the
	// preview renders as plain text with no link).
	ThreadHref string
	// Ago is the humanized "ago" recency of the message (created_at), e.g.
	// "3h" / "2d". Empty when the timestamp is unavailable.
	Ago string
}

// peopleIdentity is one operator identity folded into a peopleRow's human —
// the (kind, subject/handle) pair plus its /u/{slug} link. Primary marks the
// canonical login (the OIDC row when present) so the template can style it
// prominently; the rest are secondary handles rendered as labeled chips.
type peopleIdentity struct {
	// Kind is the identity provider — oidc, slack, gitlab, agent, …. Used as
	// the chip label so a human can tell a Slack U-id apart from a gitlab handle.
	Kind string
	// Handle is the provider-specific identifier that disambiguates this
	// identity from the human's others: the subject (a slack U-id, a gitlab
	// username, an agent name; for oidc the subject is the email), falling
	// back to the email, then the slug. Showing the subject — not the shared
	// canonical email — is what lets a human tell "slack U07…" apart from
	// "gitlab matthewbaker" when both fold into one person. Always non-empty.
	Handle string
	// Slug is the operator's canonical slug; UserHomeHref links to /u/{slug}.
	Slug         string
	UserHomeHref string
	// Primary is true for the canonical identity rendered as the row's main
	// login (exactly one per row).
	Primary bool
}

// peopleStats is the per-operator aggregate computed once per /people render.
// The count fields beyond OpenBeads / LastActive / Recent ride the §4.1
// superset GROUP BY query (ka-0000002003) on the SAME single daemon round-trip
// as OpenBeads — so /people stays at 3 bounded queries total regardless of
// operator count. Every count is zero when the operator has no matching work
// (rendered as an em-dash by the template).
type peopleStats struct {
	OpenBeads int
	// InProgress is the subset of OpenBeads that are status=in_progress
	// (actively-touched vs merely-open). Always ≤ OpenBeads.
	InProgress int
	// Created7d / Closed7d are throughput in/out over the trailing 7 days
	// (created_at window for Created7d; closed_at window for Closed7d).
	Created7d int
	Closed7d  int
	// MRsOpen / MRsMerged7d count type=merge-request beads — open/in_progress
	// now, and closed in the trailing 7 days respectively.
	MRsOpen     int
	MRsMerged7d int
	// DecisionsPending counts type=decision beads still open — "is someone
	// waiting on this person?".
	DecisionsPending int
	// AgentsActive / GardenersActive count the operator's open/in_progress
	// type=agent and type=trigger beads (fleet footprint). The row folds both
	// into one 🤖 chip; the header breaks gardeners out (§2.3).
	AgentsActive    int
	GardenersActive int
	LastActive      time.Time
	// Recent is the operator's most-recently-created beads, capped at
	// peopleRecentBeadCap. Ordered most-recent-first.
	Recent []peopleRecentBead
}

// peopleHeader is the top-level aggregate banner rendered above the directory
// (operator complaint: "no top level stats displayed"). Counts are computed
// once per render over the deduped row set — render-time sums of the per-row
// stats (§2.3), never a separate fleet-wide query, so the header can never
// disagree with the rows beneath it.
type peopleHeader struct {
	People     int // distinct humans after dedup (= number of rendered rows)
	Identities int // raw operator identities before dedup
	OpenBeads  int // total open/in_progress beads across all operators
	ActiveNow  int // rows with at least one open/in_progress bead
	// Fleet-throughput + backlog sums folded from the per-row stats (§2.3).
	Created7d        int // beads created across the fleet in the trailing 7d
	Closed7d         int // beads closed across the fleet in the trailing 7d
	MRsOpen          int // open merge-requests across all operators (review backlog)
	DecisionsPending int // pending decisions across all operators (action backlog)
	GardenersActive  int // active gardeners (type=trigger) across all operators
	// Machines is the count of machine (service / bridge / fleet / CI)
	// identities rendered in the collapsed "Service & machine accounts" section
	// below the human directory (ka-0000002163). NOT counted in People — the
	// header split (People vs Identities vs Machines) is what makes "these N
	// rows are humans, those M are bots" legible without expanding the section.
	Machines int
}

// machineRow is the lightweight view-model for one machine (service / bridge /
// fleet / CI) identity rendered in the collapsed "Service & machine accounts"
// section on /people (ka-0000002163). Deliberately thin — a machine identity is
// context, not a colleague, so it carries only what disambiguates it (display,
// kind, handle) plus an open-bead count and its /u/{slug} pivot. It is NOT a
// peopleRow: it is rendered with a distinct `machine-row` CSS class and
// plain-text (non-mailto, non-people-identity) identifiers so the human
// directory's row/header invariants (and the Users-CAMP visual audit that keys
// off people-row/people-identity/people-row-name/mailto) are untouched.
type machineRow struct {
	OperatorID   int64
	Slug         string
	Display      string
	Kind         string
	Handle       string // subject (else email) — the identifying handle
	UserHomeHref string // /u/{slug}, empty when the identity has no slug
	OpenBeads    int
}

// operatorGroup is one human after display-time dedup: a primary identity
// (the row we render) plus the secondary identities folded under it.
type operatorGroup struct {
	primary beadsapi.OperatorDirectoryEntry
	members []beadsapi.OperatorDirectoryEntry
}

// identities returns the FULL ordered identity set for the group: the primary
// (canonical) identity first, then every other folded identity. Always returns
// at least one entry (the primary) — even a singleton human renders its single
// identity chip so the directory consistently labels which handle each row is.
// basePath prefixes the /u/{slug} link so each chip points at the real
// per-user page.
func (g operatorGroup) identities(basePath string) []peopleIdentity {
	out := make([]peopleIdentity, 0, len(g.members))
	out = append(out, newPeopleIdentity(g.primary, basePath, true))
	for _, m := range g.members {
		if m.ID == g.primary.ID {
			continue
		}
		out = append(out, newPeopleIdentity(m, basePath, false))
	}
	return out
}

// canonicalEmail returns the human's primary email for prominent display:
// the primary identity's Email when set, else the first member with a non-empty
// Email, else the first @-bearing subject (oidc rows sometimes carry the email
// only in Subject). Empty when the group has no email-shaped identity at all
// (e.g. a pure agent/slack-handle human) — the template omits the email line.
func (g operatorGroup) canonicalEmail() string {
	if e := strings.TrimSpace(g.primary.Email); e != "" {
		return e
	}
	for _, m := range g.members {
		if e := strings.TrimSpace(m.Email); e != "" {
			return e
		}
	}
	for _, m := range g.members {
		if s := strings.TrimSpace(m.Subject); strings.Contains(s, "@") {
			return s
		}
	}
	return ""
}

// displayName resolves the group's human-facing heading, giving /people the
// same user-bead join /u/{slug} has (ka-0000006552). Resolution order:
//  1. a genuine name off any group member's user bead (profiles: display_name →
//     name, keyed by operator_id or normalized email) — the same head
//     displayNameFromUserBead reads, so the two surfaces agree;
//  2. any group member's non-empty directory Display — the OIDC primary chosen
//     by dedupOperators often has an empty Display while a sibling slack row
//     holds the real "First Last", so scanning members recovers the name without
//     a user bead;
//  3. the canonical email, then the primary slug — the same last resorts as
//     before, so a human with no name anywhere still renders reachably.
//
// This never renders a raw email while a real name exists anywhere in the
// group or its user beads (the Defect-1 regression).
func (g operatorGroup) displayName(profiles userProfiles) string {
	if n := g.resolvedName(profiles); n != "" {
		return n
	}
	return g.primary.Slug
}

// resolvedName is displayName without the terminal primary-slug fallback: it
// runs steps 1–3 of the resolution order (user-bead profile name → sibling
// directory Display → canonical email) and returns "" when the group has no
// name or email anywhere. displayName appends the slug fallback for /people
// (a card must always render SOMETHING); /u/{slug} calls this directly so it
// can supply the REQUESTED slug — not the group's primary slug — as its own
// fallback, preserving that page's long-standing "render the URL slug" contract
// (ka-0000001547). Splitting the terminal step out keeps one source of truth for
// the name join across both surfaces.
func (g operatorGroup) resolvedName(profiles userProfiles) string {
	if n := profiles.forOperator(g.primary).DisplayName; n != "" {
		return n
	}
	if n := strings.TrimSpace(g.primary.Display); n != "" {
		return n
	}
	for _, m := range g.members {
		if n := profiles.forOperator(m).DisplayName; n != "" {
			return n
		}
	}
	for _, m := range g.members {
		if n := strings.TrimSpace(m.Display); n != "" {
			return n
		}
	}
	return g.canonicalEmail()
}

// statusMessage resolves the group's status_message from any member's user bead
// (profiles, keyed by operator_id or normalized email). The former
// lookupStatusMessages keyed ONLY on the operator_id field, which no real user
// bead carries, so it returned "" for everyone — the email-indexed join surfaces
// the message /u/{slug} already shows (ka-0000006552, Defect 2). Returns "" when
// no member's profile carries one.
func (g operatorGroup) statusMessage(profiles userProfiles) string {
	if m := profiles.forOperator(g.primary).StatusMessage; m != "" {
		return m
	}
	for _, mem := range g.members {
		if m := profiles.forOperator(mem).StatusMessage; m != "" {
			return m
		}
	}
	return ""
}

// newPeopleIdentity builds one identity chip view-model from an operator
// directory entry. Handle prefers the subject — the provider-specific
// identifier (slack U-id, gitlab handle, agent name; an email for oidc rows)
// that distinguishes this identity from the human's others — then the email,
// then the slug as a last resort so a chip is never blank.
func newPeopleIdentity(op beadsapi.OperatorDirectoryEntry, basePath string, primary bool) peopleIdentity {
	handle := strings.TrimSpace(op.Subject)
	if handle == "" {
		handle = strings.TrimSpace(op.Email)
	}
	if handle == "" {
		handle = op.Slug
	}
	return peopleIdentity{
		Kind:         op.Kind,
		Handle:       handle,
		Slug:         op.Slug,
		UserHomeHref: basePath + "/u/" + op.Slug,
		Primary:      primary,
	}
}

// peopleIdentityKey derives the dedup key for an operator identity: a
// normalized email when present, else a normalized display name. Returns ""
// when neither is usable, which signals "do not group this row" (it stays a
// singleton keyed by its own id) — we never collapse two operators we can't
// positively tie to the same human.
func peopleIdentityKey(op beadsapi.OperatorDirectoryEntry) string {
	if e := strings.ToLower(strings.TrimSpace(op.Email)); e != "" {
		return "email:" + e
	}
	// Subjects of kind=oidc are emails too; some rows carry the email only
	// in Subject (no Email column). Treat an @-bearing subject as an email.
	if s := strings.ToLower(strings.TrimSpace(op.Subject)); strings.Contains(s, "@") {
		return "email:" + s
	}
	if d := strings.ToLower(strings.TrimSpace(op.Display)); d != "" {
		return "name:" + d
	}
	return ""
}

// dedupOperators collapses the raw operator directory into one group per
// human, keyed by peopleIdentityKey. Rows with no usable key stay as their
// own singleton group (keyed by id) so we never over-merge. Input order is
// preserved for the first-seen member of each group; the primary is chosen
// as the OIDC identity when the group has one (it's the canonical human
// login), else the first-seen member.
func dedupOperators(ops []beadsapi.OperatorDirectoryEntry) []operatorGroup {
	order := make([]string, 0, len(ops))
	byKey := make(map[string]*operatorGroup, len(ops))
	for _, op := range ops {
		key := peopleIdentityKey(op)
		if key == "" {
			key = "id:" + strconv.FormatInt(op.ID, 10)
		}
		g, ok := byKey[key]
		if !ok {
			g = &operatorGroup{primary: op}
			byKey[key] = g
			order = append(order, key)
		}
		g.members = append(g.members, op)
		// Prefer an OIDC identity as the rendered primary — it's the human's
		// canonical login, and its slug/email is the most stable.
		if op.Kind == "oidc" && g.primary.Kind != "oidc" {
			g.primary = op
		}
	}
	out := make([]operatorGroup, 0, len(order))
	for _, key := range order {
		out = append(out, *byKey[key])
	}
	return out
}

// resolveOperatorGroupName resolves one operator's human-facing name the SAME
// way /people resolves a card heading — it folds the operator's sibling
// directory identities (dedupOperators) and their user-bead profiles
// (lookupUserProfiles) into one operatorGroup and reads operatorGroup.resolvedName.
// This is the join that keeps /u/{slug} and the /people card from ever stating
// two different names for one person (ka-0000001547).
//
// Before this, /people got the sibling+profile join (MR !6238, ka-0000006552)
// but /u/{slug} still resolved its heading from a single user bead
// (displayNameFromUserBead), which cannot see a name that lives only on a
// sibling slack row's directory Display. So a human whose real name is on that
// sibling row (op-240 → "Micah Yates") — or whose OIDC primary carries an
// empty Display while the name sits on a sibling — rendered the raw slug/op-id
// as the h1 even though /people showed the resolved name.
//
// Returns "" when the operator is not a human in the directory (agent/sentinel
// kinds, dropped by filterHumanOperators), the group carries no name/email
// anywhere, or the directory is unavailable — the caller then keeps its own
// single-user-bead → slug fallback, which is the right behavior for pages
// /people never lists. resolvedName is used (not displayName) so a nameless
// group yields "" and the caller can fall back to the REQUESTED slug rather
// than the group's primary slug. lookupUserProfiles (the second round-trip) is
// issued only after a group match, so non-human and unknown ids cost just the
// one ListOperators call.
func (s *Server) resolveOperatorGroupName(ctx context.Context, operatorID int64) string {
	resp, err := s.daemon.ListOperators(ctx, peopleListLimit)
	if err != nil || resp == nil {
		return ""
	}
	groups := dedupOperators(filterHumanOperators(resp.Operators))
	for i := range groups {
		for _, m := range groups[i].members {
			if m.ID == operatorID {
				return groups[i].resolvedName(s.lookupUserProfiles(ctx))
			}
		}
	}
	return ""
}

// handlePeople renders GET /people — the public operator directory
// (M2 §4, ka-0000000340). Lists every operator that has a canonical slug
// with their display name, kind, status_message (from the user bead), and a
// link to /u/{slug}.
func (s *Server) handlePeople(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	resp, err := s.daemon.ListOperators(ctx, peopleListLimit)
	if err != nil {
		s.logger.Error("people: list operators failed", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	profiles := s.lookupUserProfiles(ctx)
	stats := s.lookupOperatorStats(ctx)
	// One bounded plan-universe scan grouped by creator email (§4 Step 4,
	// ka-0000000236) — NOT a per-row involvement fan-out. See
	// lookupPlanCountsByEmail for why the count is the created-floor and the chip
	// links to the involved set.
	planCounts := s.lookupPlanCountsByEmail(ctx)
	// One bounded kind=slack alias scan (ka-0000003119) → an id→@name resolver so
	// the "recent requests" snippets below render `<@U…>` mention tokens as the
	// operator's @name instead of leaking the raw Slack id. Same single-round-trip
	// posture as the other page-level lookups; a nil resolver (load error) simply
	// leaves the tokens raw.
	mentionResolver := s.lookupSlackMentionResolver(ctx)
	// One bounded thread-request-message scan grouped by operator (ka-0000005614)
	// — the "recent requests" preview strip under each card. Same single-round-trip
	// posture as the stats + plan-count lookups above; folded across a human's
	// grouped identities below. Slack mention tokens in each snippet are resolved
	// through mentionResolver BEFORE truncation so a resolved @name is never
	// clipped mid-id.
	recentMessages := s.lookupRecentThreadMessages(ctx, mentionResolver)

	// Drop sentinel/bridge identities (system, fleet, gitlab, jira, agent,
	// email) — those belong on /fleet, not on the human directory. Without
	// this filter every robot with a slug renders as its own row and skews
	// the header aggregates (op-9 alone owns ~240k "open beads" because
	// every unattributed legacy bead is bound to it).
	humans := filterHumanOperators(resp.Operators)

	// Collapse the raw operator identities into one row per human. Each
	// human's OIDC/Slack/agent rows share an identity key (normalized email,
	// else display name) so they fold together instead of rendering as
	// separate dupe rows — the operator's "lots of dupe users" complaint.
	// This is a render-time grouping only; the underlying operator rows are
	// untouched (no destructive link-aliases merge).
	groups := dedupOperators(humans)
	rows := make([]peopleRow, 0, len(groups))
	for _, g := range groups {
		primary := g.primary
		// Resolve the human's heading through the SAME user-bead join /u/{slug}
		// uses (profiles, keyed by operator_id AND normalized email), so /people
		// and the per-user page state the same name for one person. The chosen
		// OIDC primary often carries an empty Display while a sibling slack row
		// (or the user bead) holds the real "First Last" — without this join the
		// row led with a raw email/handle (ka-0000006552, Defect 1).
		display := g.displayName(profiles)
		// Aggregate stats across every identity in the group so the count
		// reflects the whole human, not just their primary alias. RecentBeads
		// are concatenated across members (each member's list is already
		// most-recent-first) and capped at peopleRecentBeadCap.
		var agg peopleStats
		var last time.Time
		var recent []peopleRecentBead
		var recentMsgs []peopleThreadMessage
		for _, op := range g.members {
			st := stats[op.ID]
			agg.OpenBeads += st.OpenBeads
			agg.InProgress += st.InProgress
			agg.Created7d += st.Created7d
			agg.Closed7d += st.Closed7d
			agg.MRsOpen += st.MRsOpen
			agg.MRsMerged7d += st.MRsMerged7d
			agg.DecisionsPending += st.DecisionsPending
			agg.AgentsActive += st.AgentsActive
			agg.GardenersActive += st.GardenersActive
			if st.LastActive.After(last) {
				last = st.LastActive
			}
			recent = append(recent, st.Recent...)
			recentMsgs = append(recentMsgs, recentMessages[op.ID]...)
		}
		if len(recent) > peopleRecentBeadCap {
			recent = recent[:peopleRecentBeadCap]
		}
		// Thread-request previews are already most-recent-first PER member (the
		// scan is sorted -created_at). Concatenating members loses the strict
		// cross-member ordering, but a human's aliases almost never overlap in
		// time and the strip is a teaser, not a merged feed — cap it and move on.
		if len(recentMsgs) > peopleRecentMessageCap {
			recentMsgs = recentMsgs[:peopleRecentMessageCap]
		}
		// Plans-count teaser + involved-set pivot (§4 Step 4, ka-0000000236). The
		// count is looked up by the group's canonical email (the same key
		// lookupPlanCountsByEmail folds plans under); the chip links to the
		// person's INVOLVED plans via ?operated_by=<canonical oidc alias>, so the
		// number is a created-floor and the click resolves Step 1's involvement
		// union live. No alias (a human with no email) → no link.
		email := g.canonicalEmail()
		var plansCount int
		var plansHref string
		if email != "" {
			plansCount = planCounts[strings.ToLower(strings.TrimSpace(email))]
			if alias := canonicalOIDC(email); alias != "" {
				plansHref = s.basePath + "/plans?operated_by=" + url.QueryEscape(alias)
			}
		}
		rows = append(rows, peopleRow{
			OperatorID:       primary.ID,
			Slug:             primary.Slug,
			Display:          display,
			Email:            g.canonicalEmail(),
			Kind:             primary.Kind,
			StatusMessage:    g.statusMessage(profiles),
			UserHomeHref:     s.basePath + "/u/" + primary.Slug,
			HoverHref:        s.basePath + "/htmx/operator-hover/" + primary.Slug,
			OpenBeads:        agg.OpenBeads,
			InProgress:       agg.InProgress,
			Created7d:        agg.Created7d,
			Closed7d:         agg.Closed7d,
			MRsOpen:          agg.MRsOpen,
			MRsMerged7d:      agg.MRsMerged7d,
			DecisionsPending: agg.DecisionsPending,
			AgentsActive:     agg.AgentsActive,
			GardenersActive:  agg.GardenersActive,
			FleetActive:      agg.AgentsActive + agg.GardenersActive,
			PlansCount:       plansCount,
			PlansHref:        plansHref,
			LastActiveAgo:    formatAgo(last),
			RecentBeads:      recent,
			RecentMessages:   recentMsgs,
			Identities:       g.identities(s.basePath),
			groupLastActive:  last,
		})
	}
	// Sort by activity recency (most recent first), then by display so
	// inactive operators land in a stable alphabetic tail. The default slug
	// order is fine for a small directory; once stats are visible, "who's
	// active right now" is the more useful default for a check-in surface.
	sort.SliceStable(rows, func(i, j int) bool {
		li := rows[i].groupLastActive
		lj := rows[j].groupLastActive
		if !li.Equal(lj) {
			return li.After(lj)
		}
		return strings.ToLower(rows[i].Display) < strings.ToLower(rows[j].Display)
	})
	header := peopleHeader{
		People:     len(rows),
		Identities: len(humans),
	}
	for i := range rows {
		header.OpenBeads += rows[i].OpenBeads
		if rows[i].OpenBeads > 0 {
			header.ActiveNow++
		}
		header.Created7d += rows[i].Created7d
		header.Closed7d += rows[i].Closed7d
		header.MRsOpen += rows[i].MRsOpen
		header.DecisionsPending += rows[i].DecisionsPending
		header.GardenersActive += rows[i].GardenersActive
	}

	// Machine (service / bridge / fleet / CI) identities used to be silently
	// dropped by filterHumanOperators — invisible on /people (ka-0000002163).
	// Surface them in a separate collapsed section below the human directory,
	// classified by the exact complement of the human filter, so the roster
	// stays all-human while the machines are still one click away and counted.
	// No dedup/group fold: each machine identity is distinct scaffolding, not a
	// person with multiple aliases, so it renders one row per identity.
	machines := filterMachineOperators(resp.Operators)
	machineRows := make([]machineRow, 0, len(machines))
	for _, op := range machines {
		handle := strings.TrimSpace(op.Subject)
		if handle == "" {
			handle = strings.TrimSpace(op.Email)
		}
		display := strings.TrimSpace(op.Display)
		if display == "" {
			display = handle
		}
		if display == "" {
			display = "op-" + strconv.FormatInt(op.ID, 10)
		}
		mr := machineRow{
			OperatorID: op.ID,
			Slug:       op.Slug,
			Display:    display,
			Kind:       op.Kind,
			Handle:     handle,
			OpenBeads:  stats[op.ID].OpenBeads,
		}
		if op.Slug != "" {
			mr.UserHomeHref = s.basePath + "/u/" + op.Slug
		}
		machineRows = append(machineRows, mr)
	}
	// Sort most-open-beads first (the legacy-pre-attribution sink surfaces at
	// the top where it's obvious), then by kind, then display — stable and
	// independent of the daemon's slug order.
	sort.SliceStable(machineRows, func(i, j int) bool {
		if machineRows[i].OpenBeads != machineRows[j].OpenBeads {
			return machineRows[i].OpenBeads > machineRows[j].OpenBeads
		}
		if machineRows[i].Kind != machineRows[j].Kind {
			return machineRows[i].Kind < machineRows[j].Kind
		}
		return strings.ToLower(machineRows[i].Display) < strings.ToLower(machineRows[j].Display)
	})
	header.Machines = len(machineRows)
	// Collapse the dormant tail (ka-0000004086). The shipped de-emphasis
	// (ka-0000005627, IsQuiet → data-quiet) dims fully-dormant cards in place,
	// but they still occupy the primary above-the-fold scan and dilute the
	// signal-per-pixel of active colleagues. Partition the already-sorted rows
	// on the SAME IsQuiet predicate so the dormant operators render inside a
	// collapsed <details> section below the active directory — reordering +
	// collapse, not a hide: every dormant operator stays counted in the header,
	// reachable one click away, and the primary grid is now all-active rows.
	// Order within each partition is preserved (rows is activity-sorted), so the
	// active grid still leads with the most-recently-active colleague.
	active := make([]peopleRow, 0, len(rows))
	quiet := make([]peopleRow, 0)
	for _, row := range rows {
		if row.IsQuiet() {
			quiet = append(quiet, row)
			continue
		}
		active = append(active, row)
	}
	data := map[string]any{
		"Title":        "Users",
		"Rows":         active,
		"QuietRows":    quiet,
		"Count":        len(rows),
		"QuietCount":   len(quiet),
		"Header":       header,
		"MachineRows":  machineRows,
		"MachineCount": len(machineRows),
	}
	s.render(w, r, "people.html", data)
}

// handleOperatorHoverCard renders GET /htmx/operator-hover/{slug} — the
// reusable hover-card preview partial. Wherever an operator chip appears,
// the chip can attach `hx-get="/htmx/operator-hover/{slug}"` to surface the
// same compact card the directory uses (M2 §4 acceptance criterion: reusable
// hover-card preview component).
func (s *Server) handleOperatorHoverCard(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSpace(r.PathValue("slug"))
	if slug == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	ctx := r.Context()
	resp, err := s.daemon.LookupOperatorBySlug(ctx, slug)
	if err != nil {
		s.logger.Error("people: lookup operator by slug failed", "slug", slug, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if !resp.Found {
		http.NotFound(w, r)
		return
	}
	// Pull the full directory feed once and pick the matching row. The
	// directory query is small (slug-only operators, ≤500 rows) and the
	// hover card needs the same fields anyway — going through ListOperators
	// keeps the row view-model identical to the page.
	list, err := s.daemon.ListOperators(ctx, peopleListLimit)
	if err != nil {
		s.logger.Error("people: list operators (hover) failed", "slug", slug, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	// Resolve the requested slug through the SAME dedup grouping the directory
	// uses, then render the group it belongs to — so the hover card shows the
	// human's full identity set and canonical email exactly like the page row
	// (one source of truth), regardless of which alias slug was hovered. Sentinel
	// kinds are filtered out before grouping (matches handlePeople), so a
	// /fleet sentinel slug 404s here rather than rendering a human card.
	var match *operatorGroup
	groups := dedupOperators(filterHumanOperators(list.Operators))
	for i := range groups {
		for _, m := range groups[i].members {
			if m.Slug == slug {
				match = &groups[i]
				break
			}
		}
		if match != nil {
			break
		}
	}
	if match == nil {
		http.NotFound(w, r)
		return
	}
	primary := match.primary
	// Resolve heading + status through the same user-bead join the directory
	// row uses, so the hover card never disagrees with the page it previews.
	profiles := s.lookupUserProfiles(ctx)
	row := peopleRow{
		OperatorID:    primary.ID,
		Slug:          primary.Slug,
		Display:       match.displayName(profiles),
		Email:         match.canonicalEmail(),
		Kind:          primary.Kind,
		StatusMessage: match.statusMessage(profiles),
		UserHomeHref:  s.basePath + "/u/" + primary.Slug,
		HoverHref:     s.basePath + "/htmx/operator-hover/" + primary.Slug,
		Identities:    match.identities(s.basePath),
	}
	s.renderPartial(w, r, "templates/partials/operator_hover_card.html", "operator_hover_card", map[string]any{
		"Row":      row,
		"BasePath": s.basePath,
	})
}

// userProfile is the display name + status_message resolved from one operator's
// type=user kind=config bead — the same source /u/{slug} reads (statusMessageFromBead
// and the display_name→name head of displayNameFromUserBead). /people joins it in so
// the directory states the same name and surfaces the same status text the per-user
// page does (ka-0000006552).
type userProfile struct {
	// DisplayName is a GENUINE name off the user bead (display_name → name), or
	// "" when the bead carries neither. Deliberately NOT displayNameFromUserBead's
	// email tail: at the /people group level the email/slug fallback is handled
	// uniformly by operatorGroup.displayName's later steps, so letting an
	// email-only user bead resolve here would wrongly preempt a sibling directory
	// row's real "First Last".
	DisplayName string
	// StatusMessage is the trimmed status_message field, or "" when unset.
	StatusMessage string
}

// userProfiles indexes resolved user-bead profiles by BOTH operator_id and
// normalized email, so /people can join a profile whether or not the user bead
// carries an operator_id field. In practice user beads are provisioned with
// `email` and no `operator_id` (see lookupUserBeadByOperator's comment), so the
// email index is the load-bearing path; the operator_id index is kept for beads
// that do set it.
type userProfiles struct {
	byOperatorID map[int64]userProfile
	byEmail      map[string]userProfile
}

// forOperator resolves the profile for one operator identity, preferring the
// operator_id key, then the operator's normalized email. Returns a zero
// userProfile (both fields "") when neither key matches.
func (p userProfiles) forOperator(op beadsapi.OperatorDirectoryEntry) userProfile {
	if op.ID > 0 {
		if pr, ok := p.byOperatorID[op.ID]; ok {
			return pr
		}
	}
	for _, e := range []string{op.Email, op.Subject} {
		key := normalizeProfileEmail(e)
		if key == "" {
			continue
		}
		if pr, ok := p.byEmail[key]; ok {
			return pr
		}
	}
	return userProfile{}
}

// normalizeProfileEmail lowercases + trims an email-shaped string, returning ""
// when it is not @-bearing (so a non-email subject never becomes a spurious key).
func normalizeProfileEmail(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if !strings.Contains(s, "@") {
		return ""
	}
	return s
}

// lookupUserProfiles reads type=user kind=config beads ONCE and returns the
// display name + status_message per operator, indexed by operator_id and by
// normalized email. This is the same single page-level round-trip the former
// lookupStatusMessages issued — /people already fetches exactly these beads —
// but it now ALSO carries the resolved display name and an email index, giving
// /people the same user-bead join /u/{slug} has (ka-0000006552). Without it the
// directory rendered a raw email/handle for humans whose chosen primary row has
// an empty Display (Defect 1), and status_message never surfaced because the
// operator_id join key is absent on every real user bead (Defect 2). Returns an
// empty (non-nil) index on any error so the directory degrades gracefully.
func (s *Server) lookupUserProfiles(ctx context.Context) userProfiles {
	out := userProfiles{
		byOperatorID: map[int64]userProfile{},
		byEmail:      map[string]userProfile{},
	}
	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"user"},
		Kinds: []string{"config"},
		Limit: 500,
	})
	if err != nil || res == nil {
		return out
	}
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		pr := userProfile{
			// Genuine name only (display_name → name); the email/slug fallback is
			// the group's job, not the profile's (see userProfile.DisplayName).
			DisplayName:   firstNonEmptyField(b.Fields, "display_name", "name"),
			StatusMessage: statusMessageFromBead(b),
		}
		if pr.DisplayName == "" && pr.StatusMessage == "" {
			continue
		}
		if idStr := strings.TrimSpace(b.Fields["operator_id"]); idStr != "" {
			if id, ok := parseInt64(idStr); ok {
				out.byOperatorID[id] = pr
			}
		}
		if key := normalizeProfileEmail(b.Fields["email"]); key != "" {
			out.byEmail[key] = pr
		}
	}
	return out
}

// lookupOperatorStats returns a map of operator_id → aggregate for the
// /people directory, computed server-side by the daemon (ka-0000001390).
//
// The daemon runs a real GROUP BY operated_by_id over the WHOLE beads table
// for the open/in_progress count + max(updated_at) last-active recency — no
// in-memory sampling. (Last-active spans all statuses, so operators with only
// closed work still sort by genuine activity rather than tying at the zero
// value.) The per-row "recently created" chips come from a bounded windowed
// query keyed on created_by_operator_id (the immortal originator, distinct
// from operated_by_id after a dibs handoff), capped at peopleRecentBeadCap
// per operator with peopleStatsRecentCeiling as a hard ceiling on the total
// chip rows. agent/session scaffolding is excluded daemon-side.
//
// Alias-awareness: operated_by_id / created_by_operator_id are the canonical
// operator the write-path bound through operator_aliases, so every linked
// identity already folds into one row here; the render-time group fold in
// handlePeople sums any still-unmerged duplicate identities for display.
//
// Returns an empty map on any error — the directory degrades to "no stats
// shown" rather than failing the whole page.
func (s *Server) lookupOperatorStats(ctx context.Context) map[int64]peopleStats {
	out := map[int64]peopleStats{}
	resp, err := s.daemon.OperatorStats(ctx, peopleRecentBeadCap, peopleStatsRecentCeiling, int(peopleActivityWindow.Hours()))
	if err != nil || resp == nil {
		return out
	}
	for _, st := range resp.Stats {
		if st.OperatorID <= 0 {
			continue
		}
		ps := out[st.OperatorID]
		ps.OpenBeads = st.OpenBeads
		ps.InProgress = st.InProgress
		ps.Created7d = st.Created7d
		ps.Closed7d = st.Closed7d
		ps.MRsOpen = st.MRsOpen
		ps.MRsMerged7d = st.MRsMerged7d
		ps.DecisionsPending = st.DecisionsPending
		ps.AgentsActive = st.AgentsActive
		ps.GardenersActive = st.GardenersActive
		ps.LastActive = parsePeopleStatsTime(st.LastActive)
		out[st.OperatorID] = ps
	}
	// Recent chips arrive already ordered (created_at desc within an operator)
	// and capped per operator by the daemon, so append in order.
	for _, rb := range resp.Recent {
		if rb.OperatorID <= 0 {
			continue
		}
		ps := out[rb.OperatorID]
		ps.Recent = append(ps.Recent, peopleRecentBead{ID: rb.BeadID, Title: strings.TrimSpace(rb.Title)})
		out[rb.OperatorID] = ps
	}
	return out
}

// lookupRecentThreadMessages returns a map of operator_id → that operator's
// most-recent thread-request messages, capped at peopleRecentMessageCap each
// (ka-0000005614). Backing the "recent requests" strip under each /people card
// so a glance shows what each human is asking for.
//
// Like the stats and plan-count lookups this is ONE bounded page-level
// round-trip — NOT a per-row fan-out. It pulls the most-recently-created
// type=message slack-thread-message beads (peopleRecentMessagesScanLimit as a
// hard ceiling), then groups them by operated_by_id in memory, keeping the
// first peopleRecentMessageCap per operator (the query is sorted -created_at,
// so first-seen == most-recent). handlePeople folds these across a human's
// grouped identities exactly like the recent-beads chips.
//
// Keying on operated_by_id (the attributed author) mirrors how the message
// beads are written by the slack-bridge — the human whose ask this is. Returns
// an empty map on any error so the directory degrades to "no requests shown"
// rather than failing the page.
func (s *Server) lookupRecentThreadMessages(ctx context.Context, mentions slackMentionResolver) map[int64][]peopleThreadMessage {
	out := map[int64][]peopleThreadMessage{}
	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:  []string{"message"},
		Labels: []string{"slack-thread-message"},
		Sort:   "-created_at",
		Limit:  peopleRecentMessagesScanLimit,
		// Age-bound the scan (ka-0000001991). Two things break without it.
		// (a) Correctness: an ask from five weeks ago rendered on the card as
		// though it were current — the operator's "old threads seem to remain
		// current despite irrelevance". (b) Fairness: the scan is a single
		// newest-first page shared by everyone, so once the population
		// outgrows the limit one high-volume account eats the whole page and
		// starves every other human's strip. Bounding by age keeps the page
		// well clear of the ceiling AND makes what survives genuinely recent.
		CreatedAfter: time.Now().Add(-peopleActivityWindow),
	})
	if err != nil || res == nil {
		return out
	}
	for _, b := range res.Beads {
		if b == nil || b.OperatedByID == nil {
			continue
		}
		opID := *b.OperatedByID
		if opID <= 0 {
			continue
		}
		if len(out[opID]) >= peopleRecentMessageCap {
			continue
		}
		msg, ok := newPeopleThreadMessage(s.basePath, b, mentions)
		if !ok {
			continue
		}
		out[opID] = append(out[opID], msg)
	}
	return out
}

// newPeopleThreadMessage builds one thread-request preview view-model from a
// type=message bead. The body is read from fields.text (the literal Slack
// message the operator sent) with the title as a fallback for older beads that
// stored the ask only in the title. Raw Slack `<@U…>` mention tokens in the
// body are resolved to `@Name` via the resolver BEFORE truncation, so the
// preview never leaks an opaque id and never clips a resolved name mid-token
// (ka-0000003119). Returns ok=false when the bead carries no usable body —
// such a message contributes nothing to the strip.
func newPeopleThreadMessage(basePath string, b *beadsapi.BeadDetail, mentions slackMentionResolver) (peopleThreadMessage, bool) {
	body := strings.TrimSpace(b.Fields["text"])
	if body == "" {
		body = strings.TrimSpace(b.Title)
	}
	// Resolve `<@U…>` mention tokens to @names on the RAW body, before the
	// truncate below, so the visible preview clips a resolved @name rather than
	// a half-eaten Slack id.
	body = resolveSlackMentions(body, mentions)
	// Collapse internal newlines so a multi-line Slack message renders as one
	// compact preview line rather than blowing out the card height.
	body = strings.Join(strings.Fields(body), " ")
	if body == "" {
		return peopleThreadMessage{}, false
	}
	truncated := false
	if len(body) > peopleMessagePreviewLen {
		body = truncate(body, peopleMessagePreviewLen)
		truncated = true
	}
	var href string
	if tid := strings.TrimSpace(b.Fields[threadIDField]); tid != "" {
		href = basePath + "/threads/" + tid
	}
	return peopleThreadMessage{
		Text:       body,
		Truncated:  truncated,
		ThreadHref: href,
		Ago:        formatAgo(b.CreatedAt),
	}, true
}

// lookupPlanCountsByEmail returns a map of normalized-creator-email → number of
// ACTIVE plans that human CREATED, computed from a SINGLE bounded plan-universe
// scan (§4 Step 4, ka-0000000236). This is the whole point of the "no per-row
// fan-out" constraint: rather than run the 4-facet involvement query
// (plansInvolvingOperator) once per directory row — up to peopleListLimit
// operators — we scan the active plan universe ONCE, the same
// Types=[epic]/Labels=[plan]/active query /plans itself issues, and group by the
// creator's email. The per-row chip then LINKS to the person's involved set
// (PlansHref → ?operated_by=<alias>), so the count is a created-floor teaser and
// the involvement resolution happens lazily on the click — mirroring Step 5's
// browse-by-person strip (buildPlanPeopleStrip).
//
// Keying is email-first (lowercased OperatedByEmail, else the email parsed out of
// an oidc:<email> operated_by id), matching peopleIdentityKey's fold so a plan
// created under any of a human's aliases lands on the same row as the directory
// groups them. Non-human plan owners (machine sentinels, agent-created, unowned)
// are skipped via the same bridge.IsHumanOperator predicate Step 5 uses — the
// directory counts PEOPLE's plans. Returns an empty map on any error so the
// directory degrades to "no plans shown" rather than failing the page.
func (s *Server) lookupPlanCountsByEmail(ctx context.Context) map[string]int {
	out := map[string]int{}
	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:            []string{"epic"},
		Labels:           []string{planLabel},
		Statuses:         planStatusEnum(planStatusActive),
		Sort:             "-created_at",
		ExcludeScheduled: true,
		Limit:            plansListLimit,
	})
	if err != nil || res == nil {
		return out
	}
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		if !bridge.IsHumanOperator(strings.TrimSpace(b.OperatedBy)) {
			continue
		}
		key := planCreatorEmailKey(b)
		if key == "" {
			continue
		}
		out[key]++
	}
	return out
}

// planCreatorEmailKey derives the normalized email fold key for a plan epic's
// human creator: lowercased OperatedByEmail when the projection carries one, else
// the email inside an oidc:<email> operated_by id. Returns "" when neither is
// present (a pure slack/agent owner with no email) — such a plan simply doesn't
// contribute to any directory row's count, an accepted cosmetic edge that never
// produces a wrong count. Mirrors peopleIdentityKey's email-first fold so the
// key matches the row's canonicalEmail() lookup exactly.
func planCreatorEmailKey(b *beadsapi.BeadDetail) string {
	if e := strings.ToLower(strings.TrimSpace(b.OperatedByEmail)); e != "" {
		return e
	}
	if id := strings.ToLower(strings.TrimSpace(b.OperatedBy)); strings.HasPrefix(id, "oidc:") {
		return strings.TrimPrefix(id, "oidc:")
	}
	return ""
}

// parsePeopleStatsTime parses the daemon's RFC3339 last-active string. The
// daemon emits the zero time ("0001-01-01T00:00:00Z") for an operator with no
// activity; an unparseable or zero value yields the Go zero time, which the
// caller renders as "no activity" / sorts to the alphabetic tail.
func parsePeopleStatsTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// parseInt64 is a minimal strconv.ParseInt wrapper that returns ok=false on
// any error or non-positive id. Operator ids are bigserial > 0 in the
// daemon's schema — anything outside that range is data we can ignore.
func parseInt64(s string) (int64, bool) {
	var n int64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
	}
	if n <= 0 {
		return 0, false
	}
	return n, true
}
