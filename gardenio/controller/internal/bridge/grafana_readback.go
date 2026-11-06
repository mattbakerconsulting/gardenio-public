// Package bridge — grafana-bridge read-back poll (Grafana → beads).
//
// W2-impl-b. OSS Grafana has no webhook for dashboard saves, so reflecting
// Grafana-side edits back into beads is poll-based (like jira-bridge's JQL
// poll), keyed on dashboard uid. The loop-breaker is the dashboard_hash +
// dashboard_source pair already stamped by the beads→Grafana sync:
//
//   - Each cycle, list Grafana dashboards and fetch each model.
//   - Compute the normalized hash. If it matches the bead's dashboard_hash,
//     the two sides agree — no-op (this is what stops a beads→Grafana write
//     from bouncing back as a spurious "Grafana edit").
//   - If the Grafana hash differs, a human edited the dashboard in Grafana.
//     Reconcile per the conflict policy (§5):
//     beads-wins   — keep the bead model; re-push is handled by the sync
//     side; archive the Grafana model for audit.
//     grafana-wins — pull the Grafana model into the bead's model doc and
//     stamp source=grafana (default).
//     manual       — open a decision bead; leave both sides untouched.
//
// A "conflict" in the strict sense is when BOTH sides changed since the last
// agreed hash. We detect the bead-side change by re-hashing the bead's own
// model doc and comparing it to the stored dashboard_hash: if they match, the
// bead is in sync and only Grafana moved — a plain Grafana edit, no contention.
// If the bead's model doc hash also differs from dashboard_hash (a pending
// beads-authored change the sync side may not have pushed yet) while Grafana
// differs too, that is a true conflict and the policy decides the winner.
//
// Design: gardenio/docs/research/obs-w2-grafana-bridge.md §4b, §5; build plan §8
// steps 4 & 6. Sibling of GrafanaSync (beads→Grafana).
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// ConflictPolicy selects how a beads↔Grafana divergence is resolved.
type ConflictPolicy string

const (
	// ConflictBeadsWins keeps the bead as source of truth; the Grafana-side
	// model is archived for audit and the bead is left for the sync side to
	// re-push.
	ConflictBeadsWins ConflictPolicy = "beads-wins"
	// ConflictGrafanaWins pulls the Grafana model into the bead (default).
	ConflictGrafanaWins ConflictPolicy = "grafana-wins"
	// ConflictManual opens a decision bead and touches neither side.
	ConflictManual ConflictPolicy = "manual"
)

// ParseConflictPolicy maps a string to a ConflictPolicy, defaulting to
// grafana-wins for the read-back direction (a Grafana edit is the common case;
// see §9 — start conservative, let the operator flip to manual per dashboard).
func ParseConflictPolicy(s string) ConflictPolicy {
	switch ConflictPolicy(s) {
	case ConflictBeadsWins:
		return ConflictBeadsWins
	case ConflictManual:
		return ConflictManual
	default:
		return ConflictGrafanaWins
	}
}

// GrafanaReadback polls Grafana and reflects dashboard edits back into beads.
type GrafanaReadback struct {
	store    *DashboardStore
	grafana  GrafanaReader
	owners   DashboardOwnerReader
	policy   ConflictPolicy
	interval time.Duration
	logger   *slog.Logger
	dedup    *Dedup
}

// GrafanaReadbackConfig configures a GrafanaReadback.
type GrafanaReadbackConfig struct {
	Store   *DashboardStore
	Grafana GrafanaReader
	// Owners, when set, names the bead each uid's bridge ConfigMap was
	// provisioned from; read-back prefers that bead over a uid lookup.
	Owners   DashboardOwnerReader
	Policy   ConflictPolicy
	Interval time.Duration
	Logger   *slog.Logger
}

// DashboardOwnerReader reads which dashboard bead a uid is provisioned from.
// GrafanaConfigMapWriter implements it from the gardenio.io/dashboard-bead
// annotation.
type DashboardOwnerReader interface {
	// ProvisionedBead returns the bead id, or "" when the uid has no
	// bridge-provisioned owner.
	ProvisionedBead(ctx context.Context, uid string) (string, error)
}

// NewGrafanaReadback constructs a GrafanaReadback. A zero interval defaults to
// 60s; an empty policy defaults to grafana-wins.
func NewGrafanaReadback(cfg GrafanaReadbackConfig) *GrafanaReadback {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = 60 * time.Second
	}
	policy := cfg.Policy
	if policy == "" {
		policy = ConflictGrafanaWins
	}
	return &GrafanaReadback{
		store:    cfg.Store,
		grafana:  cfg.Grafana,
		owners:   cfg.Owners,
		policy:   policy,
		interval: interval,
		logger:   logger,
		dedup:    NewDedup(logger),
	}
}

