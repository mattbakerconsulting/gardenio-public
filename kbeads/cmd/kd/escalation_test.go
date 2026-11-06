package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

// newEscalationCmdForTest builds a STANDALONE copy of the escalation command
// tree that reuses the REAL RunE bodies + flag sets, parentless so cobra's
// Execute() does not re-root to rootCmd (which would swap the mocked
// beadsClient for a live client). Mirrors newCloseCmdForTest's rationale.
func newEscalationCmdForTest() *cobra.Command {
	root := &cobra.Command{Use: "escalation", SilenceUsage: true}

	list := &cobra.Command{Use: "list", Args: cobra.NoArgs, RunE: escalationListCmd.RunE, SilenceUsage: true}
	list.Flags().String("status", "", "")
	list.Flags().Int("limit", 0, "")
	list.Flags().StringSlice("operation-type", nil, "")
	list.Flags().StringSliceP("label", "l", nil, "")
	list.Flags().StringSlice("exclude-label", nil, "")

	show := &cobra.Command{Use: "show <id>", Args: cobra.ExactArgs(1), RunE: escalationShowCmd.RunE, SilenceUsage: true}

	approve := &cobra.Command{Use: "approve <id>", Args: cobra.ExactArgs(1), RunE: escalationApproveCmd.RunE, SilenceUsage: true}
	approve.Flags().String("as", "", "")

	reject := &cobra.Command{Use: "reject <id>", Args: cobra.ExactArgs(1), RunE: escalationRejectCmd.RunE, SilenceUsage: true}
	reject.Flags().String("as", "", "")
	reject.Flags().String("reason", "", "")

	resolve := &cobra.Command{Use: "resolve <id>", Args: cobra.ExactArgs(1), RunE: escalationResolveCmd.RunE, SilenceUsage: true}
	resolve.Flags().String("status", "", "")
	resolve.Flags().String("result", "", "")
	resolve.Flags().String("executor", "", "")

	claim := &cobra.Command{Use: "claim <id>", Args: cobra.ExactArgs(1), RunE: escalationClaimCmd.RunE, SilenceUsage: true}
	handUp := &cobra.Command{Use: "hand-up <id>", Args: cobra.ExactArgs(1), RunE: escalationHandUpCmd.RunE, SilenceUsage: true}
	handUp.Flags().String("reason", "", "")

	root.AddCommand(list, show, approve, reject, resolve, claim, handUp)
	return root
}

// execEscalation runs the standalone escalation command tree with the given
// args and returns the execute error. Cobra output (usage/errors) is captured;
// command stdout (fmt.Printf / emitJSON) is NOT — wrap in captureStdout for that.
func execEscalation(t *testing.T, args ...string) error {
	t.Helper()
	c := newEscalationCmdForTest()
	var buf bytes.Buffer
	c.SetOut(&buf)
	c.SetErr(&buf)
	c.SetArgs(args)
	return c.Execute()
}

func escDetail(id, title string, pri int, status model.Status, createdBy string, createdAt time.Time) client.EscalationDetail {
	return client.EscalationDetail{
		Escalation: map[string]any{"operation_type": "aws_iam"},
		Issue: &model.Bead{
			ID:        id,
			Title:     title,
			Priority:  pri,
			Status:    status,
			Type:      model.TypeEscalation,
			CreatedBy: createdBy,
			CreatedAt: createdAt,
		},
	}
}

