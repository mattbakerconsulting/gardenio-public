package bridge

// jira_backlog_pick.go — the BACKLOG-MODE ticket picker (task ka-0000007742,
// plan ka-0000000093 / narrative ka-0000005434).
//
// # Why this exists
//
// The LIVE fsm-triage trigger (ka-0000005997) selects work with the JQL
// `project = FSM AND created >= -2d` — a NEW-ticket trigger by construction. It
// never covered the 88 pre-go-live FSM tickets (created before 2026-07-20) that
// were open when the pipeline went live (bug ka-0000003294). Backlog mode is the
// INVERSE of that live picker: instead of a rolling created-window, it walks an
// EXPLICIT worklist — the FSM keys named in a released wave step — and, for each,
// consults live Jira to skip anything already triaged.
//
// # The three rules this picker enforces (task acceptance criteria)
//
//  1. WALK AN EXPLICIT WORKLIST, not a time window. The wave step's title/body
//     names its exact FSM keys (e.g. "Wave B — crf-display (9): FSM-360,361,…").
//     ParseWaveKeys extracts that named set; the picker never runs its own
//     `created >= -Nd` clause and never reaches outside the named set. This is
//     the explicit rejection of the old nightly burn loop's `createdAfter -30d`
//     filter, which excluded 55 of the 88 backlog tickets (ka-0000004347).
//
//  2. DEDUP ON THE gardenio-triaged JIRA LABEL — the outbox sentinel is the
//     SOURCE OF TRUTH, read live from Jira, NOT a bead scan. The GFO outbox is
//     what actually stamps `gardenio-triaged` on a target FSM ticket when a triage
//     publishes; reading that label back from Jira is therefore the authoritative
//     "already triaged" signal. A bead scan would miss the three FSM-219/259/318
//     tickets triaged by hand during development (they carry the label in Jira but
//     have no matching in-fleet triage bead) and would drift from what the operator
//     sees on the ticket. isTriageLabel's `gardenio-triaged` case (jira_sync.go) is
//     reused so the sentinel spelling can never diverge between writer and reader.
//
//  3. ORDER OLDEST-FIRST WITHIN THE WAVE. Theme-batched drain: the FIRST ticket
//     in a wave does the expensive root-cause work and later tickets confirm-or-
//     differentiate against it (plan ka-0000005434). Oldest-first is the default
//     anchor — the pilot deliberately took the OLDEST tickets as the hardest case.
//     Ties (equal/absent created) fall back to numeric key order so the pick is
//     deterministic and a re-run of a partial wave picks the same next ticket.
//
// # Idempotency
//
// Idempotency is already verified safe end-to-end: 0 duplicate FSM targets across
// 60 GBFSM outbox tickets, so re-running a partial wave is not harmful. This picker
// still SKIPS already-triaged tickets — not for safety, but to avoid paying the
// per-ticket triage cost twice ($2.84 p50 / $8.67 p90 per ticket).
//
// # Purity
//
// SelectFromIssues is a PURE function over a slice of JiraIssue — no I/O — so the
// selection rules are fully unit-testable without a live Jira. The live wiring
// (fetch the wave's keys from Jira, then select) lives in BacklogPick, which is a
// thin adapter the `gb jira backlog-pick` command drives on project=monorepo where
// the JIRA read creds live (advice ka-0000005529).

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// fsmKeyRe matches an FSM issue key (case-insensitive) anywhere in text. Used to
// extract the named worklist from a wave step's title/description, which write
// the keys as a comma-separated run ("FSM-360,361,395" or "FSM-360, FSM-361").
// The leading FSM- is required on the first key of a run; ParseWaveKeys stitches
// bare numeric continuations ("…,361,395") back onto the FSM prefix.
var fsmKeyRe = regexp.MustCompile(`(?i)\bFSM-(\d+)`)

// fsmKeyNumRe matches a bare or FSM-prefixed number token so a comma-separated
// run like "FSM-360, 361, 395" is fully recovered — the wave titles abbreviate
// after the first key. It is applied ONLY to the text after the first FSM- match
// in a run (see ParseWaveKeys) so unrelated numbers elsewhere are never captured.
var fsmKeyNumRe = regexp.MustCompile(`(?i)(?:FSM-)?(\d+)`)

// BacklogTriagedLabel is the Jira label the GFO outbox stamps on a target FSM
// ticket when a triage publishes — the "already triaged" sentinel and the
// authoritative dedup signal for backlog mode (task ka-0000007742). It is the
// SAME constant recognized by isTriageLabel (jira_sync.go), so the writer (outbox)
// and this reader can never disagree on the spelling.
const BacklogTriagedLabel = "gardenio-triaged"

