package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
)

// gardenioLike mirrors the operator's gardenio project: a primary Slack
// channel, more channels, and channel_roles that never name "crew".
func gardenioLike() beadsapi.ProjectInfo {
	return beadsapi.ProjectInfo{
		Name:          "gardenio",
		SlackChannels: []string{"CPRIMARY", "CCICD", "CPLAN"},
		ChannelRoles:  map[string][]string{"CCICD": {"cicd"}, "CPLAN": {"plan-captain"}},
	}
}

func seedByName(t *testing.T, plan []seedChannel) map[string]seedChannel {
	t.Helper()
	if len(plan) != 4 {
		t.Fatalf("plan has %d channels, want 4: %+v", len(plan), plan)
	}
	out := map[string]seedChannel{}
	for i, sc := range plan {
		if sc.Name != seedChannelOrder[i] {
			t.Errorf("plan[%d] = %s, want %s", i, sc.Name, seedChannelOrder[i])
		}
		out[sc.Name] = sc
	}
	return out
}

func TestPlanLandSeed_PrimaryBindsGeneralOnly(t *testing.T) {
	plan := seedByName(t, planLandSeed(gardenioLike(), nil, nil, nil))
	g := plan["general"]
	if g.Action != "create" || g.SlackChannel != "CPRIMARY" || g.Mirror != "both" {
		t.Errorf("general = %+v, want create bound to CPRIMARY mirror=both", g)
	}
	// Crew, decisions and escalations all route to the primary channel today,
	// which #general already mirrors: created unbound, saying why.
	for _, name := range []string{"crew", "decisions", "escalations"} {
		sc := plan[name]
		if sc.Action != "create" || sc.SlackChannel != "" || sc.Mirror != "off" {
			t.Errorf("%s = %+v, want create unbound mirror=off", name, sc)
		}
		if !strings.Contains(sc.Note, "CPRIMARY") || !strings.Contains(sc.Note, "#general") {
			t.Errorf("%s note = %q, want it to name CPRIMARY and #general", name, sc.Note)
		}
	}
}

func TestPlanLandSeed_CrewRoleChannel(t *testing.T) {
	p := gardenioLike()
	p.ChannelRoles["CCREW"] = []string{"crew", "crew-lite"}
	p.ChannelRuntimeClasses = map[string]string{"CCREW": "runsc"}
	p.CardTheme = "business"
	plan := seedByName(t, planLandSeed(p, nil, nil, nil))
	c := plan["crew"]
	if c.SlackChannel != "CCREW" || c.Mirror != "both" {
		t.Fatalf("crew = %+v, want bound to CCREW", c)
	}
	if strings.Join(c.Roles, ",") != "crew,crew-lite" || c.DefaultRole != "crew" || c.RuntimeClass != "runsc" || c.CardTheme != "business" {
		t.Errorf("crew carried config = %+v", c)
	}
	// Decisions and escalations follow the crew card channel.
	for _, name := range []string{"decisions", "escalations"} {
		if sc := plan[name]; sc.SlackChannel != "" || !strings.Contains(sc.Note, "#crew") {
			t.Errorf("%s = %+v, want unbound behind #crew", name, sc)
		}
	}
	if g := plan["general"]; g.SlackChannel != "CPRIMARY" || g.Roles != nil || g.RuntimeClass != "" {
		t.Errorf("general = %+v, want CPRIMARY without CCREW's config", g)
	}
}

func TestPlanLandSeed_RouterPatternWinsForCrew(t *testing.T) {
	p := gardenioLike()
	p.ChannelRoles["CCREW"] = []string{"crew"}
	router := bridge.NewRouter(bridge.RouterConfig{
		DefaultChannel: "CDEFAULT",
		Channels:       map[string]string{"gardenio/crew/*": "CROUTED", "other/crew/*": "COTHER"},
	})
	plan := seedByName(t, planLandSeed(p, router, nil, nil))
	if c := plan["crew"]; c.SlackChannel != "CROUTED" {
		t.Errorf("crew = %+v, want the router's CROUTED", c)
	}
	// The router default is not a crew-specific route.
	router = bridge.NewRouter(bridge.RouterConfig{DefaultChannel: "CDEFAULT"})
	if c := seedByName(t, planLandSeed(p, router, nil, nil))["crew"]; c.SlackChannel != "CCREW" {
		t.Errorf("crew with only a router default = %+v, want CCREW", c)
	}
}

