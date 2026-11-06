package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// runSlackRouteAgainst runs gb slack route <arg> against a fake bridge and
// returns the query it sent and the text it printed.
func runSlackRouteAgainst(t *testing.T, arg string, resp slackRouteResponse) (string, string) {
	t.Helper()
	var query string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/slack/route" || r.Method != http.MethodGet {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		query = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()
	t.Setenv("SLACK_BRIDGE_URL", server.URL)

	var out bytes.Buffer
	slackRouteCmd.SetOut(&out)
	defer slackRouteCmd.SetOut(nil)
	if err := runSlackRoute(slackRouteCmd, []string{arg}); err != nil {
		t.Fatalf("runSlackRoute: %v", err)
	}
	return query, out.String()
}

func TestSlackRoute_NamesWinningRuleAndLosers(t *testing.T) {
	resp := slackRouteResponse{
		Agent: "worker", Channel: "C-crew", ChannelName: "gardenio-crew", Rule: "role_channel",
		Project: "gardenio", Role: "crew", SpawnChannel: "C-spawn", InheritedFromRole: "gardener",
		Candidates: []slackRouteCandidate{
			{Rule: "router", Detail: "no router configured"},
			{Rule: "spawn_channel", Detail: "spawn channel C-spawn was inherited from a gardener spawner"},
			{Rule: "role_channel", Channel: "C-crew", ChannelName: "gardenio-crew", Applies: true, Won: true, Detail: "channel_roles maps role crew in project gardenio"},
			{Rule: "inherited_spawn_channel", Channel: "C-spawn", Applies: true, Detail: "inherited from a gardener spawner"},
		},
		CardFound: true, CardChannel: "C-old", CardTS: "1.2",
	}
	query, out := runSlackRouteAgainst(t, "worker", resp)
	if query != "agent=worker" {
		t.Errorf("query = %q", query)
	}
	for _, want := range []string{
		"Channel:  #gardenio-crew (C-crew)",
		"Rule:     role_channel — channel_roles maps role crew in project gardenio",
		"inherited_from_role=gardener",
		"Card:     C-old ts 1.2",
		"not in the routed channel",
		"won   role_channel",
		"lost  inherited_spawn_channel  C-spawn  inherited from a gardener spawner",
		"-     router",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestSlackRoute_BeadIDQueriesBead(t *testing.T) {
	query, out := runSlackRouteAgainst(t, "gd-XahY2dfWYP", slackRouteResponse{
		Agent: "worker", Bead: "gd-XahY2dfWYP", BeadType: "task", Channel: "C1", Rule: "bot_default",
		Candidates: []slackRouteCandidate{{Rule: "bot_default", Channel: "C1", Applies: true, Won: true, Detail: "bridge default channel"}},
	})
	if query != "bead=gd-XahY2dfWYP" {
		t.Errorf("query = %q", query)
	}
	if !strings.Contains(out, "worker  (from task gd-XahY2dfWYP)") || !strings.Contains(out, "Card:     none recorded") {
		t.Errorf("output:\n%s", out)
	}
}

func TestSlackRoute_BridgeErrorSurfaces(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":"bead gd-x (task) has no agent to route for"}`))
	}))
	defer server.Close()
	t.Setenv("SLACK_BRIDGE_URL", server.URL)
	err := runSlackRoute(slackRouteCmd, []string{"gd-x"})
	if err == nil || !strings.Contains(err.Error(), "has no agent to route for") {
		t.Errorf("err = %v", err)
	}
}
