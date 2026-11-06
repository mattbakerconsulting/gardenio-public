package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/server"
	"github.com/spf13/cobra"
)

// admin_gen_dashboards.go — the live-sync entrypoint for the catalog→Grafana
// dashboard pipeline (task ka-0000005728, epic ka-0000002763).
//
// The pipeline's first two links land the model purely in-process:
//   - GeneratePanel (charts_grafana_panel.go, ka-0000000732) maps one catalog
//     ChartDef → one Grafana timeseries panel.
//   - AssembleDashboard (charts_grafana_dashboard.go, ka-0000006749) wraps N
//     generated panels into a full dashboard model keyed by uid.
//
// This command is the third link: it WRITES that generated model into the live
// path the grafana-bridge reads. For each catalog-backed dashboard uid it finds
// the type=dashboard bead (matched by its dashboard_uid field), locates the
// attached grafana:dashboard-model type=doc child, and replaces that doc's
// `content` field with the freshly assembled JSON. The grafana-bridge watches
// those model docs and re-renders the grafana_dashboard ConfigMap the Grafana
// sidecar provisions — so a one-line catalog edit, followed by this command,
// propagates all the way to the live panel with no hand-sync.
//
// Idempotent: re-running with no catalog change rewrites byte-identical content
// (MarshalDashboard is byte-stable), so the bridge sees no diff. --dry-run
// prints what would change without writing.

// dashboardModelDocLabel marks the type=doc bead that carries a dashboard's
// JSON model. It MUST match the controller-side constant of the same name
// (gardenio/controller/internal/bridge/grafana_dashboard_store.go) — that is the
// label the grafana-bridge's ModelDoc lookup keys on, so writing under any
// other label would not reach the live ConfigMap.
const dashboardModelDocLabel = "grafana:dashboard-model"

var adminGenDashboardsCmd = &cobra.Command{
	Use:   "gen-dashboards [uid...]",
	Short: "Regenerate Grafana dashboard model docs from the kbeads chart catalog",
	Long: `Assemble Grafana dashboard models from the chart catalog and write them into
the live grafana:dashboard-model doc beads the grafana-bridge syncs.

For each catalog-backed dashboard uid (all of them by default; or only the uids
named as arguments), this command:

  1. assembles the dashboard model from the catalog (AssembleDashboardByUID);
  2. finds the type=dashboard bead whose dashboard_uid field is that uid;
  3. locates the grafana:dashboard-model type=doc child linked under it;
  4. replaces that doc's content field with the freshly assembled JSON.

The grafana-bridge re-renders the grafana_dashboard ConfigMap from the updated
doc, so the live Grafana panel reflects the catalog. Idempotent — a no-op
catalog produces byte-identical content. Use --dry-run to preview.

--check is the drift guard (ka-0000003225 / ka-0000004040): it never writes and
exits non-zero if any live model doc has drifted from what the catalog assembles
(a catalog panel added but never regenerated to the live doc — the exact
gardenio-costs-candidate defect). Wire it as a scheduled alert or CI job so a
one-line catalog edit that was never followed by gen-dashboards is caught
instead of silently shipping a broken iframe panel.

Examples:
  kd admin gen-dashboards
  kd admin gen-dashboards gardenio-costs-candidate
  kd admin gen-dashboards --dry-run
  kd admin gen-dashboards --check          # non-zero exit on catalog↔live drift`,
	RunE: runAdminGenDashboards,
}

func init() {
	adminGenDashboardsCmd.Flags().Bool("dry-run", false, "report what would change without writing")
	adminGenDashboardsCmd.Flags().Bool("check", false, "drift guard: never write, exit non-zero if any live model doc has drifted from the catalog (implies --dry-run)")
	adminGenDashboardsCmd.Flags().Bool("no-resync", false, "do not nudge the dashboard bead to force a grafana-bridge re-sync (write the model doc only)")
	adminCmd.AddCommand(adminGenDashboardsCmd)
}

// genDashboardResult is one dashboard's outcome, emitted in --json mode.
type genDashboardResult struct {
	UID         string `json:"uid"`
	DashboardID string `json:"dashboard_bead,omitempty"`
	ModelDocID  string `json:"model_doc,omitempty"`
	Action      string `json:"action"` // updated | unchanged | created | skipped
	Bytes       int    `json:"bytes,omitempty"`
	Resynced    bool   `json:"resynced,omitempty"` // dashboard bead nudged to force a grafana-bridge re-sync
	Error       string `json:"error,omitempty"`
}