// Run polls at the configured interval until ctx is canceled. It runs one
// PollOnce immediately on start (catch-up after a restart).
func (r *GrafanaReadback) Run(ctx context.Context) error {
	r.logger.Info("grafana-readback poller started", "interval", r.interval, "policy", r.policy)
	go r.dedup.StartCleanup(ctx)

	r.PollOnce(ctx)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			r.PollOnce(ctx)
		}
	}
}

// PollOnce performs one full read-back sweep: list Grafana dashboards and
// reconcile each against its bead. Exposed for tests and catch-up.
func (r *GrafanaReadback) PollOnce(ctx context.Context) {
	refs, err := r.grafana.Search(ctx)
	if err != nil {
		r.logger.Warn("grafana-readback: search failed", "error", err)
		return
	}
	reconciled, conflicts, skipped := 0, 0, 0
	for _, ref := range refs {
		if ref.UID == "" {
			continue
		}
		outcome := r.reconcile(ctx, ref.UID)
		switch outcome {
		case readbackReconciled:
			reconciled++
		case readbackConflict:
			conflicts++
		default:
			skipped++
		}
	}
	r.logger.Info("grafana-readback: poll complete",
		"dashboards", len(refs), "reconciled", reconciled, "conflicts", conflicts, "skipped", skipped)
}

// readbackOutcome classifies what a single reconcile did, for poll-cycle stats.
type readbackOutcome int

const (
	readbackSkipped readbackOutcome = iota
	readbackReconciled
	readbackConflict
)

// reconcile pulls one Grafana dashboard and reconciles it against the matching
// dashboard bead.
func (r *GrafanaReadback) reconcile(ctx context.Context, uid string) readbackOutcome {
	beadID, ok := r.beadForUID(ctx, uid)
	if !ok {
		return readbackSkipped
	}
	if beadID == "" {
		// Grafana has a dashboard with no bead. Out of scope for read-back —
		// authorship begins in beads (or via backfill); don't auto-import here.
		r.logger.Debug("grafana-readback: no bead for grafana uid; skipping", "uid", uid)
		return readbackSkipped
	}

	model, gVersion, err := r.grafana.DashboardByUID(ctx, uid)
	if err != nil {
		r.logger.Warn("grafana-readback: fetch dashboard failed", "uid", uid, "bead", beadID, "error", err)
		return readbackSkipped
	}
	if len(model) == 0 {
		// 404 — dashboard removed in Grafana. Leave the bead; deletion policy
		// is out of scope for W2-impl-b (the bead remains as the record).
		r.logger.Info("grafana-readback: dashboard not found in Grafana; leaving bead", "uid", uid, "bead", beadID)
		return readbackSkipped
	}

	gHash, err := HashDashboardModel(model)
	if err != nil {
		r.logger.Warn("grafana-readback: hashing grafana model failed", "uid", uid, "bead", beadID, "error", err)
		return readbackSkipped
	}

	detail, err := r.store.daemon.GetBead(ctx, beadID)
	if err != nil || detail == nil {
		r.logger.Warn("grafana-readback: get bead failed", "uid", uid, "bead", beadID, "error", err)
		return readbackSkipped
	}
	beadHash := detail.Fields["dashboard_hash"]

	// Loop-breaker: Grafana model already matches the bead's recorded hash —
	// the two sides agree, nothing to do. This is what stops a beads→Grafana
	// write from echoing back as a phantom Grafana edit.
	if beadHash == gHash {
		return readbackSkipped
	}

	// Restart/intra-cycle dedup: don't re-pull the same (uid, hash) twice
	// before the stamped hash becomes observable. Seen check-and-sets, so a
	// first sighting returns false and is recorded atomically.
	dedupKey := "readback:" + beadID + ":" + gHash
	if r.dedup.Seen(dedupKey) {
		return readbackSkipped
	}

	// Determine whether the bead also has a pending unsynced change. The
	// beads→Grafana sync stamps dashboard_source=beads after pushing; if the
	// bead-side model hash equals the stored dashboard_hash, beads is in sync
	// and only Grafana moved → a plain Grafana edit, not a conflict.
	beadChanged, err := r.beadSideChanged(ctx, beadID, beadHash)
	if err != nil {
		r.logger.Warn("grafana-readback: bead-side change check failed", "uid", uid, "bead", beadID, "error", err)
		return readbackSkipped
	}

	if !beadChanged {
		// Plain Grafana-side edit: pull it into the bead.
		r.applyGrafanaModel(ctx, beadID, uid, model, gHash, gVersion)
		return readbackReconciled
	}

	// True conflict: both sides diverged from the last agreed hash.
	r.resolveConflict(ctx, beadID, uid, model, gHash, gVersion)
	return readbackConflict
}