// TestEscalationList_JSONShape locks the acceptance-criteria JSON contract:
// `kd escalation list --json` emits a bare array of objects carrying exactly
// id, title, priority, status, created_by, created_at, age_minutes.
func TestEscalationList_JSONShape(t *testing.T) {
	mc := newMockClient()
	created := time.Now().Add(-90 * time.Minute)
	mc.EscalationList = []client.EscalationDetail{
		escDetail("gd-egqpg0fc45", "rotate prod key", 1, model.StatusOpen, "agent-x", created),
	}
	withMockClient(t, mc)
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = false })

	out := captureStdout(t, func() {
		if err := execEscalation(t, "list"); err != nil {
			t.Fatalf("escalation list errored: %v", err)
		}
	})

	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("output is not a bare JSON array: %v\n%s", err, out)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d: %s", len(rows), out)
	}
	row := rows[0]
	for _, k := range []string{"id", "title", "priority", "status", "created_by", "created_at", "age_minutes"} {
		if _, ok := row[k]; !ok {
			t.Errorf("row missing required key %q: %v", k, row)
		}
	}
	if row["id"] != "gd-egqpg0fc45" {
		t.Errorf("id = %v, want gd-egqpg0fc45", row["id"])
	}
	if row["status"] != "open" {
		t.Errorf("status = %v, want open", row["status"])
	}
	// age_minutes should be ~90 (allow drift for test runtime).
	age, ok := row["age_minutes"].(float64)
	if !ok || age < 89 || age > 95 {
		t.Errorf("age_minutes = %v, want ~90", row["age_minutes"])
	}
}

// mergeEscDetail is a merge escalation as the daemon lists it: labels and
// Fields on the issue bead, and the extracted `escalation` projection.
func mergeEscDetail(id, opType string, labels []string, createdAt time.Time) client.EscalationDetail {
	fields := map[string]any{
		"operation_type":   opType,
		"operation_spec":   map[string]any{"mr_url": "https://github.com/acme/app/pull/7", "repo": "acme/app"},
		"requesting_task":  "gd-task0000001",
		"requesting_agent": "gardenio-crew-a",
		"held_by":          "coordinator:merge-desk",
		"context":          "ready for review",
	}
	raw, _ := json.Marshal(fields)
	return client.EscalationDetail{
		Escalation: map[string]any{"operation_type": opType},
		Issue: &model.Bead{
			ID:        id,
			Title:     "Merge PR #7",
			Priority:  0,
			Status:    model.StatusOpen,
			Type:      model.TypeEscalation,
			CreatedBy: "gardenio-crew-a",
			CreatedAt: createdAt,
			Labels:    labels,
			Fields:    raw,
		},
	}
}

// TestEscalationList_JSONRowGolden locks the full row shape against
// testdata/escalation_list_rows.golden.json (gd-CndLtkZ5h0): labels and the
// escalation fields the merge desk filters on ride on every row. Regenerate
// with UPDATE_GOLDEN=1 go test ./cmd/kd -run TestEscalationList_JSONRowGolden.
func TestEscalationList_JSONRowGolden(t *testing.T) {
	mc := newMockClient()
	created := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	mc.EscalationList = []client.EscalationDetail{
		mergeEscDetail("gd-escmerge001", "github_pr_merge", []string{"project:gardenio", "merge-desk:seen"}, created),
		escDetail("gd-escbare0001", "rotate prod key", 1, model.StatusInProgress, "agent-x", created),
	}
	withMockClient(t, mc)
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = false })

	out := captureStdout(t, func() {
		if err := execEscalation(t, "list"); err != nil {
			t.Fatalf("escalation list errored: %v", err)
		}
	})
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("output is not a bare JSON array: %v\n%s", err, out)
	}
	for _, r := range rows {
		r["age_minutes"] = 0 // depends on the wall clock
	}
	got, _ := json.MarshalIndent(rows, "", "  ")
	got = append(got, '\n')

	golden := filepath.Join("testdata", "escalation_list_rows.golden.json")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (UPDATE_GOLDEN=1 to create): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("row shape drifted from %s (UPDATE_GOLDEN=1 to re-bless)\ngot:\n%s\nwant:\n%s", golden, got, want)
	}
}

