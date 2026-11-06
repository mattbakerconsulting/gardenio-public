package beadsapi

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// MergeRequestBeadFields carries the GitLab MR state that the gitlab-bridge
// projects onto a type=merge-request bead. The bridge upserts these fields
// on every MR webhook + sync-poll event. See epic ka-0000001613.
type MergeRequestBeadFields struct {
	MRURL          string
	MRIID          string
	ProjectPath    string
	SourceBranch   string
	TargetBranch   string
	MRState        string
	MROutcome      string // "merged" or "closed" when the MR has reached a terminal state.
	PipelineStatus string
	PipelineURL    string
	Author         string
	Title          string
	// Forge names the forge the mr_* fields describe ("github"). Empty means
	// GitLab and is not written, so existing GitLab beads stay untouched.
	Forge string
	// GitLab numeric project id, when known. Mirrors the legacy task-bead
	// gitlab_project_id field so consumers can hop straight to the GitLab API.
	GitLabProjectID string
	// Approval state (string-typed for parity with the shared mrFields shape).
	MRApproved          string
	MRApprovers         string
	MRHasReviewComments string
	// OperatedBy is the human-operator id the MR bead should be attributed
	// to — by default the MR's GitLab creator, resolved through the user
	// directory to slack:<id> / oidc:<email>, falling back to
	// gitlab:<username>. Empty when the bridge could not resolve an author.
	// mrFieldsMap skips empties, so a re-poll that cannot resolve the author
	// never clobbers an operated_by already stamped at create time.
	OperatedBy string
	// ExternalMirror marks this bead as an upstream-MR mirror — a read-only
	// projection of a human-owned MR on the REAL monorepo (E-PROV FW-5,
	// ka-0000003146). When true, mrFieldsMap stamps the upstream / external_mirror
	// fields so beads-viewer renders the node as external. The accompanying
	// LABELS (upstream:true / gardener-delivery-skip / external-mirror:true) are
	// applied at create time via Labels — see UpsertUpstreamMirrorBead.
	ExternalMirror bool
	// Labels is the label set applied to the bead ON CREATION only (passed
	// through to CreateBeadRequest). Existing beads are not re-labelled by the
	// upsert; callers that need idempotent labelling of a pre-existing bead
	// (e.g. the upstream webhook created the bead before `gb mr link` ran) use
	// UpsertUpstreamMirrorBead, which AddLabels each entry after the upsert.
	Labels []string
}

// UpstreamMirrorLabels are the canonical labels every upstream-MR mirror bead
// carries (E-PROV FW-5, ka-0000003146):
//
//   - upstream:true          scopes the FW-2 backstop poller
//     (ListOpenMergeRequestBeads label filter) so it reconciles only the small
//     set of human-owned upstream peel MRs, never the broad dark-MR set.
//   - gardener-delivery-skip  removes the bead from every gardener-delivery
//     gardener's bead_event wake and its open-MR cron count (exclude_labels), so
//     no gardener ever drives a human-owned upstream MR — honoring the
//     read-only-mirror invariant (ka-0000003234): the bead only DISPLAYS state
//     pulled FROM upstream, nothing is ever pushed outbound.
//   - external-mirror:true   is the semantic marker that the bead is an
//     external (non-dark) mirror. Paired with the external_mirror field so the
//     viewer can render the node as external without a labels join.
var UpstreamMirrorLabels = []string{"upstream:true", "gardener-delivery-skip", "external-mirror:true"}

// UpsertUpstreamMirrorBead upserts the read-only upstream-MR mirror bead for a
// human-owned peel MR on the REAL monorepo and guarantees it carries the
// canonical UpstreamMirrorLabels (E-PROV FW-5, ka-0000003146).
//
// It is the single creation path for upstream-mirror beads, shared by
// `gb mr link --upstream` and any future bridge caller, so the read-only-mirror
// invariant (ka-0000003234) is enforced in ONE place:
//
//   - The bead is keyed by canonical mr_url (host preserved by the caller), so
//     a dark !N and an upstream !N never collide on one beads daemon.
//   - It is created OPEN (closed=false); terminal state flows in later from the
//     upstream merge_request webhook / FW-2 backstop poller.
//   - ExternalMirror is forced true so the upstream / external_mirror fields are
//     stamped for viewer rendering.
//   - The labels are applied at create time (CreateBeadRequest.Labels) AND
//     idempotently re-applied via AddLabel afterwards, so a bead the upstream
//     webhook already created (un-labelled) is brought into compliance when
//     `gb mr link --upstream` runs against it.
//
// NEVER fires the upstream Jira write path; never pushes outbound.
func (c *Client) UpsertUpstreamMirrorBead(ctx context.Context, f MergeRequestBeadFields) (string, error) {
	f.ExternalMirror = true
	f.Labels = UpstreamMirrorLabels

	id, err := c.UpsertMergeRequestBead(ctx, f, false)
	if err != nil {
		return "", err
	}

	// Idempotently ensure every canonical label is present. On the create path
	// CreateBeadRequest.Labels already set them; this loop is the no-op-or-fix
	// for the pre-existing-bead path (webhook created it before this ran).
	for _, l := range UpstreamMirrorLabels {
		if lErr := c.AddLabel(ctx, id, l); lErr != nil {
			return id, fmt.Errorf("labelling upstream-mirror bead %s with %q: %w", id, l, lErr)
		}
	}
	return id, nil
}