// beadForUID picks the bead a Grafana uid reads back into. The bead named by
// the uid's ConfigMap annotation wins when it is an active, project-allowed
// dashboard bead for this uid; an annotation naming any other bead writes
// nothing. With no annotation it falls back to the fenced FindByUID, so a bead
// outside the fleet projects never receives a fleet model (gd-eGC3fokT8T).
// ok=false means skip this uid.
func (r *GrafanaReadback) beadForUID(ctx context.Context, uid string) (beadID string, ok bool) {
	if r.owners != nil {
		owner, err := r.owners.ProvisionedBead(ctx, uid)
		if err != nil {
			r.logger.Warn("grafana-readback: reading provisioned bead failed", "uid", uid, "error", err)
			return "", false
		}
		if owner != "" {
			valid, reason, verr := r.store.DashboardForUID(ctx, owner, uid)
			if verr != nil {
				r.logger.Warn("grafana-readback: checking provisioned bead failed", "uid", uid, "bead", owner, "error", verr)
				return "", false
			}
			if !valid {
				r.logger.Warn("grafana-readback: provisioned bead does not qualify; writing nothing",
					"uid", uid, "bead", owner, "reason", reason)
				return "", false
			}
			return owner, true
		}
	}
	beadID, err := r.store.FindByUID(ctx, uid)
	if err != nil {
		r.logger.Warn("grafana-readback: find-by-uid failed", "uid", uid, "error", err)
		return "", false
	}
	return beadID, true
}

// beadSideChanged reports whether the bead's current model doc differs from the
// recorded dashboard_hash — i.e. there is a beads-authored edit not yet stamped
// as synced. When the bead has no model doc or no recorded hash, it is treated
// as not-changed (Grafana is authoritative for a fresh/never-synced bead).
func (r *GrafanaReadback) beadSideChanged(ctx context.Context, beadID, beadHash string) (bool, error) {
	if beadHash == "" {
		return false, nil
	}
	_, beadModel, err := r.store.ModelDoc(ctx, beadID)
	if err != nil {
		return false, err
	}
	if len(beadModel) == 0 {
		return false, nil
	}
	beadModelHash, err := HashDashboardModel(beadModel)
	if err != nil {
		return false, err
	}
	return beadModelHash != beadHash, nil
}

// applyGrafanaModel writes the Grafana model into the bead's model doc and
// stamps provenance (hash, source=grafana, version) so the next beads→Grafana
// sync sees "hash matches" and does not echo the change back.
func (r *GrafanaReadback) applyGrafanaModel(ctx context.Context, beadID, uid string, model []byte, hash string, version int) {
	if _, err := r.store.PutModel(ctx, beadID, model); err != nil {
		r.logger.Error("grafana-readback: writing grafana model to bead failed", "uid", uid, "bead", beadID, "error", err)
		return
	}
	fields := map[string]string{
		"dashboard_hash":   hash,
		"dashboard_source": "grafana",
	}
	if version > 0 {
		fields["dashboard_version"] = strconv.Itoa(version)
	}
	if err := r.store.daemon.UpdateBeadFields(ctx, beadID, fields); err != nil {
		r.logger.Warn("grafana-readback: stamping provenance failed", "uid", uid, "bead", beadID, "error", err)
	}
	r.logger.Info("grafana-readback: pulled Grafana edit into bead",
		"uid", uid, "bead", beadID, "version", version, "hash", shortHash(hash))
}

// resolveConflict applies the configured conflict policy when both sides
// diverged.
func (r *GrafanaReadback) resolveConflict(ctx context.Context, beadID, uid string, gModel []byte, gHash string, gVersion int) {
	switch r.policy {
	case ConflictGrafanaWins:
		// Archive the losing bead-side model, then pull Grafana in.
		if _, beadModel, err := r.store.ModelDoc(ctx, beadID); err == nil && len(beadModel) > 0 {
			if _, aerr := r.store.ArchiveConflict(ctx, beadID, "beads", beadVersion(ctx, r.store, beadID), beadModel); aerr != nil {
				r.logger.Warn("grafana-readback: archiving dropped beads model failed", "uid", uid, "bead", beadID, "error", aerr)
			}
		}
		r.applyGrafanaModel(ctx, beadID, uid, gModel, gHash, gVersion)
		r.comment(ctx, beadID, fmt.Sprintf(
			"grafana-bridge conflict resolved **grafana-wins**: pulled Grafana edit (uid=%s, v%d) into this bead; the prior beads-authored model was archived as a conflict doc for audit.", uid, gVersion))

	case ConflictBeadsWins:
		// Keep beads; archive the Grafana model. The beads→Grafana sync side
		// re-pushes the bead model (its hash still differs from the stored
		// one once we leave dashboard_hash alone), restoring Grafana.
		if _, aerr := r.store.ArchiveConflict(ctx, beadID, "grafana", gVersion, gModel); aerr != nil {
			r.logger.Warn("grafana-readback: archiving dropped grafana model failed", "uid", uid, "bead", beadID, "error", aerr)
		}
		r.comment(ctx, beadID, fmt.Sprintf(
			"grafana-bridge conflict resolved **beads-wins**: kept the beads-authored model; the divergent Grafana edit (uid=%s, v%d) was archived as a conflict doc. The bridge will re-push the beads model to Grafana.", uid, gVersion))
		r.logger.Info("grafana-readback: conflict resolved beads-wins", "uid", uid, "bead", beadID)

	case ConflictManual:
		r.openConflictDecision(ctx, beadID, uid, gModel, gVersion)
	}
}

