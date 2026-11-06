package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// resetChannelFlags restores the package-level create flags after a test.
func resetChannelFlags(t *testing.T) {
	t.Helper()
	self := channelSelf
	channelSelf = func() string { return "" }
	t.Cleanup(func() {
		channelSelf = self
		channelMemberFlag = ""
		channelProject, channelKind, channelTitle, channelTopic = "", "", "", ""
		channelRoles, channelMembers = nil, nil
		channelDefaultRole, channelRuntimeClass, channelCardTheme, channelSlackID, channelMirror = "", "", "", "", ""
		channelListArchived = false
	})
}

func channelFieldsOf(t *testing.T, req beadsapi.CreateBeadRequest) map[string]any {
	t.Helper()
	var f map[string]any
	if err := json.Unmarshal(req.Fields, &f); err != nil {
		t.Fatalf("fields: %v", err)
	}
	return f
}

func TestBuildChannelBead_DefaultKinds(t *testing.T) {
	resetChannelFlags(t)
	for name, want := range map[string]string{"general": "general", "crew": "crew", "decisions": "decisions", "escalations": "escalations", "machines": "machines", "design": "user"} {
		req, err := buildChannelBead("gardenio", name)
		if err != nil {
			t.Fatal(err)
		}
		f := channelFieldsOf(t, req)
		if f["channel_kind"] != want || f["name"] != name {
			t.Errorf("%s: fields = %v, want channel_kind=%s", name, f, want)
		}
		if req.Type != "channel" || req.Title != "#"+name {
			t.Errorf("%s: type=%q title=%q", name, req.Type, req.Title)
		}
		if len(req.Labels) != 1 || req.Labels[0] != "project:gardenio" {
			t.Errorf("%s: labels = %v, want exactly [project:gardenio]", name, req.Labels)
		}
		// Unset optional fields are not sent.
		for _, k := range []string{"mirror", "slack_channel_id", "members", "channel_roles"} {
			if _, ok := f[k]; ok {
				t.Errorf("%s: unset %s was sent", name, k)
			}
		}
	}
}

func TestBuildChannelBead_AllFlags(t *testing.T) {
	resetChannelFlags(t)
	channelKind, channelTitle, channelTopic = "dm", "Matt and crew", "a dm"
	channelMembers = []string{"oidc:a@x.io", "agent:crew-1"}
	channelRoles = []string{"crew"}
	channelMirror, channelSlackID, channelCardTheme = "both", "D0DM", "garden"
	channelDefaultRole, channelRuntimeClass = "crew", "gvisor"
	req, err := buildChannelBead("gardenio", "dm-a-crew")
	if err != nil {
		t.Fatal(err)
	}
	f := channelFieldsOf(t, req)
	for k, want := range map[string]string{"channel_kind": "dm", "mirror": "both", "slack_channel_id": "D0DM", "card_theme": "garden", "default_role": "crew", "runtime_class": "gvisor"} {
		if f[k] != want {
			t.Errorf("%s = %v, want %s", k, f[k], want)
		}
	}
	if m, _ := f["members"].([]any); len(m) != 2 {
		t.Errorf("members = %v, want 2 entries as a JSON array", f["members"])
	}
	if r, _ := f["channel_roles"].([]any); len(r) != 1 {
		t.Errorf("channel_roles = %v, want a JSON array", f["channel_roles"])
	}
	if req.Title != "Matt and crew" || req.Description != "a dm" {
		t.Errorf("title=%q description=%q", req.Title, req.Description)
	}
}

// fakeChannelDaemon serves GET /v1/beads with the given channels and records
// close calls.
func fakeChannelDaemon(t *testing.T, channels []map[string]any) (closed *[]string, lastQuery *string) {
	t.Helper()
	var c []string
	var q string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/beads", func(w http.ResponseWriter, r *http.Request) {
		q = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": channels, "total": len(channels)})
	})
	mux.HandleFunc("POST /v1/beads/{id}/close", func(w http.ResponseWriter, r *http.Request) {
		c = append(c, r.PathValue("id"))
		_ = json.NewEncoder(w).Encode(map[string]any{"id": r.PathValue("id")})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	old := daemon
	daemon = client
	t.Cleanup(func() { daemon = old })
	return &c, &q
}

func TestResolveChannel_ByNameAndID(t *testing.T) {
	_, query := fakeChannelDaemon(t, []map[string]any{
		{"id": "gd-old", "type": "channel", "status": "closed", "title": "#crew", "fields": map[string]any{"name": "crew"}},
		{"id": "gd-crew", "type": "channel", "status": "open", "title": "#crew", "fields": map[string]any{"name": "crew"}},
	})
	ctx := context.Background()
	got, err := resolveChannel(ctx, "gardenio", "crew")
	if err != nil || got.ID != "gd-crew" {
		t.Fatalf("by name: got %v, %v; want the open gd-crew", got, err)
	}
	if !strings.Contains(*query, "project%3Agardenio") {
		t.Errorf("query %q is not scoped to the land", *query)
	}
	if got, err := resolveChannel(ctx, "gardenio", "gd-old"); err != nil || got.ID != "gd-old" {
		t.Fatalf("by id: got %v, %v", got, err)
	}
	if _, err := resolveChannel(ctx, "gardenio", "general"); err == nil {
		t.Fatal("unknown name: want an error")
	}
}

func TestChannelArchive_ClosesOnce(t *testing.T) {
	resetChannelFlags(t)
	closed, _ := fakeChannelDaemon(t, []map[string]any{
		{"id": "gd-crew", "type": "channel", "status": "open", "title": "#crew", "fields": map[string]any{"name": "crew"}},
		{"id": "gd-old", "type": "channel", "status": "closed", "title": "#old", "fields": map[string]any{"name": "old"}},
	})
	channelProject = "gardenio"
	channelArchiveCmd.SetContext(context.Background())
	if err := runChannelArchive(channelArchiveCmd, []string{"crew"}); err != nil {
		t.Fatal(err)
	}
	if err := runChannelArchive(channelArchiveCmd, []string{"gd-old"}); err != nil {
		t.Fatal(err)
	}
	if len(*closed) != 1 || (*closed)[0] != "gd-crew" {
		t.Errorf("closed = %v, want only gd-crew (gd-old was already archived)", *closed)
	}
}

func TestPrintChannelTable(t *testing.T) {
	var buf bytes.Buffer
	printChannelTable(&buf, "gardenio", []*beadsapi.BeadDetail{
		{ID: "gd-g", Title: "#general", Status: "open", Fields: map[string]string{"name": "general", "channel_kind": "general", "mirror": "both", "slack_channel_id": "C0G"}},
		{ID: "gd-x", Title: "#x", Status: "closed", Fields: map[string]string{"name": "x", "channel_kind": "user"}},
	})
	out := buf.String()
	for _, want := range []string{"gd-g", "general", "both", "C0G", "archived"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}
	buf.Reset()
	printChannelTable(&buf, "gardenio", nil)
	if !strings.Contains(buf.String(), "No channels in land gardenio") {
		t.Errorf("empty table = %q", buf.String())
	}
}