func TestPlanLandSeed_ExistingLeftAsIsAndClaimsItsBinding(t *testing.T) {
	// An operator already made #crew and bound it to the primary channel.
	existing := map[string]*beadsapi.BeadDetail{
		"crew": {ID: "gd-crew", Fields: map[string]string{"name": "crew", "slack_channel_id": "CPRIMARY", "mirror": "both"}},
	}
	plan := seedByName(t, planLandSeed(gardenioLike(), nil, existing, map[string]string{"CPRIMARY": "gd-crew"}))
	if c := plan["crew"]; c.Action != "exists" || c.ID != "gd-crew" || c.SlackChannel != "CPRIMARY" || c.Note != "" {
		t.Errorf("crew = %+v, want exists, unchanged, no note", c)
	}
	if g := plan["general"]; g.Action != "create" || g.SlackChannel != "" || !strings.Contains(g.Note, "#crew") {
		t.Errorf("general = %+v, want create unbound (CPRIMARY held by #crew)", g)
	}
}

func TestPlanLandSeed_ExistingUnboundNotesTheRoute(t *testing.T) {
	existing := map[string]*beadsapi.BeadDetail{
		"general": {ID: "gd-gen", Fields: map[string]string{"name": "general", "mirror": "off"}},
	}
	plan := seedByName(t, planLandSeed(gardenioLike(), nil, existing, nil))
	if g := plan["general"]; g.Action != "exists" || !strings.Contains(g.Note, "CPRIMARY") {
		t.Errorf("general = %+v, want exists noting the CPRIMARY route", g)
	}
	// Nothing in the land mirrors CPRIMARY, so the next default channel
	// that routes there takes the binding.
	if c := plan["crew"]; c.SlackChannel != "CPRIMARY" {
		t.Errorf("crew = %+v, want it to take the free CPRIMARY binding", c)
	}
}

func TestPlanLandSeed_BoundInAnotherLand(t *testing.T) {
	plan := seedByName(t, planLandSeed(gardenioLike(), nil, nil, map[string]string{"CPRIMARY": "gd-other"}))
	if g := plan["general"]; g.SlackChannel != "" || g.Mirror != "off" || !strings.Contains(g.Note, "gd-other") {
		t.Errorf("general = %+v, want unbound, naming gd-other", g)
	}
}

func TestPlanLandSeed_NoSlackChannel(t *testing.T) {
	plan := seedByName(t, planLandSeed(beadsapi.ProjectInfo{Name: "quiet"}, nil, nil, nil))
	for name, sc := range plan {
		if sc.Action != "create" || sc.SlackChannel != "" || sc.Mirror != "off" || sc.Note != "project has no Slack channel" {
			t.Errorf("%s = %+v", name, sc)
		}
	}
}