// TestEscalationList_FiltersForwarded sends --operation-type, --label and
// --exclude-label to the daemon.
func TestEscalationList_FiltersForwarded(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	if err := execEscalation(t, "list", "--operation-type", "github_pr_merge,gitlab_mr_merge",
		"-l", "merge-desk:draft", "--exclude-label", "merge-desk:close-denied"); err != nil {
		t.Fatalf("escalation list errored: %v", err)
	}
	got := mc.ListEscalationCalls[0]
	if !slices.Equal(got.OperationTypes, []string{"github_pr_merge", "gitlab_mr_merge"}) ||
		!slices.Equal(got.Labels, []string{"merge-desk:draft"}) ||
		!slices.Equal(got.ExcludeLabels, []string{"merge-desk:close-denied"}) {
		t.Errorf("forwarded options = %+v", got)
	}
}

// TestEscalationList_FiltersAppliedClientSide keeps the result right against a
// daemon that predates the filters and returns every escalation.
func TestEscalationList_FiltersAppliedClientSide(t *testing.T) {
	mc := newMockClient()
	now := time.Now()
	mc.EscalationList = []client.EscalationDetail{
		mergeEscDetail("gd-gh", "github_pr_merge", []string{"project:gardenio"}, now),
		mergeEscDetail("gd-gl", "gitlab_mr_merge", nil, now),
		mergeEscDetail("gd-seen", "github_pr_merge", []string{"merge-desk:seen"}, now),
		mergeEscDetail("gd-helm", "helm_deploy", nil, now),
	}
	withMockClient(t, mc)
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = false })

	out := captureStdout(t, func() {
		if err := execEscalation(t, "list", "--operation-type", "github_pr_merge,gitlab_mr_merge",
			"--exclude-label", "merge-desk:seen"); err != nil {
			t.Fatalf("escalation list errored: %v", err)
		}
	})
	var rows []escalationRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	var ids []string
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	if !slices.Equal(ids, []string{"gd-gh", "gd-gl"}) {
		t.Errorf("ids = %v, want [gd-gh gd-gl]", ids)
	}
	if rows[1].Labels == nil {
		t.Errorf("labels on an unlabelled row = nil, want [] so jq index never fails")
	}
}

// TestEscalationList_DefaultStatusPassthrough verifies the default (empty)
// status is forwarded verbatim so the DAEMON applies its open+in_progress
// default — kd does not second-guess it.
func TestEscalationList_DefaultStatusPassthrough(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	if err := execEscalation(t, "list"); err != nil {
		t.Fatalf("escalation list errored: %v", err)
	}
	if len(mc.ListEscalationCalls) != 1 {
		t.Fatalf("ListEscalations called %d times, want 1", len(mc.ListEscalationCalls))
	}
	if got := mc.ListEscalationCalls[0].Status; got != "" {
		t.Errorf("default status = %q, want empty (daemon applies open+in_progress)", got)
	}
}

// TestEscalationList_StatusFlag forwards --status to the client.
func TestEscalationList_StatusFlag(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	if err := execEscalation(t, "list", "--status", "closed"); err != nil {
		t.Fatalf("escalation list --status errored: %v", err)
	}
	if got := mc.ListEscalationCalls[0].Status; got != "closed" {
		t.Errorf("status = %q, want closed", got)
	}
}

// TestEscalationApprove_WiresRequest proves approve sends approved_by (from
// --as) with approval_method=cli to the right id.
func TestEscalationApprove_WiresRequest(t *testing.T) {
	mc := newMockClient()
	mc.EscalationsByID = map[string]*client.EscalationDetail{
		"gd-egqpg0fc45": ptrDetail(escDetail("gd-egqpg0fc45", "t", 0, model.StatusOpen, "a", time.Now())),
	}
	withMockClient(t, mc)

	out := captureStdout(t, func() {
		if err := execEscalation(t, "approve", "gd-egqpg0fc45", "--as", "matthew.baker"); err != nil {
			t.Fatalf("approve errored: %v", err)
		}
	})
	if len(mc.ApproveEscalationCalls) != 1 {
		t.Fatalf("ApproveEscalation called %d times, want 1", len(mc.ApproveEscalationCalls))
	}
	got := mc.ApproveEscalationCalls[0]
	if got.ID != "gd-egqpg0fc45" {
		t.Errorf("approve id = %q, want gd-egqpg0fc45", got.ID)
	}
	if got.Req.ApprovedBy != "matthew.baker" {
		t.Errorf("approved_by = %q, want matthew.baker", got.Req.ApprovedBy)
	}
	if got.Req.ApprovalMethod != "cli" {
		t.Errorf("approval_method = %q, want cli", got.Req.ApprovalMethod)
	}
	if !strings.Contains(out, "Approved gd-egqpg0fc45") {
		t.Errorf("output missing confirmation: %s", out)
	}
}