// BacklogPickResult is the outcome of one backlog-mode pick over a wave's named
// FSM keys. It reports both the single Pick (the ticket the caller should triage
// next) and the full partition (Untriaged / Skipped / Missing) so the caller can
// print an auditable one-liner and detect a drained or misconfigured wave.
type BacklogPickResult struct {
	// Wave is the wave label the keys were parsed from, when known (e.g. "B").
	// Informational only — empty when the caller passed keys directly.
	Wave string `json:"wave"`
	// Pick is the FSM key to triage next: the oldest un-triaged key in the wave,
	// ties broken by numeric key order. Empty when the wave is fully drained
	// (every named key already carries gardenio-triaged) — the caller should treat
	// an empty Pick as "wave dry, nothing to do".
	Pick string `json:"pick"`
	// Untriaged is every named key that does NOT yet carry gardenio-triaged,
	// oldest-first (the same order Pick is drawn from). Pick == Untriaged[0].
	Untriaged []string `json:"untriaged"`
	// Skipped is every named key that ALREADY carries gardenio-triaged — deduped
	// out to avoid paying twice. Sorted by numeric key for a stable audit line.
	Skipped []string `json:"skipped"`
	// Missing is every named key for which Jira returned no issue (typo in the
	// wave title, or a ticket deleted/moved out of FSM). Surfaced rather than
	// silently dropped so a wave-authoring mistake is visible, not a silent
	// under-cover. Sorted by numeric key.
	Missing []string `json:"missing"`
}

// ParseWaveKeys extracts the ordered, de-duplicated set of FSM keys named in a
// wave step's text (title and/or description). It honors WAVE SCOPING: the
// returned set is exactly the keys the wave names — nothing inferred, nothing
// widened. The wave titles write keys as a comma-separated run that abbreviates
// after the first ("FSM-360,361,395,398" or "FSM-360, FSM-361, FSM-395"), so
// ParseWaveKeys recovers bare numeric continuations back onto the FSM- prefix.
//
// Order of first appearance is preserved (callers may want the title's ordering),
// and duplicates across title+body are collapsed. Returns nil when no FSM key is
// present.
func ParseWaveKeys(text string) []string {
	var keys []string
	seen := make(map[string]bool)

	// Walk each maximal "run" that begins at an FSM- token. Within a run,
	// comma/space-separated bare numbers are continuations of the same list
	// ("FSM-360, 361, 395"). A run ends at the next FSM- token (which starts a
	// fresh run) or at a token that is neither a number nor a separator.
	locs := fsmKeyRe.FindAllStringIndex(text, -1)
	for i, loc := range locs {
		// The end of this run is the start of the next FSM- token (or EOF).
		runEnd := len(text)
		if i+1 < len(locs) {
			runEnd = locs[i+1][0]
		}
		run := text[loc[0]:runEnd]
		addRunKeys(run, &keys, seen)
	}
	return keys
}

// addRunKeys pulls every number token out of a single FSM- run and appends the
// canonical "FSM-<n>" key for each, skipping duplicates. A run is only scanned
// once it is known to begin at an FSM- token (ParseWaveKeys guarantees this), so
// the leading key is always attributed to FSM even though later tokens are bare.
func addRunKeys(run string, keys *[]string, seen map[string]bool) {
	// Stop the run at the first token that clearly ends the comma-separated list:
	// a non-numeric, non-separator word (e.g. "(oldest" or "—"). We scan token by
	// token so "FSM-360,361,395,398,428,436,437,458,461" is fully captured but
	// "FSM-360 spans CRF summary" stops after 360.
	//
	// Split on runs of separators (comma, whitespace) and keep going only while
	// each field is a number (optionally FSM-prefixed). The FIRST field always
	// carries the FSM- prefix by construction.
	fields := splitOnSeparators(run)
	for idx, f := range fields {
		m := fsmKeyNumRe.FindStringSubmatch(f)
		// The first field is the anchoring "FSM-<n>" and always matches. A later
		// field that is not a pure number/FSM-number token ends the list run.
		isPureNumberToken := m != nil && m[0] == f
		if idx > 0 && !isPureNumberToken {
			break
		}
		if m == nil {
			break
		}
		key := "FSM-" + m[1]
		if !seen[key] {
			seen[key] = true
			*keys = append(*keys, key)
		}
	}
}

