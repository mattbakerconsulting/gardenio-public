package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

type fakeBeads struct {
	mrs       map[string]*beadsapi.BeadDetail // by mr_url
	tasks     map[string][]*beadsapi.BeadDetail
	updates   map[string]map[string]string // bead id -> fields written
	created   []beadsapi.CreateBeadRequest
	closed    []string
	lookupErr error
	updateErr error
}

func newFakeBeads() *fakeBeads {
	return &fakeBeads{
		mrs:     map[string]*beadsapi.BeadDetail{},
		tasks:   map[string][]*beadsapi.BeadDetail{},
		updates: map[string]map[string]string{},
	}
}

func (f *fakeBeads) addMR(url, id, agent string, fields map[string]string) {
	if fields == nil {
		fields = map[string]string{}
	}
	f.mrs[url] = &beadsapi.BeadDetail{ID: id, Type: "merge-request", Fields: fields}
	if agent != "" {
		f.tasks[id] = []*beadsapi.BeadDetail{{ID: "gd-task", Assignee: agent}}
	}
}

func (f *fakeBeads) MRBeadByURL(_ context.Context, u string) (*beadsapi.BeadDetail, error) {
	return f.mrs[u], f.lookupErr
}

func (f *fakeBeads) UpdateBeadFields(_ context.Context, id string, fields map[string]string) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	f.updates[id] = fields
	for _, b := range f.mrs {
		if b.ID == id {
			for k, v := range fields {
				b.Fields[k] = v
			}
		}
	}
	return nil
}

func (f *fakeBeads) ProducingTaskBeadsForMR(_ context.Context, id string) ([]*beadsapi.BeadDetail, error) {
	return f.tasks[id], nil
}

func (f *fakeBeads) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	f.created = append(f.created, req)
	return fmt.Sprintf("gd-msg%d", len(f.created)), nil
}

func (f *fakeBeads) CloseBead(_ context.Context, id string, _ map[string]string) error {
	f.closed = append(f.closed, id)
	return nil
}

const sha = "9fceb02d0ae598e95dc970b74767f19372d61af8"

func checkSuiteBody(conclusion string, prs ...int) []byte {
	var pr []string
	for _, n := range prs {
		pr = append(pr, fmt.Sprintf(`{"number":%d,"url":"https://api.github.com/repos/octo/repo/pulls/%d"}`, n, n))
	}
	concl := "null"
	if conclusion != "" {
		concl = `"` + conclusion + `"`
	}
	return []byte(fmt.Sprintf(`{"action":"completed","check_suite":{"id":5,"head_sha":%q,"conclusion":%s,"app":{"name":"GitHub Actions"},"pull_requests":[%s]},"repository":{"full_name":"octo/repo"}}`,
		sha, concl, strings.Join(pr, ",")))
}

func workflowRunBody(conclusion string, prs ...int) []byte {
	var pr []string
	for _, n := range prs {
		pr = append(pr, fmt.Sprintf(`{"number":%d}`, n))
	}
	return []byte(fmt.Sprintf(`{"action":"completed","workflow_run":{"id":7,"name":"CI","head_sha":%q,"conclusion":%q,"html_url":"https://github.com/octo/repo/actions/runs/7","pull_requests":[%s]},"repository":{"full_name":"octo/repo"}}`,
		sha, conclusion, strings.Join(pr, ",")))
}

func evOf(typ string, body []byte) Event {
	ev := Event{DeliveryID: "d1", Type: typ, Payload: body}
	fillEnvelope(&ev)
	return ev
}

const prURL = "https://github.com/octo/repo/pull/42"