// UpsertMergeRequestBead upserts a type=merge-request bead keyed by canonical
// MR URL. Returns the bead ID.
//
// Behaviour:
//   - If no bead with fields.mr_url == f.MRURL exists, a new one is created.
//     Status is "open" by default; when closed=true the bead is created with
//     status="closed" and mr_outcome set from f.MROutcome (or "merged" /
//     "closed" inferred from f.MRState when MROutcome is empty).
//   - If an existing bead is found, its fields are updated in place. When
//     closed=true and the bead is still open, the bead is closed and
//     mr_outcome is stamped.
//
// The lookup is client-side (list type=merge-request beads, filter by
// fields.mr_url) — the set is small in practice and the daemon does not
// offer a field-eq filter for arbitrary keys. See task ka-0000007548.
func (c *Client) UpsertMergeRequestBead(ctx context.Context, f MergeRequestBeadFields, closed bool) (string, error) {
	if f.MRURL == "" {
		return "", fmt.Errorf("UpsertMergeRequestBead: mr_url is required")
	}

	// Serialise the find-then-create-or-update critical section per mr_url so
	// concurrent upserts for the same MR converge to ONE bead. Without this,
	// two near-simultaneous events (one per bead linked to the MR by a single
	// `gb mr link` to multiple beads, ~2ms apart) both miss findMergeRequestBead
	// and each CreateBead a duplicate — the observed double-MR-bead in
	// ka-0000007606 (MR !3656 → ka-0000000654 + ka-0000008228). This keys on
	// the canonical mr_url, NOT the producing task bead, so the legitimate
	// multi-producer pattern (one MR producing several task beads — see
	// handleMergeRequestUpsertWebhook) is preserved: those concurrent upserts
	// serialise on the same lock, find the now-existing MR bead, and update it
	// in place rather than racing two inserts.
	//
	// This lock is IN-PROCESS ONLY and is now a best-effort fast-path, not the
	// correctness boundary: it cannot serialise two bridge replicas, or one
	// replica across a restart, both of which each miss the find and each insert
	// a duplicate. As of ka-0000002762 the DAEMON dedups createBead by
	// fields.mr_url for type=merge-request (server/beads_mr_dedup.go), which is
	// authoritative because every replica's CreateBead funnels through the single
	// beads daemon. Keeping this lock avoids a redundant daemon round-trip on the
	// common single-replica multi-producer burst; the daemon closes the
	// cross-replica window the lock structurally cannot.
	mu := c.mrUpsertLock(f.MRURL)
	mu.Lock()
	defer mu.Unlock()

	existing, err := c.findMergeRequestBead(ctx, f.MRURL)
	if err != nil {
		return "", fmt.Errorf("looking up MR bead for %s: %w", f.MRURL, err)
	}

	fields := mrFieldsMap(f)

	if existing == nil {
		title := f.Title
		if title == "" {
			title = f.MRURL
		}
		status := "open"
		if closed {
			status = "closed"
			if _, ok := fields["mr_outcome"]; !ok {
				fields["mr_outcome"] = inferOutcome(f.MRState)
			}
		}
		fieldsJSON, mErr := json.Marshal(fields)
		if mErr != nil {
			return "", fmt.Errorf("marshalling MR bead fields: %w", mErr)
		}
		req := CreateBeadRequest{
			Title:  title,
			Type:   "merge-request",
			Kind:   "issue",
			Fields: json.RawMessage(fieldsJSON),
			Status: status,
			// Labels are set on creation only (e.g. the upstream-mirror marker
			// set from UpsertUpstreamMirrorBead, FW-5). Existing beads are not
			// re-labelled here — that path runs AddLabel explicitly.
			Labels: f.Labels,
		}
		id, cErr := c.CreateBead(ctx, req)
		if cErr != nil {
			return "", fmt.Errorf("creating MR bead for %s: %w", f.MRURL, cErr)
		}
		return id, nil
	}

	// operated_by is set-once: the MR's creator should own the bead, so a later
	// event whose actor is a merger/updater (not the creator) must not clobber
	// the value stamped at create time (ka-0000004473). Drop it from the update
	// map when the existing bead already carries a non-empty operated_by.
	if existing.Fields["operated_by"] != "" {
		delete(fields, "operated_by")
	}

	// Update fields on the existing bead. UpdateBeadFields merges + skips no-ops.
	if uErr := c.UpdateBeadFields(ctx, existing.ID, fields); uErr != nil {
		return "", fmt.Errorf("updating MR bead %s: %w", existing.ID, uErr)
	}

	// Close transition: only flip if the bead is still active.
	if closed && existing.Status != "closed" {
		closeFields := map[string]string{}
		if _, ok := fields["mr_outcome"]; !ok {
			closeFields["mr_outcome"] = inferOutcome(f.MRState)
		}
		if cErr := c.CloseBead(ctx, existing.ID, closeFields); cErr != nil {
			return "", fmt.Errorf("closing MR bead %s: %w", existing.ID, cErr)
		}
	}

	return existing.ID, nil
}