func runAdminGenDashboards(cmd *cobra.Command, args []string) error {
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	check, _ := cmd.Flags().GetBool("check")
	noResync, _ := cmd.Flags().GetBool("no-resync")
	ctx := cmd.Context()

	// --check is a read-only drift guard: it must never write, so it implies
	// --dry-run. The only difference from a plain --dry-run is the exit status
	// (non-zero on drift) computed below.
	if check {
		dryRun = true
	}

	// Resolve the set of uids to regenerate: explicit args, else every
	// catalog-backed dashboard.
	uids := args
	if len(uids) == 0 {
		uids = server.DashboardUIDs()
	} else {
		uids = append([]string(nil), uids...)
		sort.Strings(uids)
	}

	results := make([]genDashboardResult, 0, len(uids))
	var failures int
	for _, uid := range uids {
		res := genOneDashboard(ctx, uid, dryRun, noResync)
		if res.Error != "" {
			failures++
		}
		results = append(results, res)
	}

	if jsonOutput {
		data, err := json.MarshalIndent(map[string]any{
			"dry_run":    dryRun,
			"check":      check,
			"dashboards": results,
		}, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json output: %w", err)
		}
		fmt.Println(string(data))
	} else {
		for _, r := range results {
			if r.Error != "" {
				fmt.Printf("%-28s ERROR: %s\n", r.UID, r.Error)
				continue
			}
			resync := ""
			if r.Resynced {
				resync = " [resync nudged]"
			}
			fmt.Printf("%-28s %-9s doc=%s (%d bytes)%s\n", r.UID, r.Action, r.ModelDocID, r.Bytes, resync)
		}
	}

	if failures > 0 {
		return fmt.Errorf("gen-dashboards: %d of %d dashboard(s) failed", failures, len(uids))
	}

	// --check is the drift guard: a live model doc that would be created or
	// updated (dry-run actions) has drifted from the catalog. Report every
	// drifted uid and fail so a scheduled alert / CI job goes red until someone
	// runs `kd admin gen-dashboards` to reconcile.
	if check {
		var drifted []string
		for _, r := range results {
			if r.Action == "updated" || r.Action == "created" {
				drifted = append(drifted, r.UID)
			}
		}
		if len(drifted) > 0 {
			return fmt.Errorf(
				"gen-dashboards drift: %d dashboard(s) diverged from the catalog: %s — run `kd admin gen-dashboards` to reconcile the live model docs",
				len(drifted), strings.Join(drifted, ", "))
		}
	}
	return nil
}

// genOneDashboard regenerates a single dashboard's model doc. It never returns
// an error directly — every failure is captured on the result so a bad uid does
// not abort the whole run. When the model doc content actually changes (and
// noResync is false) it also nudges the dashboard bead so the grafana-bridge
// re-syncs — see triggerResync for why a doc-only write is insufficient.
func genOneDashboard(ctx context.Context, uid string, dryRun, noResync bool) genDashboardResult {
	res := genDashboardResult{UID: uid}

	dash, err := server.AssembleDashboardByUID(uid)
	if err != nil {
		res.Error = fmt.Sprintf("assemble: %v", err)
		return res
	}
	model, err := server.MarshalDashboard(dash)
	if err != nil {
		res.Error = fmt.Sprintf("marshal: %v", err)
		return res
	}
	res.Bytes = len(model)

	dashboardID, err := findDashboardBeadByUID(ctx, uid)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	if dashboardID == "" {
		res.Error = fmt.Sprintf("no type=dashboard bead with dashboard_uid=%q", uid)
		return res
	}
	res.DashboardID = dashboardID

	docID, current, err := findModelDoc(ctx, dashboardID)
	if err != nil {
		res.Error = err.Error()
		return res
	}

	// Byte-stable: an unchanged catalog rewrites identical content, so skip the
	// write (and the bridge sees no diff).
	if docID != "" && string(current) == string(model) {
		res.ModelDocID = docID
		res.Action = "unchanged"
		return res
	}

	if dryRun {
		res.ModelDocID = docID
		if docID == "" {
			res.Action = "created" // would create
		} else {
			res.Action = "updated" // would update
		}
		return res
	}

	if docID == "" {
		newID, cerr := createModelDoc(ctx, dashboardID, uid, model)
		if cerr != nil {
			res.Error = cerr.Error()
			return res
		}
		res.ModelDocID = newID
		res.Action = "created"
	} else {
		if uerr := updateModelDocContent(ctx, docID, model); uerr != nil {
			res.Error = uerr.Error()
			return res
		}
		res.ModelDocID = docID
		res.Action = "updated"
	}

	// The model doc content changed, so the live ConfigMap is now stale. The
	// grafana-bridge only re-syncs on type=dashboard bead events and skips when
	// the dashboard bead's dashboard_hash already matches the model — a
	// doc-only write triggers neither. Nudge the dashboard bead to drive the
	// re-sync (unless suppressed).
	if !noResync {
		if rerr := triggerResync(ctx, dashboardID); rerr != nil {
			// The model doc is written; only the live push is degraded. Report
			// it on the result without failing the whole regeneration.
			res.Error = fmt.Sprintf("model written but re-sync nudge failed: %v", rerr)
			return res
		}
		res.Resynced = true
	}
	return res
}