func TestPipelineStatusFor(t *testing.T) {
	for in, want := range map[string]string{
		"success": "success", "neutral": "success",
		"failure": "failed", "timed_out": "failed", "startup_failure": "failed",
		"cancelled": "canceled", "stale": "canceled",
		"skipped": "skipped", "action_required": "manual", "SUCCESS": "success",
	} {
		if got, ok := pipelineStatusFor(in); !ok || got != want {
			t.Errorf("pipelineStatusFor(%q) = %q,%v want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "null", "mystery"} {
		if _, ok := pipelineStatusFor(in); ok {
			t.Errorf("pipelineStatusFor(%q) ok, want ignored", in)
		}
	}
}

func TestPipeline_WorkflowRunMapsConclusions(t *testing.T) {
	cases := []struct {
		conclusion, status, lineHas string
	}{
		{"success", "success", "✅"},
		{"failure", "failed", "❌"},
		{"cancelled", "canceled", "🚫"},
	}
	for _, c := range cases {
		t.Run(c.conclusion, func(t *testing.T) {
			fb := newFakeBeads()
			fb.addMR(prURL, "gd-mr1", "crew-a", nil)
			h := newPipelineHandler(fb, quiet())
			if err := h.Handle(context.Background(), evOf("workflow_run", workflowRunBody(c.conclusion, 42))); err != nil {
				t.Fatal(err)
			}
			got := fb.updates["gd-mr1"]
			if got["mr_pipeline_status"] != c.status || got["mr_pipeline_url"] != "https://github.com/octo/repo/actions/runs/7" {
				t.Fatalf("fields = %v", got)
			}
			if len(fb.created) != 1 || len(fb.closed) != 1 {
				t.Fatalf("thread line beads created=%d closed=%d, want 1/1", len(fb.created), len(fb.closed))
			}
			req := fb.created[0]
			var f map[string]string
			if err := json.Unmarshal(req.Fields, &f); err != nil {
				t.Fatal(err)
			}
			if f["source_agent"] != "crew-a" || req.Type != "message" || req.Labels[0] != "squawk" {
				t.Errorf("thread bead shape = %+v %v", req, f)
			}
			if !strings.Contains(f["text"], c.lineHas) || !strings.Contains(f["text"], "PR #42") ||
				!strings.Contains(f["text"], "<https://github.com/octo/repo/actions/runs/7|CI>") || !strings.Contains(f["text"], "`9fceb02`") {
				t.Errorf("line = %q", f["text"])
			}
		})
	}
}

func TestPipeline_CheckSuiteFailureUsesCommitChecksURL(t *testing.T) {
	fb := newFakeBeads()
	fb.addMR(prURL, "gd-mr1", "crew-a", nil)
	h := newPipelineHandler(fb, quiet())
	if err := h.Handle(context.Background(), evOf("check_suite", checkSuiteBody("failure", 42))); err != nil {
		t.Fatal(err)
	}
	got := fb.updates["gd-mr1"]
	if got["mr_pipeline_status"] != "failed" || got["mr_pipeline_url"] != "https://github.com/octo/repo/commit/"+sha+"/checks" {
		t.Fatalf("fields = %v", got)
	}
	if !strings.Contains(string(fb.created[0].Fields), "GitHub Actions") {
		t.Errorf("line should name the check app: %s", fb.created[0].Fields)
	}
}

func TestPipeline_NoMatchingBead(t *testing.T) {
	fb := newFakeBeads()
	h := newPipelineHandler(fb, quiet())
	for _, ev := range []Event{
		evOf("workflow_run", workflowRunBody("success", 42)),
		evOf("check_suite", checkSuiteBody("failure", 42)),
	} {
		if err := h.Handle(context.Background(), ev); err != nil {
			t.Fatalf("no bead must not error (would trigger redelivery): %v", err)
		}
	}
	if len(fb.updates)+len(fb.created) != 0 {
		t.Errorf("wrote something for unknown PR: %v %v", fb.updates, fb.created)
	}
}

func TestPipeline_IgnoredEvents(t *testing.T) {
	fb := newFakeBeads()
	fb.addMR(prURL, "gd-mr1", "crew-a", nil)
	h := newPipelineHandler(fb, quiet())
	for name, ev := range map[string]Event{
		"requested action": func() Event {
			e := evOf("workflow_run", workflowRunBody("success", 42))
			e.Action = "requested"
			return e
		}(),
		"other event type":   evOf("pull_request", workflowRunBody("success", 42)),
		"no conclusion":      evOf("check_suite", checkSuiteBody("", 42)),
		"no pull requests":   evOf("workflow_run", workflowRunBody("success")),
		"undecodable":        {Type: "workflow_run", Action: "completed", Payload: []byte("{not json")},
		"no repo in payload": {Type: "workflow_run", Action: "completed", Payload: []byte(`{"workflow_run":{"conclusion":"success","pull_requests":[{"number":42}]}}`)},
	} {
		if err := h.Handle(context.Background(), ev); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(fb.updates)+len(fb.created) != 0 {
		t.Errorf("ignored events wrote: %v %v", fb.updates, fb.created)
	}
}

func TestPipeline_RepeatAndURLOnlyChangeStaySilent(t *testing.T) {
	fb := newFakeBeads()
	fb.addMR(prURL, "gd-mr1", "crew-a", nil)
	h := newPipelineHandler(fb, quiet())
	ctx := context.Background()
	// workflow_run then check_suite for the same commit: same verdict, new URL.
	_ = h.Handle(ctx, evOf("workflow_run", workflowRunBody("success", 42)))
	_ = h.Handle(ctx, evOf("check_suite", checkSuiteBody("success", 42)))
	_ = h.Handle(ctx, evOf("check_suite", checkSuiteBody("success", 42)))
	if len(fb.created) != 1 {
		t.Fatalf("thread lines = %d, want 1", len(fb.created))
	}
	if fb.mrs[prURL].Fields["mr_pipeline_url"] != "https://github.com/octo/repo/commit/"+sha+"/checks" {
		t.Errorf("url not refreshed: %v", fb.mrs[prURL].Fields)
	}
	// A verdict flip announces again.
	_ = h.Handle(ctx, evOf("workflow_run", workflowRunBody("failure", 42)))
	if len(fb.created) != 2 {
		t.Fatalf("thread lines after flip = %d, want 2", len(fb.created))
	}
}

func TestPipeline_MultiplePRsAndNoAgent(t *testing.T) {
	fb := newFakeBeads()
	fb.addMR(prURL, "gd-mr1", "", nil) // no producing task: fields yes, thread line no
	fb.addMR("https://github.com/octo/repo/pull/43", "gd-mr2", "crew-b", nil)
	h := newPipelineHandler(fb, quiet())
	if err := h.Handle(context.Background(), evOf("workflow_run", workflowRunBody("success", 42, 43, 44))); err != nil {
		t.Fatal(err)
	}
	if fb.updates["gd-mr1"]["mr_pipeline_status"] != "success" || fb.updates["gd-mr2"]["mr_pipeline_status"] != "success" {
		t.Errorf("updates = %v", fb.updates)
	}
	if len(fb.created) != 1 || !strings.Contains(string(fb.created[0].Fields), "crew-b") {
		t.Errorf("thread lines = %v", fb.created)
	}
}

func TestPipeline_DaemonErrorsRequestRedelivery(t *testing.T) {
	fb := newFakeBeads()
	fb.addMR(prURL, "gd-mr1", "crew-a", nil)
	h := newPipelineHandler(fb, quiet())
	fb.lookupErr = errors.New("daemon down")
	if err := h.Handle(context.Background(), evOf("workflow_run", workflowRunBody("success", 42))); err == nil {
		t.Error("lookup failure must surface so GitHub redelivers")
	}
	fb.lookupErr, fb.updateErr = nil, errors.New("write failed")
	if err := h.Handle(context.Background(), evOf("workflow_run", workflowRunBody("success", 42))); err == nil {
		t.Error("update failure must surface so GitHub redelivers")
	}
	if len(fb.created) != 0 {
		t.Error("announced a verdict that was not recorded")
	}
}

func TestFanoutSink(t *testing.T) {
	a, b := &recSink{}, &recSink{fail: errors.New("boom")}
	c := &recSink{}
	if err := (fanoutSink{a, b, c}).Handle(context.Background(), Event{}); err == nil {
		t.Fatal("want error from b")
	}
	if a.count() != 1 || c.count() != 0 {
		t.Errorf("a=%d c=%d, want 1/0 (b failed before c)", a.count(), c.count())
	}
	if err := (fanoutSink{a, c}).Handle(context.Background(), Event{}); err != nil || a.count() != 2 || c.count() != 1 {
		t.Errorf("fanout a=%d c=%d err=%v", a.count(), c.count(), err)
	}
}

// Deliveries go through the real receiver: signature, replay claim, dispatch.
func TestPipeline_ThroughWebhookHandler(t *testing.T) {
	fb := newFakeBeads()
	fb.addMR(prURL, "gd-mr1", "crew-a", nil)
	hn := newHarness(testSecret, 10)
	hn.sink = nil
	hn.h = newWebhookHandler(webhookConfig{
		Secret: testSecret, Sink: fanoutSink{newPipelineHandler(fb, quiet())},
		Replay: hn.replay, Logger: quiet(),
	})
	body := string(workflowRunBody("failure", 42))
	w := hn.post(body, sign(testSecret, []byte(body)), "delivery-1", "workflow_run")
	if w.Code != 202 {
		t.Fatalf("status = %d", w.Code)
	}
	if fb.updates["gd-mr1"]["mr_pipeline_status"] != "failed" {
		t.Errorf("updates = %v", fb.updates)
	}
}
