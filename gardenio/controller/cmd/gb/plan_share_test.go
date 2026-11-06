package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// shareDaemon points the global daemon at a fake that answers the window
// routes and GET /v1/beads/{id}, recording each request.
func shareDaemon(t *testing.T, status int) *[]string {
	t.Helper()
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls = append(calls, r.Method+" "+r.URL.RequestURI()+" "+strings.TrimSpace(string(body)))
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"only the owner of land gardenio or an operator, on their own sign-in, may manage its windows"}`))
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/windows":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"window":{"id":"gd-w1","plan_id":"gd-p","land":"gardenio","state":"live","expires_at":"2026-11-28T00:00:00Z"},"token":"gw_TOKEN","path":"/w/gw_TOKEN"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/windows":
			_, _ = w.Write([]byte(`{"windows":[
				{"id":"gd-w1","plan_id":"gd-p","state":"live","view_count":12,"expires_at":"2026-11-28T00:00:00Z","created_by":"op@example.com"},
				{"id":"gd-w2","plan_id":"gd-q","state":"revoked","view_count":0,"expires_at":"2026-11-28T00:00:00Z"}]}`))
		case r.Method == http.MethodDelete:
			_, _ = w.Write([]byte(`{"window":{"id":"gd-w1","plan_id":"gd-p","state":"revoked","view_count":12,"revoked_at":"2026-10-29T03:00:00Z"}}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			_, _ = w.Write([]byte(`{"id":"gd-p","title":"P","type":"epic","labels":["plan","project:gardenio"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })
	return &calls
}

func runShare(t *testing.T, run func(*cobra.Command, []string) error, args ...string) (string, error) {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := run(cmd, args)
	return out.String(), err
}

func TestPlanShare_Mint(t *testing.T) {
	t.Setenv("BEADS_VIEWER_URL", "https://gardenio.clonk.tv/beads")
	calls := shareDaemon(t, http.StatusOK)
	planShareExpires = "7d"
	t.Cleanup(func() { planShareExpires = "30d" })
	out, err := runShare(t, runPlanShare, "gd-p")
	if err != nil {
		t.Fatal(err)
	}
	if want := `POST /v1/windows {"expires_in_days":7,"plan_id":"gd-p"}`; len(*calls) != 1 || (*calls)[0] != want {
		t.Errorf("calls = %q, want %q", *calls, want)
	}
	for _, want := range []string{"https://gardenio.clonk.tv/w/gw_TOKEN", "shown once", "Anyone who has this link", "unfurl", "gb plan share revoke gd-w1"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	jsonOutput = true
	t.Cleanup(func() { jsonOutput = false })
	out, err = runShare(t, runPlanShare, "gd-p")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		URL    string          `json:"url"`
		Window beadsapi.Window `json:"window"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.URL != "https://gardenio.clonk.tv/w/gw_TOKEN" || got.Window.ID != "gd-w1" {
		t.Errorf("--json = %s (%v)", out, err)
	}
}

func TestPlanShare_RefusedSaysSignIn(t *testing.T) {
	shareDaemon(t, http.StatusForbidden)
	_, err := runShare(t, runPlanShare, "gd-p")
	if err == nil || !strings.Contains(err.Error(), "gb login") {
		t.Fatalf("err = %v, want a pointer to gb login", err)
	}
}

func TestPlanShare_ListAndRevoke(t *testing.T) {
	calls := shareDaemon(t, http.StatusOK)
	t.Cleanup(func() { planShareLand, planSharePlan = "", "" })

	if _, err := runShare(t, runPlanShareList); err == nil {
		t.Error("list with neither --land nor --plan: want an error")
	}
	planShareLand = "gardenio"
	out, err := runShare(t, runPlanShareList)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "gd-w1") || !strings.Contains(out, "gd-w2") || !strings.Contains(out, "12") || strings.Contains(out, "gw_") {
		t.Errorf("list --land:\n%s", out)
	}

	planShareLand, planSharePlan = "", "gd-p"
	*calls = nil
	out, err = runShare(t, runPlanShareList)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "gd-w1") || strings.Contains(out, "gd-w2") {
		t.Errorf("list --plan gd-p:\n%s", out)
	}
	if len(*calls) != 2 || !strings.HasPrefix((*calls)[1], "GET /v1/windows?land=gardenio") {
		t.Errorf("list --plan calls = %q", *calls)
	}

	*calls = nil
	out, err = runShare(t, runPlanShareRevoke, "gd-w1")
	if err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || !strings.HasPrefix((*calls)[0], "DELETE /v1/windows/gd-w1") || !strings.Contains(out, "Revoked window gd-w1") {
		t.Errorf("revoke: calls %q, out %s", *calls, out)
	}
}

func TestParseShareExpiry(t *testing.T) {
	for in, want := range map[string]int{"30d": 30, "30": 30, "2w": 14, "1y": 365, "365d": 365, " 7D ": 7} {
		if got, err := parseShareExpiry(in); err != nil || got != want {
			t.Errorf("parseShareExpiry(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "0d", "366d", "2y", "-1d", "soon", "1h"} {
		if _, err := parseShareExpiry(bad); err == nil {
			t.Errorf("parseShareExpiry(%q): want an error", bad)
		}
	}
}

func TestWindowURL(t *testing.T) {
	t.Setenv("BEADS_VIEWER_URL", "")
	t.Setenv("GARDENIO_DEFAULT_HOST", "gardenio.clonk.tv")
	if got := windowURL("/w/gw_x"); got != "https://gardenio.clonk.tv/w/gw_x" {
		t.Errorf("windowURL = %q", got)
	}
	t.Setenv("BEADS_VIEWER_URL", "http://localhost:8095/beads")
	if got := windowURL("/w/gw_x"); got != "http://localhost:8095/w/gw_x" {
		t.Errorf("windowURL with BEADS_VIEWER_URL = %q", got)
	}
}
