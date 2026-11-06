package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
)

func TestParseConfig_BeadsViewerURL(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want string
	}{
		{"unset", "", ""},
		{"plain", "https://beads-viewer.example.com", "https://beads-viewer.example.com"},
		{"trailing slash trimmed", "https://beads-viewer.example.com/", "https://beads-viewer.example.com"},
		{"multiple trailing slashes trimmed", "https://beads-viewer.example.com///", "https://beads-viewer.example.com"},
		{"path prefix preserved", "https://viewer.example.com/beads/", "https://viewer.example.com/beads"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("BEADS_VIEWER_URL", tt.env)
			cfg := parseConfig()
			if cfg.beadsViewerURL != tt.want {
				t.Errorf("beadsViewerURL=%q, want %q", cfg.beadsViewerURL, tt.want)
			}
		})
	}
}

func TestParseRepoList(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []bridge.RepoRef
	}{
		{
			name:  "simple repos",
			input: "org/repo1,org/repo2",
			want: []bridge.RepoRef{
				{Owner: "org", Repo: "repo1"},
				{Owner: "org", Repo: "repo2"},
			},
		},
		{
			name:  "external deps with ~ext suffix",
			input: "org/repo1,org/repo2~ext",
			want: []bridge.RepoRef{
				{Owner: "org", Repo: "repo1"},
				{Owner: "org", Repo: "repo2", External: true},
			},
		},
		{
			name:  "default config",
			input: "joyousaversion/gardenio,joyousaversion/kbeads~ext,joyousaversion/coop~ext",
			want: []bridge.RepoRef{
				{Owner: "joyousaversion", Repo: "gardenio"},
				{Owner: "joyousaversion", Repo: "kbeads", External: true},
				{Owner: "joyousaversion", Repo: "coop", External: true},
			},
		},
		{
			name:  "spaces trimmed",
			input: " org/repo1 , org/repo2~ext ",
			want: []bridge.RepoRef{
				{Owner: "org", Repo: "repo1"},
				{Owner: "org", Repo: "repo2", External: true},
			},
		},
		{
			name:  "empty entries skipped",
			input: "org/repo1,,org/repo2",
			want: []bridge.RepoRef{
				{Owner: "org", Repo: "repo1"},
				{Owner: "org", Repo: "repo2"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseRepoList(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d repos, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i].Owner != tt.want[i].Owner {
					t.Errorf("[%d] Owner=%q, want %q", i, got[i].Owner, tt.want[i].Owner)
				}
				if got[i].Repo != tt.want[i].Repo {
					t.Errorf("[%d] Repo=%q, want %q", i, got[i].Repo, tt.want[i].Repo)
				}
				if got[i].External != tt.want[i].External {
					t.Errorf("[%d] External=%v, want %v", i, got[i].External, tt.want[i].External)
				}
			}
		})
	}
}

// TestSlackBridgeNoClientSystemOperatorDefault pins the human-operator
// invariant (epic ka-0000004835, task ka-0000006293): the bridge no longer
// configures a client-level X-System-Operator default. An operator-less
// service write must therefore carry NEITHER X-System-Operator NOR
// X-Operated-By — there is no silent sentinel fallback. The daemon-side
// resolver (ka-0000006425) is responsible for routing genuine machine-origin
// writes to the narrowly-scoped system:machine sink or refusing; the client
// never pre-binds a system identity.
func TestSlackBridgeNoClientSystemOperatorDefault(t *testing.T) {
	var gotSystem, gotOperator string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSystem = r.Header.Get("X-System-Operator")
		gotOperator = r.Header.Get("X-Operated-By")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	defer daemon.Close()

	// Mirror main()'s wiring: NO SetSystemOperator call. An operator-less
	// service call must leave both attribution headers unset.
	_, _ = daemon.ListBeadsFiltered(context.Background(), beadsapi.ListBeadsQuery{})

	if gotSystem != "" {
		t.Errorf("X-System-Operator = %q; want empty (no client-level sentinel default)", gotSystem)
	}
	if gotOperator != "" {
		t.Errorf("X-Operated-By = %q; want empty (no sentinel masquerading as human)", gotOperator)
	}
}

// TestSlackBridgeHumanOperatorStampedPerCall pins that a per-call human
// operator still flows through as X-Operated-By even with no client-level
// default — this is the path mention handlers and thread auto-close use to
// attribute service writes to the real agent/thread human (NOT a sentinel).
func TestSlackBridgeHumanOperatorStampedPerCall(t *testing.T) {
	var gotSystem, gotOperator string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSystem = r.Header.Get("X-System-Operator")
		gotOperator = r.Header.Get("X-Operated-By")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	defer daemon.Close()

	ctx := beadsapi.WithOperator(context.Background(), "slack:U0OPERATOR2")
	_, _ = daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{})

	if gotOperator != "slack:U0OPERATOR2" {
		t.Errorf("X-Operated-By = %q; want the per-call human operator slack:U0OPERATOR2", gotOperator)
	}
	if gotSystem != "" {
		t.Errorf("X-System-Operator = %q; want empty (no client-level sentinel default)", gotSystem)
	}
}
