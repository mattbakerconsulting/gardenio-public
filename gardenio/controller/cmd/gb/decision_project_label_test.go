package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// captureDecisionCreate spins up a stub daemon that records the labels sent on
// the decision-create POST, and serves the requesting task at
// GET /v1/beads/<taskID> carrying taskLabels. It returns a pointer to the
// captured labels plus the recorded label filters used on the dedup list call.
func captureDecisionCreate(t *testing.T, taskID string, taskLabels []string) (*[]string, *[]string) {
	t.Helper()
	createdLabels := &[]string{}
	dedupLabels := &[]string{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/beads" && r.Method == http.MethodPost:
			var req struct {
				Labels []string `json:"labels"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			*createdLabels = req.Labels
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{"id": "gd-dec-label"})
		case strings.HasSuffix(r.URL.Path, "/dependencies") && r.Method == http.MethodPost:
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{})
		case r.URL.Path == "/v1/beads/"+taskID && r.Method == http.MethodGet:
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]any{
				"id": taskID, "type": "task", "status": "open", "labels": taskLabels,
			})
		case strings.HasPrefix(r.URL.Path, "/v1/beads/") && r.Method == http.MethodGet:
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]any{"id": "gd-other", "type": "task", "status": "open"})
		default:
			// The dedup list call (GET /v1/beads?...): record its label filter.
			if r.Method == http.MethodGet {
				*dedupLabels = labelParams(r.URL.Query())
			}
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
		}
	}))
	t.Cleanup(srv.Close)

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	t.Cleanup(func() { daemon = oldDaemon })

	return createdLabels, dedupLabels
}

// labelParams pulls the label filter values off a list query regardless of
// whether the client encodes them as repeated or comma-joined params.
func labelParams(q url.Values) []string {
	var out []string
	for _, key := range []string{"label", "labels"} {
		for _, v := range q[key] {
			for part := range strings.SplitSeq(v, ",") {
				if part = strings.TrimSpace(part); part != "" {
					out = append(out, part)
				}
			}
		}
	}
	return out
}

// runDecisionCreate drives decisionCreateCmd with the standard minimal flags.
func runDecisionCreate(t *testing.T, prompt, requestingTask string) {
	t.Helper()
	cmd := decisionCreateCmd
	cmd.SetContext(context.Background())
	cmd.Flags().Set("prompt", prompt)
	cmd.Flags().Set("option", "a:Option A:report")
	cmd.Flags().Set("no-wait", "true")
	if requestingTask != "" {
		cmd.Flags().Set("requesting-task", requestingTask)
	}
	t.Cleanup(func() { clearDecisionCreateFlags(cmd) })

	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestDecisionCreate_StampsProjectLabelFromRequestingTask is the core
// regression test for ka-0000003942: a decision created with --requesting-task
// must inherit that task's project:<p> label. Before this fix 20/20 open
// decisions carried no project label, so `kd list --type=decision
// -l project:gardenio` returned 0 and hid every open decision from the
// gardener-release-owner runbook's Step 1.5 filter.
func TestDecisionCreate_StampsProjectLabelFromRequestingTask(t *testing.T) {
	t.Setenv("KD_PROJECT", "")
	t.Setenv("BOAT_PROJECT", "")

	created, _ := captureDecisionCreate(t, "gd-task-proj", []string{"kind:ced-audit", "project:gardenio"})
	runDecisionCreate(t, "should we ship the label fix", "gd-task-proj")

	if !slices.Contains(*created, "project:gardenio") {
		t.Errorf("created labels = %v, want to include project:gardenio", *created)
	}
}

// TestDecisionCreate_StampsProjectLabelFromEnv covers the common call shape:
// no --requesting-task, so the project comes from the ambient KD_PROJECT /
// BOAT_PROJECT the agent pod already carries. Without this fallback the
// majority of decisions would still be created unlabeled.
func TestDecisionCreate_StampsProjectLabelFromEnv(t *testing.T) {
	t.Setenv("KD_PROJECT", "gardenio")
	t.Setenv("BOAT_PROJECT", "")

	created, _ := captureDecisionCreate(t, "", nil)
	runDecisionCreate(t, "env project label decision", "")

	if !slices.Contains(*created, "project:gardenio") {
		t.Errorf("created labels = %v, want to include project:gardenio from env", *created)
	}
}

// TestDecisionCreate_RequestingTaskProjectBeatsEnv proves the precedence: the
// work the decision blocks owns the decision. A gardener running with
// KD_PROJECT=gardenio that raises a decision for a task in another project must
// file it under THAT project, or the decision lands in a queue whose owners
// cannot act on it.
func TestDecisionCreate_RequestingTaskProjectBeatsEnv(t *testing.T) {
	t.Setenv("KD_PROJECT", "gardenio")
	t.Setenv("BOAT_PROJECT", "")

	created, _ := captureDecisionCreate(t, "gd-task-other", []string{"project:kbeads"})
	runDecisionCreate(t, "cross project decision precedence", "gd-task-other")

	if !slices.Contains(*created, "project:kbeads") {
		t.Errorf("created labels = %v, want project:kbeads (requesting task wins over env)", *created)
	}
	if slices.Contains(*created, "project:gardenio") {
		t.Errorf("created labels = %v, must not carry the ambient env project", *created)
	}
}

// TestDecisionCreate_NoProjectLeavesLabelsEmpty proves the stamp is additive,
// never invented: with no requesting task and no project env, the decision is
// created exactly as before rather than carrying a bogus "project:" label.
func TestDecisionCreate_NoProjectLeavesLabelsEmpty(t *testing.T) {
	t.Setenv("KD_PROJECT", "")
	t.Setenv("BOAT_PROJECT", "")

	created, _ := captureDecisionCreate(t, "", nil)
	runDecisionCreate(t, "unlabeled decision stays unlabeled", "")

	for _, l := range *created {
		if strings.HasPrefix(l, "project:") {
			t.Errorf("created labels = %v, want no project label when none resolves", *created)
		}
	}
}

// TestDecisionCreate_FallsBackToEnvWhenTaskHasNoProject covers a requesting
// task that carries labels but no project:<p> among them — the ambient env
// still supplies the project rather than the decision going unlabeled.
func TestDecisionCreate_FallsBackToEnvWhenTaskHasNoProject(t *testing.T) {
	t.Setenv("KD_PROJECT", "gardenio")
	t.Setenv("BOAT_PROJECT", "")

	created, _ := captureDecisionCreate(t, "gd-task-nolabel", []string{"kind:ced-audit"})
	runDecisionCreate(t, "task without project label decision", "gd-task-nolabel")

	if !slices.Contains(*created, "project:gardenio") {
		t.Errorf("created labels = %v, want env project when the task carries none", *created)
	}
}

// TestDecisionCreate_DedupScopedToResolvedProject proves the dedup guard
// (ka-0000003825) searches the SAME project the create is about to stamp. This
// is the second half of the bug: findDuplicateOpenDecision filtered open
// decisions by project:<p>, but no decision ever carried that label, so the
// filter matched zero rows and the guard could never fire.
func TestDecisionCreate_DedupScopedToResolvedProject(t *testing.T) {
	t.Setenv("KD_PROJECT", "gardenio")
	t.Setenv("BOAT_PROJECT", "")

	_, dedup := captureDecisionCreate(t, "gd-task-dedup", []string{"project:kbeads"})
	runDecisionCreate(t, "dedup scope decision", "gd-task-dedup")

	if !slices.Contains(*dedup, "project:kbeads") {
		t.Errorf("dedup label filter = %v, want it scoped to project:kbeads (the project being stamped)", *dedup)
	}
}
