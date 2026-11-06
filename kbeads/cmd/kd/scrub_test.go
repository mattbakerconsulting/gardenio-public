package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
)

// resetScrubApproveFlags mirrors resetScrubRequestFlags for scrubApproveCmd,
// so --reason/--artifact don't leak between tests (RunE reads flags off the
// shared command, not off argv).
func resetScrubApproveFlags(t *testing.T) {
	t.Helper()
	scrubApproveCmd.Flags().VisitAll(func(f *pflag.Flag) {
		f.Changed = false
		_ = f.Value.Set(f.DefValue)
	})
}

// resetScrubRequestFlags clears every flag scrubRequestCmd defines back to
// its zero value, so tests don't leak --taint/--kind/--scope/etc. into one
// another (RunE reads flags directly off the shared command, not off argv).
// StringSlice flags (--bead-ids, --labels-any) accumulate across Set calls
// and carry their Changed bit between RunE invocations, so they need
// SliceValue.Replace rather than Set (ka-0000007843 pattern; see team_test.go
// resetTeamSliceFlags).
func resetScrubRequestFlags(t *testing.T) {
	t.Helper()
	scrubRequestCmd.Flags().VisitAll(func(f *pflag.Flag) {
		f.Changed = false
		if sv, ok := f.Value.(pflag.SliceValue); ok {
			_ = sv.Replace([]string{})
			return
		}
		_ = f.Value.Set(f.DefValue)
	})
}

func TestScrubRequest_NoModeFlag(t *testing.T) {
	if scrubRequestCmd.Flags().Lookup("mode") != nil {
		t.Error("scrub request must not have a --mode flag (mode is read from the taint, not requester-controlled)")
	}
	if scrubRequestCmd.Flags().Lookup("detaint-mode") != nil {
		t.Error("scrub request must not have a --detaint-mode flag (mode is read from the taint, not requester-controlled)")
	}
}

