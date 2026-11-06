// Command mr-reopen-probe drives the REAL gitlab-bridge MR-reopen drift sweep
// (internal/bridge/gitlab_mr_reopen_drift.go, ka-0000000267 — direction 2 of
// ka-0000003953) against the REAL beads daemon and a STUB GitLab, and asserts
// the reopen leg end-to-end. It is the e2e counterpart of the unit tests in
// gitlab_mr_reopen_drift_test.go and is driven by
// tests/e2e/scripts/test-mr-reopen-drift.sh (bead ka-0000006088).
//
// ── Why a probe, not a live-bridge observation ───────────────────────────────
//
// gitlabBridge.enabled is FALSE in the e2e namespace — the same reason test-gitlab-bridge.sh's
// mention leg and test-gitlab-dark-provenance.sh skip a live bridge. So there is
// no sweep pod to observe. And even with one, the sweep is driven by
// gitlab.com's real open-MR list on a 15m interval: a test cannot make a real MR
// stay `opened` on demand, nor wait 15m inside a shard budget.
//
// So this follows the cmd/gitlab-prov-probe / cmd/jira-conv-probe precedent:
// build the REAL MRReopenDriftReconciler over the REAL beadsapi.Client pointed
// at the shard's live daemon, and the REAL bridge.GitLabClient pointed at an
// httptest stub standing in for gitlab.com. Only the GitLab HTTP responses are
// synthetic — the sweep logic, the daemon queries, and every bead write are the
// production code paths.
//
// ── What this covers that the unit tests CANNOT ──────────────────────────────
//
// The unit tests use a hand-rolled fakeReopenDaemon whose ListBeadsFiltered
// reimplements the status+field filter in Go. That fake is the ASSUMPTION under
// test here: the sweep asks the daemon for `status=closed` +
// `field=mr_url=<url>` and trusts it to return exactly the owning issue bead(s).
// If the daemon's field filter were loose (prefix/substring), absent, or scoped
// so it dropped cross-project beads, every unit test would still pass while the
// production sweep re-opened the WRONG bead or none at all. This probe pins that
// seam against the real daemon — including the real `type=merge-request` bead
// that carries the SAME mr_url (on live data both come back from one query, so
// the sweep's type guard is load-bearing, not theoretical).
//
// ── Legs (each a hard sub-gate) ──────────────────────────────────────────────
//
//	1  reopen        A closed issue bead whose MR is still `opened` IS re-opened,
//	                 with the audit comment attached. The headline repair.
//	2  merged-stays  The negative: a closed bead whose MR is `merged` stays
//	                 CLOSED (it never enters the open-MR scan at all).
//	3  override      A bead closed deliberately via `kd close --allow-open-mr`
//	                 (mr_close_override=true) stays closed even though its MR is
//	                 open — directions 1 and 2 compose instead of fighting.
//	4  mr-bead-guard The canonical type=merge-request bead carrying the same
//	                 mr_url is NEVER re-opened; only the issue bead it tracks.
//	                 Asserted against a REAL daemon query that returns both.
//	5  reverify      An MR that is `opened` in the group listing but `merged` on
//	                 the per-MR re-verify fetch does NOT get re-opened — the
//	                 stale-snapshot race guard, driven through real HTTP.
//
// Each leg prints one "LEG <name>: PASS|FAIL <detail>" line; the process exits
// non-zero if any leg failed. Like the other probes this is a test-only driver:
// it is in no release Makefile/Dockerfile target and never ships in an image.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
)

// probeGroupID is the stub GitLab group the sweep scans. Any non-zero value
// works (zero would disable the sweep); the stub keys off the path, not the id.
const probeGroupID = 4242

// projectPath is the stub GitLab project every probe MR lives under. It must
// round-trip through bridge.ParseMRURL, which requires a /-/merge_requests/<iid>
// URL shape.
const projectPath = "e2e-probe/mr-reopen"

// leg records one sub-gate's outcome.
type leg struct {
	name   string
	ok     bool
	detail string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mr-reopen-probe:", err)
		os.Exit(1)
	}
}