// openConflictDecision archives the Grafana model and opens a decision bead so
// an operator picks the winner. Neither live model is touched.
func (r *GrafanaReadback) openConflictDecision(ctx context.Context, beadID, uid string, gModel []byte, gVersion int) {
	archiveID, aerr := r.store.ArchiveConflict(ctx, beadID, "grafana", gVersion, gModel)
	if aerr != nil {
		r.logger.Warn("grafana-readback: archiving grafana model for manual conflict failed", "uid", uid, "bead", beadID, "error", aerr)
	}
	// The background goes in the `context` field, where every decision
	// surface reads it (internal/decisionview); the description keeps the
	// parent marker and the one-line summary for the bead page.
	background := fmt.Sprintf(
		"Dashboard bead %s and Grafana dashboard uid=%s were both edited since the last sync, so neither side was written. "+
			"Grafana wins pulls the Grafana edit (v%d, archived at %s) into the bead; Beads wins keeps the bead's model and pushes it back to Grafana. "+
			"The losing model stays in the archive.",
		beadID, uid, gVersion, archiveID)
	fields := map[string]string{
		"dashboard_uid": uid,
		"prompt": fmt.Sprintf(
			"Dashboard %s (uid=%s) was edited in BOTH beads and Grafana since the last sync. Pick the winner. The Grafana model (v%d) is archived at %s.",
			beadID, uid, gVersion, archiveID),
		"context": background,
		"options": `[` +
			`{"id":"grafana","short":"Grafana wins","label":"Pull the Grafana edit into the bead","artifact_type":"diff-summary"},` +
			`{"id":"beads","short":"Beads wins","label":"Keep the beads model; re-push to Grafana","artifact_type":"diff-summary"}` +
			`]`,
	}
	fieldsJSON, _ := json.Marshal(fields)
	decID, err := r.store.daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:       "Dashboard conflict: " + uid,
		Type:        "decision",
		Kind:        "issue",
		Priority:    1,
		CreatedBy:   r.store.actor,
		Description: "parent(" + beadID + ") — beads↔Grafana dashboard conflict; operator must pick the winner.",
		Fields:      fieldsJSON,
	})
	if err != nil {
		r.logger.Error("grafana-readback: opening conflict decision failed", "uid", uid, "bead", beadID, "error", err)
		return
	}
	if derr := r.store.daemon.AddDependency(ctx, beadID, decID, "blocks", r.store.actor); derr != nil {
		r.logger.Warn("grafana-readback: linking conflict decision failed", "uid", uid, "bead", beadID, "decision", decID, "error", derr)
	}
	r.comment(ctx, beadID, fmt.Sprintf(
		"grafana-bridge detected a beads↔Grafana conflict for uid=%s and opened decision %s (policy=manual). Neither side was modified.", uid, decID))
	r.logger.Info("grafana-readback: opened manual conflict decision", "uid", uid, "bead", beadID, "decision", decID)
}

// comment posts a provenance comment, logging (not failing) on error.
func (r *GrafanaReadback) comment(ctx context.Context, beadID, text string) {
	if err := r.store.daemon.AddComment(ctx, beadID, r.store.actor, text); err != nil {
		r.logger.Warn("grafana-readback: posting comment failed", "bead", beadID, "error", err)
	}
}

// beadVersion reads the bead's recorded dashboard_version as an int (0 if
// unset/unparseable), for tagging archived conflict artifacts.
func beadVersion(ctx context.Context, store *DashboardStore, beadID string) int {
	detail, err := store.daemon.GetBead(ctx, beadID)
	if err != nil || detail == nil {
		return 0
	}
	v, _ := strconv.Atoi(detail.Fields["dashboard_version"])
	return v
}

// shortHash trims a hex hash for log lines.
func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