// splitOnSeparators splits a run on commas and whitespace, dropping empty
// fields. It preserves token boundaries so addRunKeys can decide, per token,
// whether the comma-separated list continues.
func splitOnSeparators(run string) []string {
	return strings.FieldsFunc(run, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
}

// waveLabelRe extracts the wave letter(s) from a wave step title
// ("Wave B — …", "Waves I-K — …") for the informational BacklogPickResult.Wave.
// The trailing \b requires the captured letter(s) to be a standalone token —
// "Wave B" and "Waves I-K" match, but "no wave here" does NOT (the "h" of "here"
// is followed by more letters, so it is not a bare wave letter).
var waveLabelRe = regexp.MustCompile(`(?i)\bwaves?\s+([A-Z](?:\s*[-–]\s*[A-Z])?)\b`)

// waveLabelNonLetterRe strips the separators ("-", spaces) out of a captured
// wave label so "I-K" normalizes to "IK".
var waveLabelNonLetterRe = regexp.MustCompile(`[^A-Za-z]`)

// ParseWaveLabel returns the wave letter(s) named in a wave step title, or "" if
// the title does not name a wave. Informational only — it never affects scoping.
func ParseWaveLabel(title string) string {
	m := waveLabelRe.FindStringSubmatch(title)
	if m == nil {
		return ""
	}
	return strings.ToUpper(waveLabelNonLetterRe.ReplaceAllString(m[1], ""))
}

// SelectFromIssues is the PURE core of the backlog picker: given the wave's named
// keys and the JiraIssues fetched for them, it partitions the keys into
// Untriaged / Skipped / Missing and returns the oldest un-triaged key as Pick.
//
// It performs NO I/O — the caller fetches issues (BacklogPick does, via the live
// JiraClient) and hands them here — so every selection rule is unit-testable
// against a fixed issue set.
//
// Rules (task ka-0000007742):
//   - A named key with an issue carrying gardenio-triaged → Skipped (dedup).
//   - A named key with an issue NOT carrying it → Untriaged.
//   - A named key with NO issue in the fetch → Missing (surfaced, not dropped).
//   - Pick is the oldest Untriaged key (Created ascending), ties broken by
//     numeric key so the pick is deterministic across re-runs.
func SelectFromIssues(namedKeys []string, issues []JiraIssue) BacklogPickResult {
	// Index the fetched issues by upper-cased key for O(1) lookup. Jira returns
	// keys upper-cased already, but normalize defensively so a caller that passed
	// lower-cased named keys still matches.
	byKey := make(map[string]JiraIssue, len(issues))
	for _, iss := range issues {
		byKey[strings.ToUpper(iss.Key)] = iss
	}

	var result BacklogPickResult
	// created holds the parsed sort key for each Untriaged entry so the final
	// sort does not re-parse timestamps. Absent/garbage created sorts last
	// (oldest-first means a ticket with no date is treated as newest, so it does
	// not jump ahead of a ticket with a real old date).
	type pending struct {
		key     string
		created string
		num     int
	}
	var untriaged []pending

	seen := make(map[string]bool, len(namedKeys))
	for _, raw := range namedKeys {
		key := strings.ToUpper(strings.TrimSpace(raw))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true

		iss, ok := byKey[key]
		if !ok {
			result.Missing = append(result.Missing, key)
			continue
		}
		if issueHasTriagedSentinel(iss) {
			result.Skipped = append(result.Skipped, key)
			continue
		}
		untriaged = append(untriaged, pending{
			key:     key,
			created: iss.Fields.Created,
			num:     fsmKeyNum(key),
		})
	}

	// Oldest-first, ties → numeric key. An empty/unparseable created sorts AFTER
	// any real timestamp (treated as "newest") so a dateless ticket never
	// pre-empts a genuinely old one for the wave's anchoring pick.
	sort.SliceStable(untriaged, func(i, j int) bool {
		ci, cj := untriaged[i].created, untriaged[j].created
		if ci != cj {
			// Lexical compare works for RFC3339-ish Jira timestamps
			// ("2026-02-09T…"); empty string sorts first lexically, so invert
			// so empties go last.
			if ci == "" {
				return false
			}
			if cj == "" {
				return true
			}
			return ci < cj
		}
		return untriaged[i].num < untriaged[j].num
	})

	result.Untriaged = make([]string, len(untriaged))
	for i, p := range untriaged {
		result.Untriaged[i] = p.key
	}
	if len(result.Untriaged) > 0 {
		result.Pick = result.Untriaged[0]
	}

	sortByFSMNum(result.Skipped)
	sortByFSMNum(result.Missing)
	return result
}

// issueHasTriagedSentinel reports whether a fetched Jira issue already carries
// the gardenio-triaged sentinel. It matches ONLY the exact sentinel (not the
// sev-/gxp- refinement labels) — the sentinel is what the outbox writes to mark
// a ticket done, and a ticket could in principle carry a stray sev-label without
// having been fully triaged, so the bare sentinel is the correct dedup signal.
func issueHasTriagedSentinel(iss JiraIssue) bool {
	for _, l := range iss.Fields.Labels {
		if strings.TrimSpace(l) == BacklogTriagedLabel {
			return true
		}
	}
	return false
}

// fsmKeyNum extracts the numeric part of an FSM key ("FSM-360" → 360) for
// deterministic tie-breaking. A key with no parseable number sorts last (returns
// a large sentinel) so a malformed key never silently sorts first.
func fsmKeyNum(key string) int {
	m := fsmKeyNumRe.FindStringSubmatch(key)
	if m == nil {
		return 1 << 30
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 1 << 30
	}
	return n
}

// sortByFSMNum sorts a slice of FSM keys ascending by their numeric part, in
// place, for stable audit output.
func sortByFSMNum(keys []string) {
	sort.SliceStable(keys, func(i, j int) bool {
		return fsmKeyNum(keys[i]) < fsmKeyNum(keys[j])
	})
}

// BacklogPickIssueSearcher is the minimal live-Jira surface BacklogPick needs: a
// key-scoped search returning labels + created. *JiraClient satisfies it. Kept as
// an interface so BacklogPick is testable with a fake and so the picker can never
// reach a write method.
type BacklogPickIssueSearcher interface {
	SearchIssues(ctx context.Context, jql string, fields []string, maxResults int) ([]JiraIssue, error)
}

// backlogPickSearchFields is the minimal field set the picker reads back from
// Jira: the labels (for the gardenio-triaged dedup) and created (for oldest-first
// ordering). Summary/status are pulled too so a caller can print a human line
// without a second fetch.
var backlogPickSearchFields = []string{"labels", "created", "summary", "status"}

// backlogPickJQLBatch is the max number of keys per `key IN (...)` search. Jira
// caps JQL length; the poller batches key-scoped searches at 50 (jira_poller.go),
// and the largest wave (misc, 22) fits in one batch — but batch anyway so a
// future all-88 call is walked completely rather than silently truncated.
const backlogPickJQLBatch = 50

// BacklogPick fetches the named wave keys from LIVE Jira and returns the pick.
// It is the thin live adapter over SelectFromIssues: it issues ONLY key-scoped
// read searches (never a created-window JQL, never a write) and hands the fetched
// issues to the pure selector. The gardenio-triaged dedup is therefore read from
// Jira at pick time — the outbox sentinel is the source of truth, not a bead scan
// (task ka-0000007742).
//
// namedKeys is the wave's scope (from ParseWaveKeys). An empty namedKeys returns
// an empty result rather than an error — a released wave that names no keys is a
// no-op, not a failure. wave is the informational wave label for the result.
func BacklogPick(ctx context.Context, jira BacklogPickIssueSearcher, wave string, namedKeys []string) (BacklogPickResult, error) {
	if jira == nil {
		return BacklogPickResult{}, fmt.Errorf("backlog-pick: a Jira searcher is required")
	}
	// De-dup and normalize the named set before searching so the JQL and the
	// selector see the same canonical keys.
	var keys []string
	seen := make(map[string]bool, len(namedKeys))
	for _, raw := range namedKeys {
		k := strings.ToUpper(strings.TrimSpace(raw))
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return BacklogPickResult{Wave: wave}, nil
	}

	var issues []JiraIssue
	for start := 0; start < len(keys); start += backlogPickJQLBatch {
		end := start + backlogPickJQLBatch
		if end > len(keys) {
			end = len(keys)
		}
		batch := keys[start:end]
		jql := "key IN (" + quoteJQL(batch) + ")"
		page, err := jira.SearchIssues(ctx, jql, backlogPickSearchFields, len(batch))
		if err != nil {
			return BacklogPickResult{Wave: wave}, fmt.Errorf("backlog-pick: searching %d keys: %w", len(batch), err)
		}
		issues = append(issues, page...)
	}

	result := SelectFromIssues(keys, issues)
	result.Wave = wave
	return result, nil
}