func run() error {
	addr := envOr("BEADS_HTTP_URL", "http://localhost:8080")
	daemon, err := beadsapi.New(beadsapi.Config{
		HTTPAddr: addr,
		// Same token env chain kd honors, so the probe authenticates wherever kd
		// does — the e2e shard exports BEADS_TOKEN, while an agent pod carries
		// BEADS_AUTH_TOKEN / BEADS_DAEMON_TOKEN.
		Token: firstEnv("BEADS_TOKEN", "GARDENIO_TOKEN", "KBEADS_TOKEN", "BEADS_AUTH_TOKEN", "BEADS_DAEMON_TOKEN"),
	})
	if err != nil {
		return fmt.Errorf("beads client: %w", err)
	}
	// The daemon rejects writes it cannot attribute to an operator ("HTTP 400:
	// operator could not be resolved"). The shard exports BEADS_OPERATED_BY (the
	// same value kd sends); fall back to the e2e actor so the probe is runnable
	// standalone.
	if op := envOr("BEADS_OPERATED_BY", ""); op != "" {
		daemon.SetOperatedBy(op)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	// Fail fast with a clear message if the daemon is unreachable — otherwise
	// every leg fails with the same opaque transport error.
	if _, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{Limit: 1}); err != nil {
		return fmt.Errorf("daemon unreachable at %s: %w", addr, err)
	}

	// Unique per-run so concurrent shards (and reruns) never collide on an
	// mr_url. The sweep keys beads by mr_url, so a collision would cross-wire
	// two runs' fixtures.
	runID := fmt.Sprintf("%d", time.Now().UnixNano())

	h := &harness{daemon: daemon, ctx: ctx, runID: runID}
	defer h.cleanup()

	legs := []leg{
		h.legReopen(),
		h.legMergedStaysClosed(),
		h.legOverrideRespected(),
		h.legNeverReopensMRBead(),
		h.legReverifyRace(),
	}

	failed := 0
	for _, l := range legs {
		status := "PASS"
		if !l.ok {
			status = "FAIL"
			failed++
		}
		fmt.Printf("LEG %s: %s %s\n", l.name, status, l.detail)
	}
	if failed > 0 {
		return fmt.Errorf("%d/%d legs failed", failed, len(legs))
	}
	return nil
}

// harness owns the live daemon client, the fixtures created against it, and the
// per-leg stub GitLab servers.
type harness struct {
	daemon  *beadsapi.Client
	ctx     context.Context
	runID   string
	created []string // bead ids to clean up
}

// mrURL builds a unique, ParseMRURL-parseable MR url for this run. The host is
// irrelevant to the sweep's bead lookup (it matches on the exact stored string),
// and the re-verify fetch goes to the stub's base URL, not this host.
func (h *harness) mrURL(iid int) string {
	return fmt.Sprintf("https://gitlab.com/%s/-/merge_requests/%s%d", projectPath, h.runID[len(h.runID)-6:], iid)
}

// iidOf extracts the IID the stub must answer for, mirroring ParseMRURL so the
// stub and the sweep agree on the key.
func iidOf(mrURL string) int {
	ref := bridge.ParseMRURL(mrURL)
	if ref == nil {
		return 0
	}
	return ref.IID
}