// TestEscalationApprove_RefusesAlreadyResolved proves the pre-check blocks a
// resolve verb on an already-closed escalation before hitting the daemon.
func TestEscalationApprove_RefusesAlreadyResolved(t *testing.T) {
	mc := newMockClient()
	mc.EscalationsByID = map[string]*client.EscalationDetail{
		"gd-egqpg0fc45": ptrDetail(escDetail("gd-egqpg0fc45", "t", 0, model.StatusClosed, "a", time.Now())),
	}
	withMockClient(t, mc)

	err := execEscalation(t, "approve", "gd-egqpg0fc45", "--as", "op")
	if err == nil {
		t.Fatal("approve of a closed escalation returned nil; want a refusal")
	}
	if !strings.Contains(err.Error(), "already closed") {
		t.Errorf("error = %q, want it to mention 'already closed'", err.Error())
	}
	if len(mc.ApproveEscalationCalls) != 0 {
		t.Errorf("ApproveEscalation should not be called for a closed escalation; got %d", len(mc.ApproveEscalationCalls))
	}
}

// TestEscalationReject_WiresRequest proves reject sends rejected_by + reason.
func TestEscalationReject_WiresRequest(t *testing.T) {
	mc := newMockClient()
	mc.EscalationsByID = map[string]*client.EscalationDetail{
		"gd-egqpg0fc45": ptrDetail(escDetail("gd-egqpg0fc45", "t", 0, model.StatusOpen, "a", time.Now())),
	}
	withMockClient(t, mc)

	_ = captureStdout(t, func() {
		if err := execEscalation(t, "reject", "gd-egqpg0fc45", "--as", "op", "--reason", "not needed"); err != nil {
			t.Fatalf("reject errored: %v", err)
		}
	})
	if len(mc.RejectEscalationCalls) != 1 {
		t.Fatalf("RejectEscalation called %d times, want 1", len(mc.RejectEscalationCalls))
	}
	got := mc.RejectEscalationCalls[0]
	if got.Req.RejectedBy != "op" || got.Req.Reason != "not needed" {
		t.Errorf("reject req = %+v, want rejected_by=op reason='not needed'", got.Req)
	}
}

// TestEscalationResolve_RequiresStatus proves resolve rejects a missing/invalid
// --status before any client call.
func TestEscalationResolve_RequiresStatus(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	if err := execEscalation(t, "resolve", "gd-egqpg0fc45"); err == nil {
		t.Fatal("resolve without --status returned nil; want a required-flag error")
	}
	if err := execEscalation(t, "resolve", "gd-egqpg0fc45", "--status", "bogus"); err == nil {
		t.Fatal("resolve with invalid --status returned nil; want a validation error")
	}
	if len(mc.CaptureResultCalls) != 0 {
		t.Errorf("CaptureEscalationResult should not be called on invalid status; got %d", len(mc.CaptureResultCalls))
	}
}

// TestEscalationResolve_WiresRequest proves resolve forwards status + parsed
// result JSON + executor.
func TestEscalationResolve_WiresRequest(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	_ = captureStdout(t, func() {
		if err := execEscalation(t, "resolve", "gd-egqpg0fc45",
			"--status", "success", "--result", `{"output":"done"}`, "--executor", "op"); err != nil {
			t.Fatalf("resolve errored: %v", err)
		}
	})
	if len(mc.CaptureResultCalls) != 1 {
		t.Fatalf("CaptureEscalationResult called %d times, want 1", len(mc.CaptureResultCalls))
	}
	got := mc.CaptureResultCalls[0].Req
	if got.ResultStatus != "success" {
		t.Errorf("result_status = %q, want success", got.ResultStatus)
	}
	if got.Executor != "op" {
		t.Errorf("executor = %q, want op", got.Executor)
	}
	m, ok := got.Result.(map[string]any)
	if !ok || m["output"] != "done" {
		t.Errorf("result = %v, want map with output=done", got.Result)
	}
}