// MRBeadForTask walks the `produces` dep edges out of taskBeadID and returns
// the first type=merge-request bead reached, or (nil, nil) when no MR bead is
// linked. Errors only on transport failures — dangling dep targets are
// silently skipped. Consumers should fall back to legacy task-bead mr_*
// fields when the result is nil.
//
// See epic ka-0000001613 — promotes MRs to first-class beads; consumers walk
// `task --produces--> merge-request` instead of reading mr_* fields off the
// task bead.
func (c *Client) MRBeadForTask(ctx context.Context, taskBeadID string) (*BeadDetail, error) {
	if taskBeadID == "" {
		return nil, nil
	}
	deps, err := c.GetDependencies(ctx, taskBeadID)
	if err != nil {
		return nil, fmt.Errorf("listing deps for task %s: %w", taskBeadID, err)
	}
	for _, d := range deps {
		if d.Type != "produces" || d.DependsOnID == "" {
			continue
		}
		mr, err := c.GetBead(ctx, d.DependsOnID)
		if err != nil || mr == nil {
			continue
		}
		if mr.Type == "merge-request" {
			return mr, nil
		}
	}
	return nil, nil
}

// MRBeadsForAgent walks the 2-hop `agent --produces--> task --produces-->
// merge-request` graph and returns the unique set of merge-request beads
// reached from agentBeadID. Mirrors the predicate pattern of MRBeadForTask
// (1-hop, single result) at the agent-fleet scale: an agent typically
// spawns 0-N task beads that each produce 0-1 MR beads.
//
// Returns an empty slice when nothing is linked. Dangling dep targets and
// per-bead fetch failures are silently skipped — losing one MR row should
// not blank the agent-detail produced-MRs section. Errors only on the
// first-hop transport failure (we cannot enumerate the task set).
//
// Dedup: the same MR bead reached via multiple producing tasks is returned
// once. Order is the traversal order of the agent's `produces` deps.
func (c *Client) MRBeadsForAgent(ctx context.Context, agentBeadID string) ([]*BeadDetail, error) {
	if agentBeadID == "" {
		return nil, nil
	}
	taskDeps, err := c.GetDependencies(ctx, agentBeadID)
	if err != nil {
		return nil, fmt.Errorf("listing deps for agent %s: %w", agentBeadID, err)
	}
	var out []*BeadDetail
	seen := map[string]bool{}
	for _, td := range taskDeps {
		if td.Type != "produces" || td.DependsOnID == "" {
			continue
		}
		task, err := c.GetBead(ctx, td.DependsOnID)
		if err != nil || task == nil {
			continue
		}
		// Second hop: walk produces edges out of each downstream bead. We
		// don't require task.Type == "task" — the produces edge is the
		// authoritative signal; a bead reached via two hops of produces is
		// fair game.
		mrDeps, err := c.GetDependencies(ctx, task.ID)
		if err != nil {
			continue
		}
		for _, md := range mrDeps {
			if md.Type != "produces" || md.DependsOnID == "" {
				continue
			}
			if seen[md.DependsOnID] {
				continue
			}
			mr, err := c.GetBead(ctx, md.DependsOnID)
			if err != nil || mr == nil {
				continue
			}
			if mr.Type != "merge-request" {
				continue
			}
			seen[md.DependsOnID] = true
			out = append(out, mr)
		}
	}
	return out, nil
}

