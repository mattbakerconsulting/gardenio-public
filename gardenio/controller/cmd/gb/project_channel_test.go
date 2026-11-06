package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// fakeProjectChannelDaemon serves the reads `gb project channel` makes (project
// and role listings, GET of one bead) and records every PATCH body.
type fakeProjectChannelDaemon struct {
	mu       sync.Mutex
	projects []wireBead
	roles    []wireBead
	patches  []map[string]json.RawMessage
	patchIDs []string
}

func newFakeProjectChannelDaemon(t *testing.T, projects, roles []wireBead) *fakeProjectChannelDaemon {
	t.Helper()
	f := &fakeProjectChannelDaemon{projects: projects, roles: roles}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/beads", func(w http.ResponseWriter, r *http.Request) {
		beads := f.projects
		if strings.Contains(r.URL.Query().Get("type"), "role") {
			beads = f.roles
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": beads, "total": len(beads)})
	})
	mux.HandleFunc("GET /v1/beads/{id}", func(w http.ResponseWriter, r *http.Request) {
		for _, b := range f.projects {
			if b.ID == r.PathValue("id") {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(b)
				return
			}
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("PATCH /v1/beads/{id}", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Fields map[string]json.RawMessage `json:"fields"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("PATCH body: %v (%s)", err, raw)
		}
		f.mu.Lock()
		f.patches = append(f.patches, body.Fields)
		f.patchIDs = append(f.patchIDs, r.PathValue("id"))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })
	return f
}

// onlyPatch returns the single PATCH the command made, decoded field by field.
func (f *fakeProjectChannelDaemon) onlyPatch(t *testing.T) (string, string, map[string]string, map[string]string) {
	t.Helper()
	if len(f.patches) != 1 {
		t.Fatalf("want exactly 1 PATCH (one bead update), got %d", len(f.patches))
	}
	p := f.patches[0]
	if len(p) != 3 {
		t.Errorf("PATCH must carry only slack_channel, channel_roles, channel_modes; got keys %v", keysOf(p))
	}
	var csv string
	var roles, modes map[string]string
	if err := json.Unmarshal(p["slack_channel"], &csv); err != nil {
		t.Fatalf("slack_channel: %v", err)
	}
	if err := json.Unmarshal(p["channel_roles"], &roles); err != nil {
		t.Fatalf("channel_roles must be a JSON object: %v (%s)", err, p["channel_roles"])
	}
	if err := json.Unmarshal(p["channel_modes"], &modes); err != nil {
		t.Fatalf("channel_modes must be a JSON object: %v (%s)", err, p["channel_modes"])
	}
	return f.patchIDs[0], csv, roles, modes
}

func gardenioProjectBead(fields map[string]string) wireBead {
	return wireBead{ID: "gd-proj", Title: "gardenio", Type: "project", Status: "open", Fields: fields}
}

var crewRoleBeads = []wireBead{{ID: "gd-role-crew", Title: "crew", Type: "role", Status: "open"}}

func TestProjectChannelAdd_WritesAllThreeFieldsInOneUpdate(t *testing.T) {
	f := newFakeProjectChannelDaemon(t, []wireBead{
		gardenioProjectBead(map[string]string{
			"slack_channel": "C0AAA, C0BBB",
			"channel_roles": `{"C0AAA":"thread"}`,
			// A stringified-JSON map, as some writers stored it.
			"channel_modes": "{\"C0AAA\":\"concierge\"}\n",
			"git_url":       "https://example.com/g.git",
		}),
	}, crewRoleBeads)

	var out, errOut bytes.Buffer
	err := runProjectChannelAdd(context.Background(), &out, &errOut, projectChannelAddOpts{
		Project: "gardenio", Channel: "C0NEW", Role: "crew", Mode: "mention",
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	id, csv, roles, modes := f.onlyPatch(t)
	if id != "gd-proj" {
		t.Errorf("patched %s, want gd-proj", id)
	}
	if csv != "C0AAA,C0BBB,C0NEW" {
		t.Errorf("slack_channel = %q, want the new channel appended after the existing ones", csv)
	}
	if want := map[string]string{"C0AAA": "thread", "C0NEW": "crew"}; !reflect.DeepEqual(roles, want) {
		t.Errorf("channel_roles = %v, want %v", roles, want)
	}
	if want := map[string]string{"C0AAA": "concierge", "C0NEW": "mention"}; !reflect.DeepEqual(modes, want) {
		t.Errorf("channel_modes = %v, want %v", modes, want)
	}
	if errOut.Len() != 0 {
		t.Errorf("unexpected warning for a known role: %q", errOut.String())
	}
	if !strings.Contains(out.String(), "wired C0NEW to project gardenio") {
		t.Errorf("output = %q", out.String())
	}
}

func TestProjectChannelAdd_PrimaryMovesFirstAndKeepsUnsetEntries(t *testing.T) {
	f := newFakeProjectChannelDaemon(t, []wireBead{
		gardenioProjectBead(map[string]string{
			"slack_channel": "C0AAA,C0BBB",
			"channel_roles": `{"C0BBB":"crew"}`,
			"channel_modes": `{"C0BBB":"concierge"}`,
		}),
	}, crewRoleBeads)

	// No --role/--mode: re-adding with --primary must only reorder.
	if err := runProjectChannelAdd(context.Background(), io.Discard, io.Discard, projectChannelAddOpts{
		Project: "gardenio", Channel: "C0BBB", Primary: true,
	}); err != nil {
		t.Fatalf("add: %v", err)
	}
	_, csv, roles, modes := f.onlyPatch(t)
	if csv != "C0BBB,C0AAA" {
		t.Errorf("slack_channel = %q, want C0BBB moved first", csv)
	}
	if roles["C0BBB"] != "crew" || modes["C0BBB"] != "concierge" {
		t.Errorf("unset --role/--mode must keep the existing entries: roles=%v modes=%v", roles, modes)
	}
}

func TestProjectChannelAdd_RefusesChannelClaimedByAnotherProject(t *testing.T) {
	f := newFakeProjectChannelDaemon(t, []wireBead{
		gardenioProjectBead(map[string]string{"slack_channel": "C0AAA"}),
		{ID: "gd-other", Title: "falcon", Type: "project", Status: "open", Fields: map[string]string{"slack_channel": "C0ZZZ,C0TAKEN"}},
	}, crewRoleBeads)

	err := runProjectChannelAdd(context.Background(), io.Discard, io.Discard, projectChannelAddOpts{
		Project: "gardenio", Channel: "C0TAKEN", Role: "crew",
	})
	if err == nil {
		t.Fatal("add of a channel another project claims must fail")
	}
	if !strings.Contains(err.Error(), "already wired to project falcon") {
		t.Errorf("error = %q, want it to name the owning project", err)
	}
	if len(f.patches) != 0 {
		t.Errorf("a refused add must not write; got %d PATCH(es)", len(f.patches))
	}
}

func TestProjectChannelAdd_WarnsOnRoleWithoutRoleBead(t *testing.T) {
	f := newFakeProjectChannelDaemon(t, []wireBead{
		gardenioProjectBead(map[string]string{"slack_channel": "C0AAA"}),
	}, crewRoleBeads)

	var errOut bytes.Buffer
	if err := runProjectChannelAdd(context.Background(), io.Discard, &errOut, projectChannelAddOpts{
		Project: "gardenio", Channel: "C0NEW", Role: "crew, ghost-role",
	}); err != nil {
		t.Fatalf("an unknown role warns, it does not refuse: %v", err)
	}
	if !strings.Contains(errOut.String(), `role "ghost-role" has no type:role bead`) {
		t.Errorf("stderr = %q, want a warning naming ghost-role", errOut.String())
	}
	if strings.Contains(errOut.String(), `"crew"`) {
		t.Errorf("crew has a role bead and must not be warned about: %q", errOut.String())
	}
	_, _, roles, _ := f.onlyPatch(t)
	if roles["C0NEW"] != "crew,ghost-role" {
		t.Errorf("channel_roles[C0NEW] = %q, want the trimmed list crew,ghost-role", roles["C0NEW"])
	}
}

func TestProjectChannelAdd_RejectsUnknownMode(t *testing.T) {
	f := newFakeProjectChannelDaemon(t, []wireBead{gardenioProjectBead(nil)}, crewRoleBeads)
	err := runProjectChannelAdd(context.Background(), io.Discard, io.Discard, projectChannelAddOpts{
		Project: "gardenio", Channel: "C0NEW", Mode: "shout",
	})
	if err == nil || !strings.Contains(err.Error(), "mention, concierge") {
		t.Fatalf("err = %v, want a refusal listing the valid modes", err)
	}
	if len(f.patches) != 0 {
		t.Errorf("got %d PATCH(es) for a refused add", len(f.patches))
	}
}

func TestProjectChannelAdd_RefusesMalformedMap(t *testing.T) {
	f := newFakeProjectChannelDaemon(t, []wireBead{
		gardenioProjectBead(map[string]string{"slack_channel": "C0AAA", "channel_roles": `{"C0AAA":`}),
	}, crewRoleBeads)
	err := runProjectChannelAdd(context.Background(), io.Discard, io.Discard, projectChannelAddOpts{
		Project: "gardenio", Channel: "C0NEW", Role: "crew",
	})
	if err == nil || !strings.Contains(err.Error(), "channel_roles") {
		t.Fatalf("err = %v, want a refusal naming the malformed channel_roles", err)
	}
	if len(f.patches) != 0 {
		t.Errorf("rewriting a malformed map from empty would drop every mapping; got %d PATCH(es)", len(f.patches))
	}
}

func TestProjectChannelRm_RemovesAllThreeEntries(t *testing.T) {
	f := newFakeProjectChannelDaemon(t, []wireBead{
		gardenioProjectBead(map[string]string{
			"slack_channel": "C0AAA,C0GONE,C0BBB",
			"channel_roles": `{"C0AAA":"thread","C0GONE":"crew"}`,
			"channel_modes": `{"C0GONE":"concierge"}`,
		}),
	}, nil)

	var out bytes.Buffer
	if err := runProjectChannelRm(context.Background(), &out, "gardenio", "C0GONE"); err != nil {
		t.Fatalf("rm: %v", err)
	}
	_, csv, roles, modes := f.onlyPatch(t)
	if csv != "C0AAA,C0BBB" {
		t.Errorf("slack_channel = %q, want C0GONE dropped and the rest kept in order", csv)
	}
	if want := map[string]string{"C0AAA": "thread"}; !reflect.DeepEqual(roles, want) {
		t.Errorf("channel_roles = %v, want %v", roles, want)
	}
	if len(modes) != 0 {
		t.Errorf("channel_modes = %v, want empty", modes)
	}
	if !strings.Contains(out.String(), "unwired C0GONE") {
		t.Errorf("output = %q", out.String())
	}
}

func TestProjectChannelRm_UnwiredChannelIsNoOp(t *testing.T) {
	f := newFakeProjectChannelDaemon(t, []wireBead{
		gardenioProjectBead(map[string]string{"slack_channel": "C0AAA"}),
	}, nil)
	var out bytes.Buffer
	if err := runProjectChannelRm(context.Background(), &out, "gardenio", "C0NOPE"); err != nil {
		t.Fatalf("rm: %v", err)
	}
	if len(f.patches) != 0 {
		t.Errorf("rm of an unwired channel must not write; got %d PATCH(es)", len(f.patches))
	}
	if !strings.Contains(out.String(), "nothing to do") {
		t.Errorf("output = %q", out.String())
	}
}

func TestProjectChannelList(t *testing.T) {
	newFakeProjectChannelDaemon(t, []wireBead{
		gardenioProjectBead(map[string]string{
			"slack_channel": "C0AAA,C0BBB",
			"channel_roles": `{"C0AAA":"crew","C0ORPH":"thread"}`,
			"channel_modes": `{"C0BBB":"concierge"}`,
		}),
	}, nil)
	var out bytes.Buffer
	if err := runProjectChannelList(context.Background(), &out, "gardenio"); err != nil {
		t.Fatalf("list: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("want header + 3 rows, got:\n%s", out.String())
	}
	for i, want := range [][]string{
		{"C0AAA", "crew", "mention", "primary"},
		{"C0BBB", "-", "concierge"},
		{"C0ORPH", "thread", "mention", "not in slack_channel"},
	} {
		got := lines[i+1]
		for _, w := range want {
			if !strings.Contains(got, w) {
				t.Errorf("row %d = %q, want it to contain %q", i, got, w)
			}
		}
	}
	if strings.Contains(lines[2], "primary") {
		t.Errorf("only the first slack_channel entry is primary: %q", lines[2])
	}
}

func TestProjectChannel_UnknownProject(t *testing.T) {
	newFakeProjectChannelDaemon(t, []wireBead{gardenioProjectBead(nil)}, nil)
	err := runProjectChannelList(context.Background(), io.Discard, "nope")
	if err == nil || !strings.Contains(err.Error(), `project "nope" not found`) {
		t.Fatalf("err = %v", err)
	}
}
