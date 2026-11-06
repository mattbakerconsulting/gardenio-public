package beadsapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The machine-callable calls must carry no alias list: ResyncUserBeadAliases
// sends only the bead id, and PruneUserBeadAliases sends only the source — the
// daemon derives both the aliases and the keep set (gd-uAvleAZUvV).
func TestUserBeadAliasCalls_SendNoAliasList(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = map[string]any{}
		_ = json.Unmarshal(b, &gotBody)
		_, _ = w.Write([]byte(`{"operator_id":1,"nothing_to_link":true,"deleted":[],"count":0}`))
	}))
	defer srv.Close()
	c, err := New(Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := c.ResyncUserBeadAliases(t.Context(), "gd-user1")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/admin/operators/link-aliases" || !resp.NothingToLink {
		t.Errorf("path %q resp %+v", gotPath, resp)
	}
	if len(gotBody) != 1 || gotBody["resync_bead_id"] != "gd-user1" {
		t.Errorf("resync body = %v, want only resync_bead_id", gotBody)
	}

	if _, err := c.PruneUserBeadAliases(t.Context()); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/admin/operators/prune-aliases" {
		t.Errorf("path %q", gotPath)
	}
	if len(gotBody) != 1 || gotBody["source"] != "user-bead-bridge" {
		t.Errorf("prune body = %v, want only source=user-bead-bridge (no keep list)", gotBody)
	}
}
