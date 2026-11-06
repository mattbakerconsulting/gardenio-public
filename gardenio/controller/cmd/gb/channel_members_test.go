package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestNextChannelMembers(t *testing.T) {
	members := []string{"oidc:a@x.io", "agent:b"}
	if got, changed := nextChannelMembers(members, "oidc:c@x.io", true); !changed || !slices.Equal(got, []string{"oidc:a@x.io", "agent:b", "oidc:c@x.io"}) {
		t.Errorf("join: %v %v", got, changed)
	}
	if got, changed := nextChannelMembers(members, "agent:b", false); !changed || !slices.Equal(got, []string{"oidc:a@x.io"}) {
		t.Errorf("leave: %v %v", got, changed)
	}
	if _, changed := nextChannelMembers(members, "agent:b", true); changed {
		t.Error("joining twice changed the list")
	}
	if _, changed := nextChannelMembers(members, "oidc:c@x.io", false); changed {
		t.Error("leaving a channel one is not in changed the list")
	}
	if !slices.Equal(members, []string{"oidc:a@x.io", "agent:b"}) {
		t.Errorf("input mutated: %v", members)
	}
}

func TestChannelMemberList(t *testing.T) {
	ch := &beadsapi.BeadDetail{Fields: map[string]string{"members": `["oidc:a@x.io","agent:b"]`}}
	if got := channelMemberList(ch); !slices.Equal(got, []string{"oidc:a@x.io", "agent:b"}) {
		t.Errorf("members = %v", got)
	}
	for _, raw := range []string{"", "not json"} {
		if got := channelMemberList(&beadsapi.BeadDetail{Fields: map[string]string{"members": raw}}); len(got) != 0 {
			t.Errorf("members of %q = %v, want none", raw, got)
		}
	}
}

// fakeMembershipDaemon serves one channel bead for list and get, records the
// fields of every PATCH to it, and applies the patched members.
func fakeMembershipDaemon(t *testing.T, channel map[string]any) *[]map[string]any {
	t.Helper()
	var patches []map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/beads", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []map[string]any{channel}, "total": 1})
	})
	mux.HandleFunc("GET /v1/beads/{id}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(channel)
	})
	mux.HandleFunc("PATCH /v1/beads/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Fields map[string]any `json:"fields"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		patches = append(patches, body.Fields)
		if f, ok := channel["fields"].(map[string]any); ok {
			f["members"] = body.Fields["members"]
		}
		_ = json.NewEncoder(w).Encode(channel)
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
	return &patches
}

func TestChannelJoinAndLeave(t *testing.T) {
	resetChannelFlags(t)
	channelProject = "gardenio"
	channelSelf = func() string { return "oidc:me@x.io" }
	patches := fakeMembershipDaemon(t, map[string]any{
		"id": "gd-design", "type": "channel", "status": "open", "title": "#design",
		"fields": map[string]any{"name": "design", "channel_kind": "user", "members": []string{"oidc:a@x.io"}},
	})
	channelJoinCmd.SetContext(context.Background())

	if err := runChannelMembership(channelJoinCmd, "design", true); err != nil {
		t.Fatal(err)
	}
	if err := runChannelMembership(channelJoinCmd, "design", false); err != nil {
		t.Fatal(err)
	}
	channelMemberFlag = "oidc:a@x.io"
	if err := runChannelMembership(channelJoinCmd, "design", false); err != nil {
		t.Fatal(err)
	}
	// Leaving a channel one is not in sends nothing.
	channelMemberFlag = "oidc:nobody@x.io"
	if err := runChannelMembership(channelJoinCmd, "design", false); err != nil {
		t.Fatal(err)
	}

	want := [][]any{{"oidc:a@x.io", "oidc:me@x.io"}, {"oidc:a@x.io"}, {}}
	if len(*patches) != len(want) {
		t.Fatalf("patches = %v, want %d", *patches, len(want))
	}
	for i, p := range *patches {
		if len(p) != 1 {
			t.Errorf("patch %d touches %v, want members only", i, p)
		}
		got, _ := p["members"].([]any)
		if len(got) != len(want[i]) || (len(got) > 0 && !slices.Equal(got, want[i])) {
			t.Errorf("patch %d members = %v, want %v", i, p["members"], want[i])
		}
	}
}

func TestChannelJoin_Refusals(t *testing.T) {
	resetChannelFlags(t)
	channelProject = "gardenio"
	patches := fakeMembershipDaemon(t, map[string]any{
		"id": "gd-general", "type": "channel", "status": "open", "title": "#general",
		"fields": map[string]any{"name": "general", "channel_kind": "general"},
	})
	channelJoinCmd.SetContext(context.Background())
	if err := runChannelMembership(channelJoinCmd, "general", true); err == nil || !strings.Contains(err.Error(), "cannot tell who you are") {
		t.Errorf("no identity: %v", err)
	}
	channelSelf = func() string { return "oidc:me@x.io" }
	if err := runChannelMembership(channelJoinCmd, "general", true); err == nil || !strings.Contains(err.Error(), "only a user channel") {
		t.Errorf("join general: %v", err)
	}
	if len(*patches) != 0 {
		t.Errorf("refusals patched: %v", *patches)
	}
}

func TestBuildChannelBead_CreatorIsFirstMember(t *testing.T) {
	resetChannelFlags(t)
	channelSelf = func() string { return "agent:crew-1" }
	req, err := buildChannelBead("gardenio", "design")
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := channelFieldsOf(t, req)["members"].([]any); len(m) != 1 || m[0] != "agent:crew-1" {
		t.Errorf("user channel members = %v, want [agent:crew-1]", m)
	}
	req, err = buildChannelBead("gardenio", "general")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := channelFieldsOf(t, req)["members"]; ok {
		t.Error("a default channel got members")
	}
}