// stubGitLab serves the two endpoints the sweep calls:
//
//	GET /api/v4/groups/<id>/merge_requests?state=opened...  → the open-MR list
//	GET /api/v4/projects/<path>/merge_requests/<iid>        → the re-verify fetch
//
// openList is what the group listing returns; verifyState overrides the state a
// per-MR re-verify reports (keyed by IID), which is how the reverify-race leg
// simulates an MR that merged between the snapshot and the write.
func stubGitLab(openList []bridge.GitLabMR, verifyState map[int]string) *httptest.Server {
	byIID := map[int]bridge.GitLabMR{}
	for _, mr := range openList {
		byIID[mr.IID] = mr
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path

		// Group listing — honor the state filter the sweep sends so a wrong
		// state (anything but "opened") surfaces as an empty scan rather than
		// silently passing.
		if strings.Contains(path, "/merge_requests") && strings.Contains(path, "/groups/") {
			if r.URL.Query().Get("state") != "opened" {
				_ = json.NewEncoder(w).Encode([]bridge.GitLabMR{})
				return
			}
			_ = json.NewEncoder(w).Encode(openList)
			return
		}

		// Per-MR re-verify fetch: .../merge_requests/<iid>
		if strings.Contains(path, "/projects/") {
			idx := strings.LastIndex(path, "/merge_requests/")
			if idx >= 0 {
				var iid int
				_, _ = fmt.Sscanf(path[idx+len("/merge_requests/"):], "%d", &iid)
				mr, ok := byIID[iid]
				if !ok {
					w.WriteHeader(http.StatusNotFound)
					_, _ = io.WriteString(w, `{"message":"404 Not found"}`)
					return
				}
				if st, override := verifyState[iid]; override {
					mr.State = st
				}
				_ = json.NewEncoder(w).Encode(mr)
				return
			}
		}

		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"404 Not found"}`)
	}))
}

// newSweep builds the REAL reconciler over the REAL daemon client and a REAL
// GitLabClient pointed at the stub. Only the GitLab wire responses are
// synthetic.
func (h *harness) newSweep(stubURL string) *bridge.MRReopenDriftReconciler {
	gl := bridge.NewGitLabClient(bridge.GitLabClientConfig{
		BaseURL: stubURL,
		Token:   "probe-token",
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return bridge.NewMRReopenDriftReconciler(bridge.MRReopenDriftConfig{
		GitLab:  gl,
		Daemon:  h.daemon,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		GroupID: probeGroupID,
	})
}

// seedBead creates a bead on the LIVE daemon with the given type/status/fields.
// Beads are labelled project:e2e-test and tracked for cleanup.
func (h *harness) seedBead(title, beadType, status string, fields map[string]string) (string, error) {
	raw, err := json.Marshal(fields)
	if err != nil {
		return "", err
	}
	id, err := h.daemon.CreateBead(h.ctx, beadsapi.CreateBeadRequest{
		Title:     fmt.Sprintf("%s [mr-reopen-probe %s]", title, h.runID),
		Type:      beadType,
		Priority:  4,
		Labels:    []string{"project:e2e-test"},
		Fields:    raw,
		CreatedBy: "mr-reopen-probe",
	})
	if err != nil {
		return "", err
	}
	h.created = append(h.created, id)
	// Create then close: the daemon owns close semantics, and a bead that is
	// closed AFTER its mr_url is set is exactly the drift shape the sweep
	// repairs.
	if status == "closed" {
		if err := h.daemon.UpdateBead(h.ctx, id, beadsapi.UpdateBeadRequest{Status: strPtr("closed")}); err != nil {
			return "", fmt.Errorf("closing seed bead %s: %w", id, err)
		}
	}
	return id, nil
}

func (h *harness) statusOf(id string) (string, error) {
	b, err := h.daemon.GetBead(h.ctx, id)
	if err != nil {
		return "", err
	}
	return b.Status, nil
}

// ── Leg 1: the headline repair ───────────────────────────────────────────────
//
// A closed issue bead whose linked MR is still `opened` is re-opened, and the
// audit comment explaining why is attached. This is the exact MR !6183 drift the
// origin bug describes, driven through the real daemon.
func (h *harness) legReopen() leg {
	const name = "reopen"
	url := h.mrURL(1)
	id, err := h.seedBead("drift: closed bead, MR still open", "task", "closed", map[string]string{
		"mr_url": url, "mr_state": "opened",
	})
	if err != nil {
		return leg{name, false, "seed failed: " + err.Error()}
	}

	srv := stubGitLab([]bridge.GitLabMR{{
		IID: iidOf(url), WebURL: url, State: "opened", SourceBranch: "fix/probe",
	}}, nil)
	defer srv.Close()

	h.newSweep(srv.URL).ScanOnce(h.ctx)

	got, err := h.statusOf(id)
	if err != nil {
		return leg{name, false, "readback failed: " + err.Error()}
	}
	if got == "closed" {
		return leg{name, false, fmt.Sprintf("bead %s still closed — the sweep did NOT repair the drift", id)}
	}
	if got != "open" {
		return leg{name, false, fmt.Sprintf("bead %s has status %q; want open", id, got)}
	}

	// The audit comment is load-bearing: the sweep writes it BEFORE the status
	// flip precisely so a re-opened bead always explains itself.
	comments, err := h.daemon.GetComments(h.ctx, id)
	if err != nil {
		return leg{name, false, fmt.Sprintf("re-opened %s but comments unreadable: %v", id, err)}
	}
	found := false
	for _, c := range comments {
		if strings.Contains(c.Text, "gitlab-bridge reconcile sweep") && strings.Contains(c.Text, url) {
			found = true
			break
		}
	}
	if !found {
		return leg{name, false, fmt.Sprintf("bead %s re-opened but no audit comment naming the MR", id)}
	}
	return leg{name, true, fmt.Sprintf("closed bead %s with an open MR was re-opened + audit-commented", id)}
}

// ── Leg 2: the negative — a merged MR's bead stays closed ────────────────────
//
// The complement of leg 1 and the direction that keeps the sweep safe: a bead
// closed because its MR MERGED must stay closed. Structurally it never enters
// the scan (the sweep lists only `opened` MRs), which is exactly what we assert
// — a regression that widened the listing to state=all would re-open every
// correctly-closed bead in the fleet.
func (h *harness) legMergedStaysClosed() leg {
	const name = "merged-stays-closed"
	url := h.mrURL(2)
	id, err := h.seedBead("correct: closed bead, MR merged", "task", "closed", map[string]string{
		"mr_url": url, "mr_state": "merged", "mr_merged": "true",
	})
	if err != nil {
		return leg{name, false, "seed failed: " + err.Error()}
	}

	// The MR is merged, so GitLab's `state=opened` listing does not contain it.
	// The stub still knows the MR (for a re-verify) but reports it merged.
	srv := stubGitLab(nil, map[int]string{iidOf(url): "merged"})
	defer srv.Close()

	h.newSweep(srv.URL).ScanOnce(h.ctx)

	got, err := h.statusOf(id)
	if err != nil {
		return leg{name, false, "readback failed: " + err.Error()}
	}
	if got != "closed" {
		return leg{name, false, fmt.Sprintf("bead %s went to %q — a merged MR's bead must stay closed", id, got)}
	}
	return leg{name, true, fmt.Sprintf("bead %s (MR merged) correctly left closed", id)}
}

// ── Leg 3: a deliberate --allow-open-mr close is honored ─────────────────────
//
// `kd close --allow-open-mr` stamps mr_close_override=true (kbeads close.go).
// The sweep must leave such a bead closed even though its MR is open, so the
// close-time guard (direction 1) and this repair sweep (direction 2) compose
// instead of fighting — otherwise abandoning an MR would be impossible: the
// operator closes the bead and the sweep re-opens it every 15 minutes.
func (h *harness) legOverrideRespected() leg {
	const name = "override-respected"
	url := h.mrURL(3)
	id, err := h.seedBead("deliberate: closed over an open MR", "task", "closed", map[string]string{
		"mr_url": url, "mr_state": "opened", "mr_close_override": "true",
	})
	if err != nil {
		return leg{name, false, "seed failed: " + err.Error()}
	}

	srv := stubGitLab([]bridge.GitLabMR{{
		IID: iidOf(url), WebURL: url, State: "opened",
	}}, nil)
	defer srv.Close()

	h.newSweep(srv.URL).ScanOnce(h.ctx)

	got, err := h.statusOf(id)
	if err != nil {
		return leg{name, false, "readback failed: " + err.Error()}
	}
	if got != "closed" {
		return leg{name, false, fmt.Sprintf(
			"bead %s went to %q despite mr_close_override=true — a deliberate --allow-open-mr close must be honored", id, got)}
	}
	return leg{name, true, fmt.Sprintf("bead %s (mr_close_override=true) correctly left closed", id)}
}

// ── Leg 4: never re-open the canonical type=merge-request bead ───────────────
//
// The MR bead carries the SAME mr_url as the issue bead it tracks, and the real
// daemon returns BOTH from the sweep's single closed+mr_url query (verified on
// live data). So the sweep's `Type == "merge-request"` guard is what stands
// between "repair the issue bead" and "resurrect a closed MR ledger record".
// This leg drives that collision against the real daemon: only the issue bead
// may come back open.
func (h *harness) legNeverReopensMRBead() leg {
	const name = "mr-bead-guard"
	url := h.mrURL(4)

	mrBeadID, err := h.seedBead("canonical MR record", "merge-request", "closed", map[string]string{
		"mr_url": url, "mr_state": "opened",
	})
	if err != nil {
		return leg{name, false, "seed MR bead failed: " + err.Error()}
	}
	issueID, err := h.seedBead("drift: issue bead sharing the MR url", "task", "closed", map[string]string{
		"mr_url": url, "mr_state": "opened",
	})
	if err != nil {
		return leg{name, false, "seed issue bead failed: " + err.Error()}
	}

	// Prove the premise: the daemon really does return BOTH beads for the
	// sweep's exact query. If it did not, this leg would pass vacuously and the
	// type guard would be untested.
	res, err := h.daemon.ListBeadsFiltered(h.ctx, beadsapi.ListBeadsQuery{
		Statuses: []string{"closed"},
		Fields:   map[string]string{"mr_url": url},
		Limit:    50,
	})
	if err != nil {
		return leg{name, false, "premise query failed: " + err.Error()}
	}
	sawMRBead := false
	for _, b := range res.Beads {
		if b.ID == mrBeadID {
			sawMRBead = true
		}
	}
	if !sawMRBead {
		return leg{name, false, fmt.Sprintf(
			"premise broken: the daemon did not return the type=merge-request bead %s for the sweep's query, so the type guard is untested here", mrBeadID)}
	}

	srv := stubGitLab([]bridge.GitLabMR{{
		IID: iidOf(url), WebURL: url, State: "opened",
	}}, nil)
	defer srv.Close()

	h.newSweep(srv.URL).ScanOnce(h.ctx)

	mrStatus, err := h.statusOf(mrBeadID)
	if err != nil {
		return leg{name, false, "MR-bead readback failed: " + err.Error()}
	}
	if mrStatus != "closed" {
		return leg{name, false, fmt.Sprintf(
			"type=merge-request bead %s was re-opened (status %q) — the sweep must never re-open the MR ledger bead", mrBeadID, mrStatus)}
	}
	issueStatus, err := h.statusOf(issueID)
	if err != nil {
		return leg{name, false, "issue-bead readback failed: " + err.Error()}
	}
	if issueStatus != "open" {
		return leg{name, false, fmt.Sprintf(
			"issue bead %s has status %q; want open — the type guard must drop only the MR bead, not the repair", issueID, issueStatus)}
	}
	return leg{name, true, fmt.Sprintf(
		"MR bead %s left closed while issue bead %s was re-opened (daemon returned both)", mrBeadID, issueID)}
}

// ── Leg 5: the stale-snapshot race guard ─────────────────────────────────────
//
// The group listing is a snapshot; an MR can merge between the list and the
// write. The sweep therefore re-verifies each MR with a fresh per-MR fetch and
// acts only on a positive `opened`. Here the listing says opened but the
// re-verify says merged: the bead must stay closed. Without this guard the
// sweep would re-open beads for MRs that had just landed — churn on exactly the
// work that succeeded.
func (h *harness) legReverifyRace() leg {
	const name = "reverify-race"
	url := h.mrURL(5)
	id, err := h.seedBead("race: listed open, merged on re-verify", "task", "closed", map[string]string{
		"mr_url": url, "mr_state": "opened",
	})
	if err != nil {
		return leg{name, false, "seed failed: " + err.Error()}
	}

	// Listing reports `opened`; the per-MR re-verify reports `merged`.
	srv := stubGitLab([]bridge.GitLabMR{{
		IID: iidOf(url), WebURL: url, State: "opened",
	}}, map[int]string{iidOf(url): "merged"})
	defer srv.Close()

	h.newSweep(srv.URL).ScanOnce(h.ctx)

	got, err := h.statusOf(id)
	if err != nil {
		return leg{name, false, "readback failed: " + err.Error()}
	}
	if got != "closed" {
		return leg{name, false, fmt.Sprintf(
			"bead %s went to %q — an MR that merged between the listing and the re-verify must NOT be re-opened", id, got)}
	}
	return leg{name, true, fmt.Sprintf("bead %s left closed when re-verify reported merged", id)}
}

// cleanup closes every bead the probe created. Best-effort: a leaked e2e-test
// bead is noise, not a failure, and must never mask a leg result.
func (h *harness) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, id := range h.created {
		_ = h.daemon.UpdateBead(ctx, id, beadsapi.UpdateBeadRequest{Status: strPtr("closed")})
	}
}

func strPtr(s string) *string { return &s }

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// firstEnv returns the first non-empty value among keys, or "".
func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}