func TestSeedChannelRequest(t *testing.T) {
	req, err := seedChannelRequest(seedChannel{
		Land: "gardenio", Name: "crew", SlackChannel: "CCREW", Mirror: "both",
		Roles: []string{"crew"}, DefaultRole: "crew", RuntimeClass: "runsc", CardTheme: "garden",
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.Type != "channel" || req.Title != "#crew" || len(req.Labels) != 1 || req.Labels[0] != "project:gardenio" {
		t.Errorf("req = %+v", req)
	}
	f := channelFieldsOf(t, req)
	for k, want := range map[string]string{"name": "crew", "channel_kind": "crew", "mirror": "both", "slack_channel_id": "CCREW", "default_role": "crew", "runtime_class": "runsc", "card_theme": "garden"} {
		if f[k] != want {
			t.Errorf("%s = %v, want %s", k, f[k], want)
		}
	}
	if r, _ := f["channel_roles"].([]any); len(r) != 1 {
		t.Errorf("channel_roles = %v, want a JSON array", f["channel_roles"])
	}

	req, _ = seedChannelRequest(seedChannel{Land: "gardenio", Name: "decisions", Mirror: "off"})
	f = channelFieldsOf(t, req)
	if f["mirror"] != "off" {
		t.Errorf("unbound mirror = %v, want off", f["mirror"])
	}
	for _, k := range []string{"slack_channel_id", "channel_roles", "default_role", "runtime_class", "card_theme"} {
		if _, ok := f[k]; ok {
			t.Errorf("unbound channel sent %s", k)
		}
	}
}

func TestPlanLandFields_UnsetSetsBoth(t *testing.T) {
	p := gardenioLike()
	p.BeadID = "gd-proj"
	fields := planLandFields(p, planLandSeed(p, nil, nil, nil))
	if len(fields) != 2 {
		t.Fatalf("fields = %+v, want 2", fields)
	}
	def, mirror := fields[0], fields[1]
	// #general is still to be created, so its id is only known on --apply.
	if def.Field != "land_default_channel" || def.Action != "set" || def.Value != "" || def.ProjectID != "gd-proj" || !strings.Contains(def.Note, "#general") {
		t.Errorf("land_default_channel = %+v, want set to the new #general", def)
	}
	if mirror.Field != "land_slack_mirror" || mirror.Action != "set" || mirror.Value != "on" {
		t.Errorf("land_slack_mirror = %+v, want set on", mirror)
	}
}

func TestPlanLandFields_ExistingGeneral(t *testing.T) {
	existing := map[string]*beadsapi.BeadDetail{
		"general": {ID: "gd-gen", Fields: map[string]string{"name": "general", "slack_channel_id": "CPRIMARY", "mirror": "both"}},
	}
	p := gardenioLike()
	fields := planLandFields(p, planLandSeed(p, nil, existing, map[string]string{"CPRIMARY": "gd-gen"}))
	if def := fields[0]; def.Action != "set" || def.Value != "gd-gen" {
		t.Errorf("land_default_channel = %+v, want set to gd-gen", def)
	}
}

func TestPlanLandFields_SetValuesLeftAsIs(t *testing.T) {
	existing := map[string]*beadsapi.BeadDetail{
		"general": {ID: "gd-gen", Fields: map[string]string{"name": "general"}},
	}
	p := gardenioLike()
	p.LandDefaultChannel, p.LandSlackMirror = "gd-lobby", "off"
	fields := planLandFields(p, planLandSeed(p, nil, existing, nil))
	if def := fields[0]; def.Action != "exists" || def.Value != "gd-lobby" || !strings.Contains(def.Note, "gd-gen") {
		t.Errorf("land_default_channel = %+v, want exists gd-lobby noting #general gd-gen", def)
	}
	if mirror := fields[1]; mirror.Action != "exists" || mirror.Value != "off" {
		t.Errorf("land_slack_mirror = %+v, want exists off", mirror)
	}
}

func TestPlanLandFields_NoSlackChannelMirrorsOff(t *testing.T) {
	p := beadsapi.ProjectInfo{Name: "quiet"}
	if mirror := planLandFields(p, planLandSeed(p, nil, nil, nil))[1]; mirror.Action != "set" || mirror.Value != "off" {
		t.Errorf("land_slack_mirror = %+v, want set off", mirror)
	}
}

// fakeSeed is what the fake daemon recorded: channel creates and project
// field patches by bead id.
type fakeSeed struct {
	created []beadsapi.CreateBeadRequest
	patched map[string]map[string]any
}

// fakeSeedDaemon serves the project and channel lists and records creates.
func fakeSeedDaemon(t *testing.T, projects, channels []map[string]any) *[]beadsapi.CreateBeadRequest {
	t.Helper()
	return &fakeSeedDaemonOpts(t, projects, channels, false).created
}

// fakeSeedDaemonOpts is fakeSeedDaemon; noChannelType makes the daemon refuse
// type=channel the way a daemon older than the channel type does.
func fakeSeedDaemonOpts(t *testing.T, projects, channels []map[string]any, noChannelType bool) *fakeSeed {
	t.Helper()
	rec := &fakeSeed{patched: map[string]map[string]any{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/beads", func(w http.ResponseWriter, r *http.Request) {
		beads := channels
		if noChannelType && r.URL.Query().Get("type") == "channel" {
			http.Error(w, `{"error":"unknown bead type \"channel\"; valid types: project"}`, http.StatusBadRequest)
			return
		}
		if r.URL.Query().Get("type") == "project" {
			beads = projects
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": beads, "total": len(beads)})
	})
	mux.HandleFunc("POST /v1/beads", func(w http.ResponseWriter, r *http.Request) {
		var req beadsapi.CreateBeadRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		rec.created = append(rec.created, req)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "gd-new" + string(rune('a'+len(rec.created)-1)), "type": "channel", "title": req.Title})
	})
	mux.HandleFunc("GET /v1/beads/{id}", func(w http.ResponseWriter, r *http.Request) {
		for _, p := range projects {
			if p["id"] == r.PathValue("id") {
				_ = json.NewEncoder(w).Encode(p)
				return
			}
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("PATCH /v1/beads/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Fields map[string]any `json:"fields"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		rec.patched[r.PathValue("id")] = body.Fields
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
	return rec
}

func resetSeedFlags(t *testing.T) {
	t.Helper()
	channelSeedCmd.SetContext(context.Background())
	t.Cleanup(func() {
		channelProject, seedRouterConfig = "", ""
		seedAllLands, seedApply, jsonOutput = false, false, false
	})
}

var seedProjects = []map[string]any{
	{"id": "gd-proj", "type": "project", "status": "open", "title": "gardenio", "fields": map[string]any{
		"slack_channel": "CPRIMARY,CCICD", "channel_roles": `{"CCICD":"cicd"}`,
	}},
	{"id": "gd-proj2", "type": "project", "status": "open", "title": "claimlayer", "fields": map[string]any{"slack_channel": "CCLAIM"}},
}

func TestRunChannelSeed_DryRunWritesNothing(t *testing.T) {
	resetSeedFlags(t)
	created := fakeSeedDaemon(t, seedProjects, nil)
	channelProject = "gardenio"
	out := string(captureStdout(t, func() error { return runChannelSeed(channelSeedCmd, nil) }))
	if len(*created) != 0 {
		t.Errorf("dry run created %d beads", len(*created))
	}
	if !strings.Contains(out, "Dry run: 4 channel(s) to create and 2 land field(s) to set") || strings.Contains(out, "claimlayer") {
		t.Errorf("output:\n%s", out)
	}
	if !strings.Contains(out, "land_default_channel") || !strings.Contains(out, "land_slack_mirror") {
		t.Errorf("dry run should show the land fields:\n%s", out)
	}
}

func TestRunChannelSeed_ApplyAllLands(t *testing.T) {
	resetSeedFlags(t)
	// claimlayer already has #general bound to its channel: re-running must
	// not touch it.
	rec := fakeSeedDaemonOpts(t, seedProjects, []map[string]any{
		{"id": "gd-cg", "type": "channel", "status": "open", "title": "#general", "labels": []string{"project:claimlayer"},
			"fields": map[string]any{"name": "general", "channel_kind": "general", "slack_channel_id": "CCLAIM", "mirror": "both"}},
	}, false)
	created := &rec.created
	seedAllLands, seedApply, jsonOutput = true, true, true
	out := string(captureStdout(t, func() error { return runChannelSeed(channelSeedCmd, nil) }))
	if len(*created) != 7 {
		t.Fatalf("created %d channels, want 7 (4 gardenio + 3 claimlayer)", len(*created))
	}
	var result seedResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	plan := result.Channels
	if plan[0].Land != "claimlayer" || plan[0].Action != "exists" || plan[0].ID != "gd-cg" {
		t.Errorf("plan[0] = %+v, want claimlayer #general exists", plan[0])
	}
	for _, sc := range plan[1:] {
		if sc.Action != "created" || sc.ID == "" {
			t.Errorf("%s/%s = %+v, want created with an id", sc.Land, sc.Name, sc)
		}
	}
	bound := 0
	for _, req := range *created {
		var f map[string]any
		_ = json.Unmarshal(req.Fields, &f)
		if f["slack_channel_id"] == "CPRIMARY" {
			bound++
		}
		if f["slack_channel_id"] == "CCLAIM" {
			t.Errorf("re-bound CCLAIM: %s", req.Fields)
		}
	}
	if bound != 1 {
		t.Errorf("CPRIMARY bound %d times, want exactly once", bound)
	}
	// claimlayer opens on the #general it already had; gardenio on the one
	// just created (the first create, gd-newd after claimlayer's three).
	var gardenioGeneral string
	for _, sc := range plan {
		if sc.Land == "gardenio" && sc.Name == "general" {
			gardenioGeneral = sc.ID
		}
	}
	for id, want := range map[string]map[string]any{
		"gd-proj2": {"land_default_channel": "gd-cg", "land_slack_mirror": "on"},
		"gd-proj":  {"land_default_channel": gardenioGeneral, "land_slack_mirror": "on"},
	} {
		got := rec.patched[id]
		if len(got) != len(want) || got["land_default_channel"] != want["land_default_channel"] || got["land_slack_mirror"] != want["land_slack_mirror"] {
			t.Errorf("%s patched %v, want %v", id, got, want)
		}
	}
	for _, lf := range result.LandFields {
		if lf.Action != "updated" || lf.Value == "" {
			t.Errorf("%s/%s = %+v, want updated with a value", lf.Land, lf.Field, lf)
		}
	}
}

func TestRunChannelSeed_ApplyLeavesSetLandFields(t *testing.T) {
	resetSeedFlags(t)
	rec := fakeSeedDaemonOpts(t, []map[string]any{
		{"id": "gd-proj", "type": "project", "status": "open", "title": "gardenio", "fields": map[string]any{
			"slack_channel": "CPRIMARY", "land_default_channel": "gd-lobby", "land_slack_mirror": "off",
		}},
	}, nil, false)
	channelProject, seedApply = "gardenio", true
	out := string(captureStdout(t, func() error { return runChannelSeed(channelSeedCmd, nil) }))
	if len(rec.patched) != 0 {
		t.Errorf("patched %v, want the set land fields left alone", rec.patched)
	}
	if !strings.Contains(out, "gd-lobby") {
		t.Errorf("output should report the existing land_default_channel:\n%s", out)
	}
}

func TestRunChannelSeed_NeedsALand(t *testing.T) {
	resetSeedFlags(t)
	fakeSeedDaemon(t, seedProjects, nil)
	t.Setenv("KD_PROJECT", "")
	t.Setenv("BOAT_PROJECT", "")
	if err := runChannelSeed(channelSeedCmd, nil); err == nil || !strings.Contains(err.Error(), "--all-lands") {
		t.Errorf("err = %v, want a hint naming --all-lands", err)
	}
	channelProject = "nope"
	if err := runChannelSeed(channelSeedCmd, nil); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("err = %v, want unknown project refused", err)
	}
}

func TestRunChannelSeed_DaemonWithoutChannelType(t *testing.T) {
	resetSeedFlags(t)
	rec := fakeSeedDaemonOpts(t, seedProjects, nil, true)
	created := &rec.created
	channelProject = "gardenio"
	out := string(captureStdout(t, func() error { return runChannelSeed(channelSeedCmd, nil) }))
	if !strings.Contains(out, "Dry run: 4 channel(s) to create") {
		t.Errorf("dry run on an old daemon should still plan:\n%s", out)
	}
	seedApply = true
	if err := runChannelSeed(channelSeedCmd, nil); err == nil || !strings.Contains(err.Error(), "channel") {
		t.Errorf("apply on an old daemon: err = %v, want the unknown-type refusal", err)
	}
	if len(*created) != 0 {
		t.Errorf("created %d beads on an old daemon", len(*created))
	}
}