// TestEscalationResolve_RejectsBadResultJSON proves an invalid --result JSON
// aborts before any client call.
func TestEscalationResolve_RejectsBadResultJSON(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	err := execEscalation(t, "resolve", "gd-egqpg0fc45", "--status", "success", "--result", "{not json")
	if err == nil {
		t.Fatal("resolve with bad --result JSON returned nil; want a parse error")
	}
	if len(mc.CaptureResultCalls) != 0 {
		t.Errorf("CaptureEscalationResult should not be called on bad JSON; got %d", len(mc.CaptureResultCalls))
	}
}

// TestEscalationApprove_403IsActionable proves the operator-only 403 becomes an
// actionable message pointing at gb decision, not a bare "HTTP 403". This is the
// by-design guard from the acceptance criteria: a crew agent cannot resolve.
func TestEscalationApprove_403IsActionable(t *testing.T) {
	mc := newMockClient()
	mc.EscalationsByID = map[string]*client.EscalationDetail{
		"gd-egqpg0fc45": ptrDetail(escDetail("gd-egqpg0fc45", "t", 0, model.StatusOpen, "a", time.Now())),
	}
	mc.ApproveEscalationErr = &client.APIError{StatusCode: 403, Message: "forbidden: requires operator role"}
	withMockClient(t, mc)

	err := execEscalation(t, "approve", "gd-egqpg0fc45", "--as", "agent-x")
	if err == nil {
		t.Fatal("approve with a 403 returned nil; want the error surfaced")
	}
	if !strings.Contains(err.Error(), "OPERATORS") || !strings.Contains(err.Error(), "gb decision create") {
		t.Errorf("403 error is not actionable; got: %q", err.Error())
	}
}

// TestEscalationApproveReject_403HandsThePageLink: kd approve and reject never
// approve for a person (P10.B gd-4mEAhIE01U); a refusal prints the
// escalation's page, approve_url under --json, and still fails.
func TestEscalationApproveReject_403HandsThePageLink(t *testing.T) {
	t.Setenv("BEADS_VIEWER_URL", "https://garden.example.com/")
	for _, verb := range []string{"approve", "reject"} {
		t.Run(verb, func(t *testing.T) {
			mc := newMockClient()
			mc.EscalationsByID = map[string]*client.EscalationDetail{
				"gd-egqpg0fc45": ptrDetail(escDetail("gd-egqpg0fc45", "t", 0, model.StatusOpen, "a", time.Now())),
			}
			refusal := &client.APIError{StatusCode: 403, Message: "escalation approval is not available to a `kd login` session"}
			mc.ApproveEscalationErr, mc.RejectEscalationErr = refusal, refusal
			withMockClient(t, mc)

			var err error
			out := captureStdout(t, func() { err = execEscalation(t, verb, "gd-egqpg0fc45", "--as", "owner") })
			if err == nil {
				t.Fatalf("%s with a 403 returned nil; nothing was approved, so it must fail", verb)
			}
			if !strings.Contains(out, "https://garden.example.com/gd-egqpg0fc45") {
				t.Errorf("%s did not print the page link; stdout: %q", verb, out)
			}

			jsonOutput = true
			t.Cleanup(func() { jsonOutput = false })
			out = captureStdout(t, func() { _ = execEscalation(t, verb, "gd-egqpg0fc45", "--as", "owner") })
			var v map[string]any
			if json.Unmarshal([]byte(out), &v) != nil || v["approve_url"] != "https://garden.example.com/gd-egqpg0fc45" {
				t.Errorf("--json %s = %q, want approve_url", verb, out)
			}
		})
	}
}