func TestScrubRequest_BeadIDsScope(t *testing.T) {
	mc := newMockClient()
	mc.CreateScrubRequestFn = func(req *client.CreateScrubRequestRequest) (*client.ScrubRequestView, error) {
		fields, _ := json.Marshal(map[string]any{"resolved_bead_ids": []string{"gd-a", "gd-b"}})
		return &client.ScrubRequestView{
			Bead:        &model.Bead{ID: "gd-scrub-1", Type: model.TypeScrubRequest, Fields: fields},
			DetaintMode: "strip",
		}, nil
	}
	withMockClient(t, mc)
	resetScrubRequestFlags(t)
	t.Cleanup(func() { resetScrubRequestFlags(t) })

	if err := scrubRequestCmd.Flags().Set("taint", "acme-production"); err != nil {
		t.Fatal(err)
	}
	if err := scrubRequestCmd.Flags().Set("kind", "detaint"); err != nil {
		t.Fatal(err)
	}
	if err := scrubRequestCmd.Flags().Set("bead-ids", "gd-a,gd-b"); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		if err := scrubRequestCmd.RunE(scrubRequestCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if len(mc.CreateScrubRequestCalls) != 1 {
		t.Fatalf("expected 1 CreateScrubRequest call, got %d", len(mc.CreateScrubRequestCalls))
	}
	call := mc.CreateScrubRequestCalls[0]
	if call.Taint != "acme-production" || call.Kind != "detaint" {
		t.Errorf("unexpected call: %+v", call)
	}
	var scope struct {
		BeadIDs []string `json:"bead_ids"`
	}
	if err := json.Unmarshal(call.Scope, &scope); err != nil {
		t.Fatalf("decoding scope: %v", err)
	}
	if len(scope.BeadIDs) != 2 || scope.BeadIDs[0] != "gd-a" || scope.BeadIDs[1] != "gd-b" {
		t.Errorf("expected bead_ids=[gd-a gd-b], got %v", scope.BeadIDs)
	}

	for _, want := range []string{"gd-scrub-1", "strip", "gd-a", "gd-b"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}
}

func TestScrubRequest_RawScopeJSON(t *testing.T) {
	mc := newMockClient()
	mc.CreateScrubRequestFn = func(req *client.CreateScrubRequestRequest) (*client.ScrubRequestView, error) {
		return &client.ScrubRequestView{Bead: &model.Bead{ID: "gd-scrub-2", Type: model.TypeScrubRequest}}, nil
	}
	withMockClient(t, mc)
	resetScrubRequestFlags(t)
	t.Cleanup(func() { resetScrubRequestFlags(t) })

	if err := scrubRequestCmd.Flags().Set("taint", "acme-production"); err != nil {
		t.Fatal(err)
	}
	if err := scrubRequestCmd.Flags().Set("kind", "purge"); err != nil {
		t.Fatal(err)
	}
	rawScope := `{"older_than_days":30,"labels_any":["project:acme"]}`
	if err := scrubRequestCmd.Flags().Set("scope", rawScope); err != nil {
		t.Fatal(err)
	}

	captureStdout(t, func() {
		if err := scrubRequestCmd.RunE(scrubRequestCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	call := mc.CreateScrubRequestCalls[0]
	if string(call.Scope) != rawScope {
		t.Errorf("expected raw scope %q forwarded verbatim, got %q", rawScope, call.Scope)
	}

	// A purge response with no DetaintMode set must print no Detaint Mode line.
}

func TestScrubRequest_PurgeResponseNoDetaintModeLine(t *testing.T) {
	mc := newMockClient()
	mc.CreateScrubRequestFn = func(req *client.CreateScrubRequestRequest) (*client.ScrubRequestView, error) {
		return &client.ScrubRequestView{Bead: &model.Bead{ID: "gd-scrub-3", Type: model.TypeScrubRequest}}, nil
	}
	withMockClient(t, mc)
	resetScrubRequestFlags(t)
	t.Cleanup(func() { resetScrubRequestFlags(t) })

	if err := scrubRequestCmd.Flags().Set("taint", "acme-production"); err != nil {
		t.Fatal(err)
	}
	if err := scrubRequestCmd.Flags().Set("kind", "purge"); err != nil {
		t.Fatal(err)
	}
	if err := scrubRequestCmd.Flags().Set("bead-ids", "gd-a"); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		if err := scrubRequestCmd.RunE(scrubRequestCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})
	if strings.Contains(out, "Detaint Mode") {
		t.Errorf("expected no Detaint Mode line for a purge response, got:\n%s", out)
	}
}

func TestScrubRequest_MissingTaintRejected(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	resetScrubRequestFlags(t)
	t.Cleanup(func() { resetScrubRequestFlags(t) })

	if err := scrubRequestCmd.Flags().Set("kind", "detaint"); err != nil {
		t.Fatal(err)
	}
	if err := scrubRequestCmd.Flags().Set("bead-ids", "gd-a"); err != nil {
		t.Fatal(err)
	}

	err := scrubRequestCmd.RunE(scrubRequestCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "--taint") {
		t.Fatalf("expected a --taint required error, got %v", err)
	}
	if len(mc.CreateScrubRequestCalls) != 0 {
		t.Errorf("expected no client call, got %d", len(mc.CreateScrubRequestCalls))
	}
}

func TestScrubRequest_InvalidKindRejected(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	resetScrubRequestFlags(t)
	t.Cleanup(func() { resetScrubRequestFlags(t) })

	if err := scrubRequestCmd.Flags().Set("taint", "acme-production"); err != nil {
		t.Fatal(err)
	}
	if err := scrubRequestCmd.Flags().Set("kind", "delete"); err != nil {
		t.Fatal(err)
	}
	if err := scrubRequestCmd.Flags().Set("bead-ids", "gd-a"); err != nil {
		t.Fatal(err)
	}

	err := scrubRequestCmd.RunE(scrubRequestCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "--kind") {
		t.Fatalf("expected a --kind validation error, got %v", err)
	}
}

func TestScrubRequest_ScopeAndFlagsMutuallyExclusive(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	resetScrubRequestFlags(t)
	t.Cleanup(func() { resetScrubRequestFlags(t) })

	if err := scrubRequestCmd.Flags().Set("taint", "acme-production"); err != nil {
		t.Fatal(err)
	}
	if err := scrubRequestCmd.Flags().Set("kind", "detaint"); err != nil {
		t.Fatal(err)
	}
	if err := scrubRequestCmd.Flags().Set("scope", `{"bead_ids":["gd-a"]}`); err != nil {
		t.Fatal(err)
	}
	if err := scrubRequestCmd.Flags().Set("bead-ids", "gd-a"); err != nil {
		t.Fatal(err)
	}

	err := scrubRequestCmd.RunE(scrubRequestCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("expected a mutual-exclusivity error, got %v", err)
	}
}

func TestScrubRequest_ScopeRequired(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	resetScrubRequestFlags(t)
	t.Cleanup(func() { resetScrubRequestFlags(t) })

	if err := scrubRequestCmd.Flags().Set("taint", "acme-production"); err != nil {
		t.Fatal(err)
	}
	if err := scrubRequestCmd.Flags().Set("kind", "detaint"); err != nil {
		t.Fatal(err)
	}

	err := scrubRequestCmd.RunE(scrubRequestCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "--scope") {
		t.Fatalf("expected a --scope required error, got %v", err)
	}
}

func TestScrubRequest_ServerRejection(t *testing.T) {
	mc := newMockClient()
	mc.CreateScrubRequestErr = fmt.Errorf("requester holds no grant for taint")
	withMockClient(t, mc)
	resetScrubRequestFlags(t)
	t.Cleanup(func() { resetScrubRequestFlags(t) })

	if err := scrubRequestCmd.Flags().Set("taint", "acme-production"); err != nil {
		t.Fatal(err)
	}
	if err := scrubRequestCmd.Flags().Set("kind", "detaint"); err != nil {
		t.Fatal(err)
	}
	if err := scrubRequestCmd.Flags().Set("bead-ids", "gd-a"); err != nil {
		t.Fatal(err)
	}

	err := scrubRequestCmd.RunE(scrubRequestCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "no grant") {
		t.Fatalf("expected the server rejection to surface, got %v", err)
	}
}

func TestScrubCmd_ParentIsRunnable(t *testing.T) {
	// A non-runnable parent short-circuits cobra's ValidateArgs on an
	// unknown subcommand, printing help and exiting 0 (ka-0000005587 /
	// ka-0000003820 shape). scrubCmd must have a RunE so unknown
	// subcommands are rejected instead.
	if scrubCmd.RunE == nil {
		t.Error("scrubCmd must have a RunE so cobra.NoArgs rejects unknown subcommands")
	}
}

// testArtifact is a stand-in gd-FJUw7nG1DV audit artifact for the
// --artifact CLI tests below.
func testArtifact() *model.ScrubAuditArtifact {
	return &model.ScrubAuditArtifact{
		RequestID:    "gd-scrub-9",
		Taint:        "acme-production",
		Kind:         "detaint",
		Approver:     "operator@example.com",
		ApprovalMode: model.ScrubModeAuto,
		DetaintMode:  model.DetaintModeFork,
		Beads: []model.ScrubAuditArtifactBead{
			{ID: "gd-a", ContentSHA: "sha-a", TablesTouched: []string{"beads", "taint_audit"}, SuccessorBeadID: "gd-a-clean"},
		},
	}
}

// TestScrubApprove_ArtifactFlagWritesFileNotStdout pins acceptance
// criterion (h): --artifact <path> writes the exact JSON body to that file
// and nothing else to stdout.
func TestScrubApprove_ArtifactFlagWritesFileNotStdout(t *testing.T) {
	mc := newMockClient()
	mc.ApproveScrubRequestArtifact = testArtifact()
	withMockClient(t, mc)
	resetScrubApproveFlags(t)
	t.Cleanup(func() { resetScrubApproveFlags(t) })

	artifactPath := filepath.Join(t.TempDir(), "artifact.json")
	if err := scrubApproveCmd.Flags().Set("artifact", artifactPath); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		if err := scrubApproveCmd.RunE(scrubApproveCmd, []string{"gd-scrub-9"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if out != "" {
		t.Errorf("expected nothing on stdout when --artifact is set, got:\n%s", out)
	}

	data, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatalf("reading artifact file: %v", err)
	}
	var got model.ScrubAuditArtifact
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decoding artifact file: %v", err)
	}
	want := testArtifact()
	if got.RequestID != want.RequestID || got.Kind != want.Kind || got.DetaintMode != want.DetaintMode {
		t.Errorf("artifact file = %+v, want %+v", got, want)
	}
	if len(got.Beads) != 1 || got.Beads[0].SuccessorBeadID != "gd-a-clean" {
		t.Errorf("artifact file beads = %+v, want successor_bead_id=gd-a-clean", got.Beads)
	}
}

// TestScrubApprove_NoArtifactFlagPrintsToStdout pins acceptance criterion
// (h)'s other half: omitting --artifact prints the same JSON to stdout and
// writes no file.
func TestScrubApprove_NoArtifactFlagPrintsToStdout(t *testing.T) {
	mc := newMockClient()
	mc.ApproveScrubRequestArtifact = testArtifact()
	withMockClient(t, mc)
	resetScrubApproveFlags(t)
	t.Cleanup(func() { resetScrubApproveFlags(t) })

	out := captureStdout(t, func() {
		if err := scrubApproveCmd.RunE(scrubApproveCmd, []string{"gd-scrub-9"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	var got model.ScrubAuditArtifact
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decoding stdout as the artifact JSON: %v\nstdout:\n%s", err, out)
	}
	if got.RequestID != "gd-scrub-9" || got.DetaintMode != model.DetaintModeFork {
		t.Errorf("stdout artifact = %+v, want request_id=gd-scrub-9 detaint_mode=fork", got)
	}
	if len(got.Beads) != 1 || got.Beads[0].SuccessorBeadID != "gd-a-clean" {
		t.Errorf("stdout artifact beads = %+v, want successor_bead_id=gd-a-clean", got.Beads)
	}
}

// TestScrubApprove_ArtifactFlagErrorsWhenResponseHasNone covers the
// idempotent-retry edge: if the server returns no artifact (e.g. the
// scrub-request was already executed by a prior call), --artifact must
// error rather than silently write an empty/missing file.
func TestScrubApprove_ArtifactFlagErrorsWhenResponseHasNone(t *testing.T) {
	mc := newMockClient()
	mc.ApproveScrubRequestArtifact = nil
	withMockClient(t, mc)
	resetScrubApproveFlags(t)
	t.Cleanup(func() { resetScrubApproveFlags(t) })

	artifactPath := filepath.Join(t.TempDir(), "artifact.json")
	if err := scrubApproveCmd.Flags().Set("artifact", artifactPath); err != nil {
		t.Fatal(err)
	}

	err := scrubApproveCmd.RunE(scrubApproveCmd, []string{"gd-scrub-9"})
	if err == nil {
		t.Fatal("expected an error when the response carries no artifact")
	}
	if _, statErr := os.Stat(artifactPath); statErr == nil {
		t.Error("expected no artifact file to be written on error")
	}
}
