package main

import (
	"context"
	"errors"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// stubResolver is a minimal wrapupMRResolver fake. Returning (mr, nil) means
// hit; (nil, nil) means miss-absent; (nil, err) means hard error. Records
// every URL it was called with so tests can pin call order + count.
type stubResolver struct {
	lookup map[string]*beadsapi.BeadDetail
	err    error
	calls  []string
}

func (s *stubResolver) MRBeadByURL(_ context.Context, mrURL string) (*beadsapi.BeadDetail, error) {
	s.calls = append(s.calls, mrURL)
	if s.err != nil {
		return nil, s.err
	}
	if s.lookup == nil {
		return nil, nil
	}
	return s.lookup[mrURL], nil
}

// TestResolveWrapupMRs_HitPath: wrapup carries a URL that resolves to a
// merge-request bead. The pill must carry mr_iid / mr_title / mr_state /
// mr_pipeline_status from the bead's Fields map.
func TestResolveWrapupMRs_HitPath(t *testing.T) {
	url := "https://gitlab.com/acme/x/-/merge_requests/2310"
	stub := &stubResolver{
		lookup: map[string]*beadsapi.BeadDetail{
			url: {
				ID:    "gd-MR0001",
				Type:  "merge-request",
				Title: "fallback bead title",
				Fields: map[string]string{
					"mr_url":             url,
					"mr_iid":             "2310",
					"mr_title":           "feat(viewer): MR pill partial",
					"mr_state":           "opened",
					"mr_pipeline_status": "running",
				},
			},
		},
	}
	wrapup := `{"accomplishments":"done","pull_requests":["` + url + `"]}`
	got := ResolveWrapupMRs(context.Background(), stub, wrapup)
	if len(got) != 1 {
		t.Fatalf("want 1 pill, got %d", len(got))
	}
	v := got[0]
	if v.URL != url {
		t.Errorf("URL: want %q, got %q", url, v.URL)
	}
	if v.IID != "2310" {
		t.Errorf("IID: want %q, got %q", "2310", v.IID)
	}
	if v.Title != "feat(viewer): MR pill partial" {
		t.Errorf("Title: want %q, got %q", "feat(viewer): MR pill partial", v.Title)
	}
	if v.State != "opened" {
		t.Errorf("State: want %q, got %q", "opened", v.State)
	}
	if v.PipelineStatus != "running" {
		t.Errorf("PipelineStatus: want %q, got %q", "running", v.PipelineStatus)
	}
	if len(stub.calls) != 1 || stub.calls[0] != url {
		t.Errorf("daemon.MRBeadByURL call log: want [%q], got %v", url, stub.calls)
	}
}

// TestResolveWrapupMRs_MissAbsent: wrapup URL has no matching MR bead. The
// pill must still appear with State="unknown" so the operator sees a deep-link.
func TestResolveWrapupMRs_MissAbsent(t *testing.T) {
	url := "https://gitlab.com/x/-/merge_requests/9999"
	stub := &stubResolver{lookup: map[string]*beadsapi.BeadDetail{}}
	wrapup := `{"pull_requests":["` + url + `"]}`
	got := ResolveWrapupMRs(context.Background(), stub, wrapup)
	if len(got) != 1 {
		t.Fatalf("want 1 degraded pill, got %d: %+v", len(got), got)
	}
	if got[0].URL != url {
		t.Errorf("URL: want %q, got %q", url, got[0].URL)
	}
	if got[0].State != "unknown" {
		t.Errorf("State: want %q, got %q", "unknown", got[0].State)
	}
	if got[0].IID != "" || got[0].Title != "" || got[0].PipelineStatus != "" {
		t.Errorf("degraded pill should have empty IID/Title/PipelineStatus; got %+v", got[0])
	}
}

// TestResolveWrapupMRs_DaemonError: daemon returns an error per-URL. The
// resolver must swallow it and degrade to "unknown" rather than propagating
// nil — one bad bead must not blank the whole /agents row.
func TestResolveWrapupMRs_DaemonError(t *testing.T) {
	url := "https://gitlab.com/x/-/merge_requests/1"
	stub := &stubResolver{err: errors.New("boom")}
	wrapup := `{"pull_requests":["` + url + `"]}`
	got := ResolveWrapupMRs(context.Background(), stub, wrapup)
	if len(got) != 1 {
		t.Fatalf("want 1 degraded pill on daemon error, got %d", len(got))
	}
	if got[0].State != "unknown" {
		t.Errorf("State: want %q, got %q", "unknown", got[0].State)
	}
}

// TestResolveWrapupMRs_EmptyAndMalformed: every non-resolvable input must
// return nil — never a partial / panic.
func TestResolveWrapupMRs_EmptyAndMalformed(t *testing.T) {
	stub := &stubResolver{}
	cases := []struct {
		name   string
		wrapup string
	}{
		{"empty", ""},
		{"whitespace", "   \n  "},
		{"malformed_json", "{not json"},
		{"no_pull_requests_field", `{"accomplishments":"done"}`},
		{"empty_pull_requests", `{"pull_requests":[]}`},
		{"only_blank_urls", `{"pull_requests":["", "  "]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveWrapupMRs(context.Background(), stub, tc.wrapup)
			if got != nil {
				t.Errorf("want nil, got %+v", got)
			}
		})
	}
}

// TestResolveWrapupMRs_MultipleURLs: mix hit + miss in one wrapup. Order is
// preserved and the daemon is called once per non-blank URL.
func TestResolveWrapupMRs_MultipleURLs(t *testing.T) {
	hitURL := "https://gitlab.com/x/-/merge_requests/1"
	missURL := "https://gitlab.com/x/-/merge_requests/2"
	stub := &stubResolver{
		lookup: map[string]*beadsapi.BeadDetail{
			hitURL: {
				Type: "merge-request",
				Fields: map[string]string{
					"mr_url":   hitURL,
					"mr_iid":   "1",
					"mr_state": "merged",
				},
			},
		},
	}
	wrapup := `{"pull_requests":["` + hitURL + `","` + missURL + `"]}`
	got := ResolveWrapupMRs(context.Background(), stub, wrapup)
	if len(got) != 2 {
		t.Fatalf("want 2 pills, got %d", len(got))
	}
	if got[0].URL != hitURL || got[0].State != "merged" {
		t.Errorf("pill[0]: want hit on %q with state=merged, got %+v", hitURL, got[0])
	}
	if got[1].URL != missURL || got[1].State != "unknown" {
		t.Errorf("pill[1]: want miss on %q with state=unknown, got %+v", missURL, got[1])
	}
	if len(stub.calls) != 2 {
		t.Errorf("want 2 daemon calls, got %d: %v", len(stub.calls), stub.calls)
	}
}

// TestResolveWrapupMRs_NilDaemon: nil resolver must not panic and must
// degrade every URL to "unknown".
func TestResolveWrapupMRs_NilDaemon(t *testing.T) {
	url := "https://gitlab.com/x/-/merge_requests/1"
	wrapup := `{"pull_requests":["` + url + `"]}`
	got := ResolveWrapupMRs(context.Background(), nil, wrapup)
	if len(got) != 1 {
		t.Fatalf("want 1 degraded pill, got %d", len(got))
	}
	if got[0].URL != url || got[0].State != "unknown" {
		t.Errorf("nil daemon: want degraded {URL,State=unknown}; got %+v", got[0])
	}
}

// TestWrapUpMRViewFromBead_FallbackChains pins the per-field fallbacks the
// projector relies on when the merge-request bead has partial data.
func TestWrapUpMRViewFromBead_FallbackChains(t *testing.T) {
	url := "https://gitlab.com/x/-/merge_requests/7"

	t.Run("title_falls_back_to_bead_title", func(t *testing.T) {
		v := wrapUpMRViewFromBead(url, &beadsapi.BeadDetail{
			Title:  "bead title fallback",
			Fields: map[string]string{"mr_state": "opened"},
		})
		if v.Title != "bead title fallback" {
			t.Errorf("want bead.Title fallback, got %q", v.Title)
		}
	})

	t.Run("state_defaults_to_unknown", func(t *testing.T) {
		v := wrapUpMRViewFromBead(url, &beadsapi.BeadDetail{Fields: map[string]string{}})
		if v.State != "unknown" {
			t.Errorf("want unknown default state, got %q", v.State)
		}
	})

	t.Run("pipeline_status_falls_back_to_pipeline_status_field", func(t *testing.T) {
		v := wrapUpMRViewFromBead(url, &beadsapi.BeadDetail{
			Fields: map[string]string{
				"mr_state":        "opened",
				"pipeline_status": "success",
			},
		})
		if v.PipelineStatus != "success" {
			t.Errorf("want fallback pipeline_status=success, got %q", v.PipelineStatus)
		}
	})

	t.Run("mr_pipeline_status_wins_over_pipeline_status", func(t *testing.T) {
		v := wrapUpMRViewFromBead(url, &beadsapi.BeadDetail{
			Fields: map[string]string{
				"mr_state":           "opened",
				"mr_pipeline_status": "failed",
				"pipeline_status":    "success",
			},
		})
		if v.PipelineStatus != "failed" {
			t.Errorf("want mr_pipeline_status=failed wins, got %q", v.PipelineStatus)
		}
	})

	t.Run("mr_url_field_overrides_url_arg", func(t *testing.T) {
		other := "https://gitlab.com/x/-/merge_requests/8"
		v := wrapUpMRViewFromBead(url, &beadsapi.BeadDetail{
			Fields: map[string]string{
				"mr_url":   other,
				"mr_state": "opened",
			},
		})
		if v.URL != other {
			t.Errorf("want mr_url field to override URL arg; got %q", v.URL)
		}
	})
}