// triggerResync nudges the dashboard bead so the grafana-bridge re-renders the
// live ConfigMap from the freshly written model doc. It clears the
// dashboard_hash loop-breaker field: the empty-string write (a) differs from
// the stored hash, so the bridge's hash-equality skip no longer fires, and
// (b) emits a beads.bead.updated event — the topic GrafanaSync subscribes to.
// The bridge recomputes the hash from the doc, writes the ConfigMap, and
// re-stamps dashboard_hash, closing the loop.
func triggerResync(ctx context.Context, dashboardID string) error {
	fields, err := json.Marshal(map[string]string{"dashboard_hash": ""})
	if err != nil {
		return fmt.Errorf("marshal resync fields: %w", err)
	}
	if _, err := beadsClient.UpdateBead(ctx, dashboardID, &client.UpdateBeadRequest{Fields: fields}); err != nil {
		return fmt.Errorf("nudge dashboard bead %s: %w", dashboardID, err)
	}
	return nil
}

// findDashboardBeadByUID returns the id of the open type=dashboard bead whose
// dashboard_uid field equals uid, or "" when none exists.
func findDashboardBeadByUID(ctx context.Context, uid string) (string, error) {
	resp, err := beadsClient.ListBeads(ctx, &client.ListBeadsRequest{
		Type:         []string{"dashboard"},
		Status:       []string{"open", "in_progress", "blocked", "deferred"},
		FieldFilters: map[string]string{"dashboard_uid": uid},
		Limit:        10,
	})
	if err != nil {
		return "", fmt.Errorf("list dashboards by uid %q: %w", uid, err)
	}
	for _, b := range resp.Beads {
		if b == nil {
			continue
		}
		if beadFieldValue(b, "dashboard_uid") == uid {
			return b.ID, nil
		}
	}
	return "", nil
}

// findModelDoc returns the id and current content of the grafana:dashboard-model
// type=doc bead linked parent-child under dashboardID. The doc is a *reverse*
// dependency of the dashboard (the doc depends-on the dashboard), matching how
// the controller's DashboardStore.ModelDoc and `gb grafana put` link it.
// Returns ("", nil, nil) when no model doc exists yet.
func findModelDoc(ctx context.Context, dashboardID string) (docID string, content []byte, err error) {
	deps, err := beadsClient.GetReverseDependencies(ctx, dashboardID)
	if err != nil {
		return "", nil, fmt.Errorf("reverse deps of %s: %w", dashboardID, err)
	}
	for _, d := range deps {
		if d == nil || d.Type != model.DepParentChild {
			continue
		}
		child, gerr := beadsClient.GetBead(ctx, d.BeadID)
		if gerr != nil || child == nil || string(child.Type) != "doc" {
			continue
		}
		if !hasLabelValue(child.Labels, dashboardModelDocLabel) {
			continue
		}
		return child.ID, []byte(beadFieldValue(child, "content")), nil
	}
	return "", nil, nil
}

// updateModelDocContent replaces the content field of an existing model doc.
// Field updates merge server-side, so only `content` is sent.
func updateModelDocContent(ctx context.Context, docID string, model []byte) error {
	fields, err := json.Marshal(map[string]string{"content": string(model)})
	if err != nil {
		return fmt.Errorf("marshal doc fields: %w", err)
	}
	if _, err := beadsClient.UpdateBead(ctx, docID, &client.UpdateBeadRequest{Fields: fields}); err != nil {
		return fmt.Errorf("update model doc %s: %w", docID, err)
	}
	return nil
}

// createModelDoc creates a grafana:dashboard-model type=doc bead carrying the
// model, links it parent-child under the dashboard bead, and labels it — the
// exact shape the controller's DashboardStore.PutModel writes, so a dashboard
// that has no model doc yet (e.g. authored without one) is brought into the
// synced shape.
func createModelDoc(ctx context.Context, dashboardID, uid string, modelJSON []byte) (string, error) {
	fields, err := json.Marshal(map[string]string{
		"format":  "code",
		"content": string(modelJSON),
	})
	if err != nil {
		return "", fmt.Errorf("marshal doc fields: %w", err)
	}
	created, err := beadsClient.CreateBead(ctx, &client.CreateBeadRequest{
		Title:       "dashboard model: " + dashboardID,
		Type:        "doc",
		Kind:        string(model.KindData),
		Priority:    2,
		CreatedBy:   actor,
		Description: "parent(" + dashboardID + ") — Grafana dashboard JSON model generated from the kbeads chart catalog (uid=" + uid + ")",
		Fields:      fields,
	})
	if err != nil {
		return "", fmt.Errorf("create model doc: %w", err)
	}
	if _, err := beadsClient.AddDependency(ctx, &client.AddDependencyRequest{
		BeadID:      created.ID,
		DependsOnID: dashboardID,
		Type:        string(model.DepParentChild),
		CreatedBy:   actor,
	}); err != nil {
		return created.ID, fmt.Errorf("link model doc %s → %s: %w", created.ID, dashboardID, err)
	}
	if _, err := beadsClient.AddLabel(ctx, created.ID, dashboardModelDocLabel); err != nil {
		return created.ID, fmt.Errorf("label model doc %s: %w", created.ID, err)
	}
	return created.ID, nil
}

// hasLabelValue reports whether labels contains want.
func hasLabelValue(labels []string, want string) bool {
	for _, l := range labels {
		if strings.EqualFold(l, want) {
			return true
		}
	}
	return false
}