func TestEscalationPageURL_FallsBackToTheDaemonHost(t *testing.T) {
	t.Setenv("BEADS_VIEWER_URL", "")
	old := httpURL
	t.Cleanup(func() { httpURL = old })
	for in, want := range map[string]string{
		"https://gardenio.example.com/kd": "https://gardenio.example.com/gd-x",
		"http://localhost:8080":           "",
	} {
		httpURL = in
		if got := escalationPageURL("gd-x"); got != want {
			t.Errorf("httpURL %s: page = %q, want %q", in, got, want)
		}
	}
}

// TestEscalationShow_JSON emits the full detail (escalation projection + issue).
func TestEscalationShow_JSON(t *testing.T) {
	mc := newMockClient()
	mc.EscalationsByID = map[string]*client.EscalationDetail{
		"gd-egqpg0fc45": ptrDetail(escDetail("gd-egqpg0fc45", "rotate", 2, model.StatusOpen, "a", time.Now())),
	}
	withMockClient(t, mc)
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = false })

	out := captureStdout(t, func() {
		if err := execEscalation(t, "show", "gd-egqpg0fc45"); err != nil {
			t.Fatalf("show errored: %v", err)
		}
	})
	var d client.EscalationDetail
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		t.Fatalf("show --json output is not an EscalationDetail: %v\n%s", err, out)
	}
	if d.Issue == nil || d.Issue.ID != "gd-egqpg0fc45" {
		t.Errorf("show --json issue = %+v, want id gd-egqpg0fc45", d.Issue)
	}
}

func ptrDetail(d client.EscalationDetail) *client.EscalationDetail { return &d }

func TestEscalationClaim_WiresRequest(t *testing.T) {
	mc := newMockClient()
	mc.EscalationsByID = map[string]*client.EscalationDetail{
		"gd-egqpg0fc45": ptrDetail(escDetail("gd-egqpg0fc45", "t", 0, model.StatusOpen, "a", time.Now())),
	}
	withMockClient(t, mc)
	_ = captureStdout(t, func() {
		if err := execEscalation(t, "claim", "gd-egqpg0fc45"); err != nil {
			t.Fatalf("claim errored: %v", err)
		}
	})
	if len(mc.ClaimEscalationCalls) != 1 || mc.ClaimEscalationCalls[0] != "gd-egqpg0fc45" {
		t.Fatalf("ClaimEscalation calls = %v", mc.ClaimEscalationCalls)
	}
}

func TestEscalationHandUp_RequiresReason(t *testing.T) {
	mc := newMockClient()
	mc.EscalationsByID = map[string]*client.EscalationDetail{
		"gd-egqpg0fc45": ptrDetail(escDetail("gd-egqpg0fc45", "t", 0, model.StatusOpen, "a", time.Now())),
	}
	withMockClient(t, mc)
	if err := execEscalation(t, "hand-up", "gd-egqpg0fc45"); err == nil {
		t.Fatal("hand-up without --reason must fail")
	}
	if len(mc.HandUpEscalationCalls) != 0 {
		t.Fatal("hand-up without --reason reached the client")
	}
}

func TestEscalationHandUp_WiresRequest(t *testing.T) {
	mc := newMockClient()
	mc.EscalationsByID = map[string]*client.EscalationDetail{
		"gd-egqpg0fc45": ptrDetail(escDetail("gd-egqpg0fc45", "t", 0, model.StatusOpen, "a", time.Now())),
	}
	withMockClient(t, mc)
	_ = captureStdout(t, func() {
		if err := execEscalation(t, "hand-up", "gd-egqpg0fc45", "--reason", "needs owner"); err != nil {
			t.Fatalf("hand-up errored: %v", err)
		}
	})
	if len(mc.HandUpEscalationCalls) != 1 || mc.HandUpEscalationCalls[0].Req.Reason != "needs owner" {
		t.Fatalf("HandUpEscalation calls = %+v", mc.HandUpEscalationCalls)
	}
}