// MRBeadByURL returns the type=merge-request bead whose fields.mr_url matches
// the given canonical MR URL, or (nil, nil) when none exists. Errors only on
// transport / decode failures.
func (c *Client) MRBeadByURL(ctx context.Context, mrURL string) (*BeadDetail, error) {
	if mrURL == "" {
		return nil, nil
	}
	return c.findMergeRequestBead(ctx, mrURL)
}

// ListOpenMergeRequestBeads returns open type=merge-request beads, optionally
// scoped to those carrying the given label. It is the read side of the scoped
// upstream backstop poller (E-PROV FW-2, ka-0000007488): the poller passes the
// upstream-mirror marker label (e.g. "upstream:true" from FW-5) so it only
// reconciles the small set of human-owned upstream MR mirror beads via a
// per-bead GitLab lookup — NOT a broad group-merged-MR scan like GitLabPoller.
// An empty label returns every open MR bead.
//
// Only open beads are returned: a mirror bead already closed (mr_outcome
// stamped) needs no reconciliation, so the caller never re-fetches it.
func (c *Client) ListOpenMergeRequestBeads(ctx context.Context, label string) ([]*BeadDetail, error) {
	// Page the merge-request-bead set to exhaustion via ListAllBeadsFiltered
	// (added in ka-0000001936 / MR !6521). The old Limit:500 fetch silently
	// truncated once the population outgrew the cap: there are 6834
	// merge-request beads fleet-wide (measured 2026-08-03), so an unlabelled
	// call (empty label = "every open MR bead") returned only the newest 500
	// and dropped the rest with no warning (bug ka-0000005190, class
	// ka-0000003555). The labelled upstream-backstop call (upstream:true) is
	// still small, but paging keeps it growth-proof too. ListAllBeadsFiltered
	// owns paging, so Limit/Offset stay unset here.
	q := ListBeadsQuery{
		Types:    []string{"merge-request"},
		Statuses: []string{"open", "in_progress", "blocked", "deferred"},
	}
	if label != "" {
		q.Labels = []string{label}
	}
	result, err := c.ListAllBeadsFiltered(ctx, q)
	if err != nil {
		return nil, err
	}
	return result.Beads, nil
}

