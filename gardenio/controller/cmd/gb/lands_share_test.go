package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/planwindow"
)

// landShareView is a land view as the daemon's preview answers it: one open
// plan whose current phase is named, one with an internal reference in its
// title, and a finished one; four open plans in all.
const landShareView = `{"land":"gardenio","open_plans":4,"expires_at":"2026-11-29T00:00:00Z","plans":[
	{"title":"Lands: the land window","status":"open","state":"building","current_phase":1,"steps_done":4,"steps_total":6,
	 "phases":[{"number":1,"title":"Build it","status":"open","steps":[]}],"open_decisions":1,"open_escalations":1,
	 "crew":[{"name":"gardenio-lw5","live":true},{"name":"gardenio-lw4","live":false}]},
	{"title":"Move the kbeads bucket","status":"open","state":"ready","steps_done":0,"steps_total":2},
	{"title":"Old plan","status":"closed","state":"complete","steps_done":3,"steps_total":3}]}`

// landShareDaemon points the global daemon at a fake that answers the window
// routes for land windows, recording each request.
func landShareDaemon(t *testing.T, status int) *[]string {
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
			_, _ = w.Write([]byte(`{"window":{"id":"gd-lw1","target_kind":"land","plan_id":"","land":"gardenio","state":"live","expires_at":"2026-11-29T00:00:00Z"},"token":"gw_LAND","path":"/w/gw_LAND"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/windows/preview":
			_, _ = w.Write([]byte(landShareView))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/windows":
			_, _ = w.Write([]byte(`{"windows":[
				{"id":"gd-lw1","target_kind":"land","plan_id":"","land":"gardenio","state":"live","view_count":5,"expires_at":"2026-11-29T00:00:00Z","created_by":"op@example.com"},
				{"id":"gd-w1","target_kind":"plan","plan_id":"gd-p","land":"gardenio","state":"live","view_count":12,"expires_at":"2026-11-28T00:00:00Z"}]}`))
		case r.Method == http.MethodDelete:
			_, _ = w.Write([]byte(`{"window":{"id":"gd-lw1","target_kind":"land","land":"gardenio","state":"revoked","view_count":5,"revoked_at":"2026-10-30T03:00:00Z"}}`))
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

func TestLandsShare_Mint(t *testing.T) {
	t.Setenv("BEADS_VIEWER_URL", "https://gardenio.clonk.tv/beads")
	calls := landShareDaemon(t, http.StatusOK)
	landsShareExpires = "2w"
	t.Cleanup(func() { landsShareExpires = "30d" })
	out, err := runShare(t, runLandsShare, "gardenio")
	if err != nil {
		t.Fatal(err)
	}
	if want := `POST /v1/windows {"expires_in_days":14,"land":"gardenio"}`; len(*calls) != 1 || (*calls)[0] != want {
		t.Errorf("calls = %q, want %q", *calls, want)
	}
	for _, want := range []string{"whole of land gardenio", "https://gardenio.clonk.tv/w/gw_LAND", "shown once", planwindow.LandGrants, "gb lands share revoke gd-lw1"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	landsShareExpires = "400d"
	if _, err := runShare(t, runLandsShare, "gardenio"); err == nil || !strings.Contains(err.Error(), "365d") {
		t.Errorf("--expires 400d: err = %v, want the 365d bound", err)
	}
	landsShareExpires = "30d"

	jsonOutput = true
	t.Cleanup(func() { jsonOutput = false })
	out, err = runShare(t, runLandsShare, "gardenio")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		URL    string          `json:"url"`
		Grants string          `json:"grants"`
		Window beadsapi.Window `json:"window"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.URL != "https://gardenio.clonk.tv/w/gw_LAND" || got.Window.TargetKind != "land" || got.Grants != planwindow.LandGrants {
		t.Errorf("--json = %s (%v)", out, err)
	}
}

func TestLandsShare_RefusedSaysSignIn(t *testing.T) {
	landShareDaemon(t, http.StatusForbidden)
	for name, run := range map[string]func() (string, error){
		"mint":    func() (string, error) { return runShare(t, runLandsShare, "gardenio") },
		"preview": func() (string, error) { return runShare(t, runLandsSharePreview, "gardenio") },
		"list":    func() (string, error) { return runShare(t, runLandsShareList, "gardenio") },
		"revoke":  func() (string, error) { return runShare(t, runLandsShareRevoke, "gd-lw1") },
	} {
		if _, err := run(); err == nil || !strings.Contains(err.Error(), "gb login") {
			t.Errorf("%s: err = %v, want a pointer to gb login", name, err)
		}
	}
}

func TestLandsShare_ListAndRevoke(t *testing.T) {
	calls := landShareDaemon(t, http.StatusOK)
	out, err := runShare(t, runLandsShareList, "gardenio")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "gd-lw1") || strings.Contains(out, "gd-w1 ") || !strings.Contains(out, "op@example.com") || strings.Contains(out, "gw_") {
		t.Errorf("list shows the land windows only, without a token:\n%s", out)
	}
	if len(*calls) != 1 || !strings.HasPrefix((*calls)[0], "GET /v1/windows?land=gardenio") {
		t.Errorf("list calls = %q", *calls)
	}

	*calls = nil
	out, err = runShare(t, runLandsShareRevoke, "gd-lw1")
	if err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || !strings.HasPrefix((*calls)[0], "DELETE /v1/windows/gd-lw1") || !strings.Contains(out, "Revoked window gd-lw1 on the whole of land gardenio") {
		t.Errorf("revoke: calls %q, out %s", *calls, out)
	}

	// gb plan share list names a land window by what it shows.
	planShareLand = "gardenio"
	t.Cleanup(func() { planShareLand = "" })
	out, err = runShare(t, runPlanShareList)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "gd-lw1  (whole land)") {
		t.Errorf("plan share list --land:\n%s", out)
	}
}

func TestLandsShare_Preview(t *testing.T) {
	t.Setenv("BEADS_VIEWER_URL", "https://gardenio.clonk.tv/beads")
	calls := landShareDaemon(t, http.StatusOK)
	landsShareExpires = "7d"
	t.Cleanup(func() { landsShareExpires = "30d" })
	out, err := runShare(t, runLandsSharePreview, "gardenio")
	if err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || !strings.HasPrefix((*calls)[0], "GET /v1/windows/preview?expires_in_days=7&land=gardenio") {
		t.Errorf("preview calls = %q (it must mint nothing)", *calls)
	}
	for _, want := range []string{
		"nothing is minted",
		"Lands: the land window", "Building", "Build it", "4 of 6", // the plan, its state, current phase and progress
		"Finished · Complete",          // the finished plan
		"and 2 more open plans",        // 4 open, 2 shown
		"1 withheld", "plans[1].title", // the internal reference in a title
		planwindow.WithheldText,
		"https://gardenio.clonk.tv/beads/p/gardenio/share/preview?days=7",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("preview lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "kbeads") {
		t.Errorf("preview prints a withheld title:\n%s", out)
	}

	jsonOutput = true
	t.Cleanup(func() { jsonOutput = false })
	out, err = runShare(t, runLandsSharePreview, "gardenio")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		View     planwindow.LandPayload `json:"view"`
		Withheld []map[string]string    `json:"withheld"`
		Page     string                 `json:"page"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || len(got.View.Plans) != 3 || len(got.Withheld) != 1 || !strings.HasSuffix(got.Page, "/p/gardenio/share/preview?days=7") {
		t.Errorf("--json = %s (%v)", out, err)
	}
}