// ProducingTaskBeadsForMR returns the task beads that produced mrBeadID via a
// `produces` dep edge. Returns an empty slice when nothing is linked. Errors
// only on transport failures.
func (c *Client) ProducingTaskBeadsForMR(ctx context.Context, mrBeadID string) ([]*BeadDetail, error) {
	if mrBeadID == "" {
		return nil, nil
	}
	deps, err := c.GetReverseDependencies(ctx, mrBeadID)
	if err != nil {
		return nil, fmt.Errorf("listing reverse deps for MR %s: %w", mrBeadID, err)
	}
	var out []*BeadDetail
	for _, d := range deps {
		if d.Type != "produces" || d.BeadID == "" {
			continue
		}
		b, err := c.GetBead(ctx, d.BeadID)
		if err != nil || b == nil {
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

// mrUpsertLock returns the per-mr_url mutex that serialises
// UpsertMergeRequestBead's find-then-create-or-update critical section. The
// lock set is keyed on canonical mr_url; LoadOrStore makes first-creation
// race-safe so two goroutines for the same URL share one mutex. See bug
// ka-0000007606.
func (c *Client) mrUpsertLock(mrURL string) *sync.Mutex {
	v, _ := c.mrUpsertLocks.LoadOrStore(mrURL, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// findMergeRequestBead returns the MR bead whose fields.mr_url == mrURL, or
// nil if none exists. Errors only on transport / decode failures — "not
// found" is a nil bead with nil error.
//
// The lookup pushes the mr_url selector SERVER-SIDE via the daemon's
// field-equality filter (`fields ? 'mr_url' AND fields->>'mr_url' = $1`,
// kbeads queries.go), so the daemon returns the single matching bead instead
// of a capped page the client re-scans. Before this it fetched Limit:500
// type=merge-request beads (created_at DESC) and linear-scanned for the URL —
// but there are 6834 merge-request beads fleet-wide (measured 2026-08-03), so
// the daemon capped the fetch at 500 and any MR bead older than that window
// was invisible: MRBeadByURL returned (nil, nil) = "no such MR bead" for an MR
// whose bead genuinely exists, and UpsertMergeRequestBead's find-then-create
// missed the existing bead and inserted a DUPLICATE (bug ka-0000005190; class
// ka-0000003555). All statuses stay in the filter: a merged MR's canonical
// bead is status=closed (config.go type:merge-request), so closed MUST be
// reachable. Limit:2 detects (rather than silently swallows) an unexpected
// duplicate for one canonical mr_url — mirrors the daemon-side mrBeadsByURL
// point lookup in auto_close.go.
func (c *Client) findMergeRequestBead(ctx context.Context, mrURL string) (*BeadDetail, error) {
	result, err := c.ListBeadsFiltered(ctx, ListBeadsQuery{
		Types:    []string{"merge-request"},
		Statuses: []string{"open", "in_progress", "blocked", "deferred", "closed"},
		Fields:   map[string]string{"mr_url": mrURL},
		Limit:    2,
	})
	if err != nil {
		return nil, err
	}
	for _, b := range result.Beads {
		if b.Fields["mr_url"] == mrURL {
			return b, nil
		}
	}
	return nil, nil
}

// mrFieldsMap projects MergeRequestBeadFields onto a string map. Empty
// values are skipped so UpdateBeadFields's no-op short-circuit can fire
// when the bridge re-polls an MR with unchanged state.
func mrFieldsMap(f MergeRequestBeadFields) map[string]string {
	out := map[string]string{}
	setIfNotEmpty(out, "forge", f.Forge)
	setIfNotEmpty(out, "mr_url", f.MRURL)
	setIfNotEmpty(out, "mr_iid", f.MRIID)
	setIfNotEmpty(out, "project_path", f.ProjectPath)
	setIfNotEmpty(out, "source_branch", f.SourceBranch)
	setIfNotEmpty(out, "target_branch", f.TargetBranch)
	setIfNotEmpty(out, "mr_state", f.MRState)
	setIfNotEmpty(out, "mr_outcome", f.MROutcome)
	setIfNotEmpty(out, "mr_pipeline_status", f.PipelineStatus)
	setIfNotEmpty(out, "mr_pipeline_url", f.PipelineURL)
	setIfNotEmpty(out, "author", f.Author)
	setIfNotEmpty(out, "operated_by", f.OperatedBy)
	setIfNotEmpty(out, "mr_title", f.Title)
	setIfNotEmpty(out, "gitlab_mr_iid", f.MRIID)
	setIfNotEmpty(out, "gitlab_project_id", f.GitLabProjectID)
	setIfNotEmpty(out, "gitlab_mr_source_branch", f.SourceBranch)
	setIfNotEmpty(out, "mr_approved", f.MRApproved)
	setIfNotEmpty(out, "mr_approvers", f.MRApprovers)
	setIfNotEmpty(out, "mr_has_review_comments", f.MRHasReviewComments)
	if f.ExternalMirror {
		// Mark the bead as a read-only upstream-MR mirror (E-PROV FW-5,
		// ka-0000003146). beads-viewer reads external_mirror to render the node
		// as external; `upstream` is a redundant alias kept for query symmetry
		// with the upstream:true label. Set-true only — a later non-mirror
		// upsert for the same URL never appears (URL is host-keyed), and even
		// if it did, mrFieldsMap only ever sets these true, never clears them.
		out["external_mirror"] = "true"
		out["upstream"] = "true"
	}
	if f.MRState == "merged" || f.MRState == "closed" {
		// Maintain mr_merged parity with the existing task-bead dual-write so
		// downstream consumers (auto-close logic, etc.) see the same signal.
		if f.MRState == "merged" {
			out["mr_merged"] = "true"
		} else {
			out["mr_merged"] = "false"
		}
	}
	return out
}

func setIfNotEmpty(m map[string]string, k, v string) {
	if v != "" {
		m[k] = v
	}
}

// inferOutcome maps a GitLab state into the mr_outcome enum. "merged" stays
// "merged"; everything else collapses to "closed" — the bridge only calls
// the upsert in the closed=true path when state is one of those two.
func inferOutcome(state string) string {
	if state == "merged" {
		return "merged"
	}
	return "closed"
}
