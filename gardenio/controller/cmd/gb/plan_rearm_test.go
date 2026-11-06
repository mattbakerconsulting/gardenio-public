package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// gb plan rearm reopens the plan's last engine-closed, sealed gardener with
// status=open and enabled=true and nothing else (gd-miuZqFACwL); it refuses a
// gardener closed by hand or carrying no valid seal, and does nothing when the
// plan already has an open one.

const rearmDigest = "d1"

func rearmGardener(id, status string, fields map[string]any) map[string]any {
	f := map[string]any{"role": "plan-gardener", "project": "gardenio", "action": "spawn_agent",
		"gardener":      `{"max_runs":8}`,
		"spawn_lineage": `{"v":1,"author":"oidc:owner@example.com","digest":"` + rearmDigest + `","iat":1}`}
	for k, v := range fields {
		f[k] = v
	}
	return map[string]any{"id": id, "type": "trigger", "status": status, "title": "Plan gardener",
		"labels": []string{"project:gardenio", "gardener-epic:gd-plan", "plan-gardener"},
		"fields": f, "closed_at": "2026-11-04T09:24:51Z", "spawn_lineage_digest": rearmDigest}
}

// fakeRearmDaemon serves the plan's gardeners and records PATCH bodies.
func fakeRearmDaemon(t *testing.T, gardeners ...map[string]any) *[]map[string]any {
	t.Helper()
	var patches []map[string]any
	byID := map[string]map[string]any{}
	for _, g := range gardeners {
		byID[g["id"].(string)] = g
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/beads", func(w http.ResponseWriter, r *http.Request) {
		statuses := r.URL.Query().Get("status")
		var rows []map[string]any
		for _, g := range gardeners {
			if strings.Contains(statuses, g["status"].(string)) {
				rows = append(rows, g)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": rows, "total": len(rows)})
	})
	mux.HandleFunc("/v1/beads/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
		g, ok := byID[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPatch {
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			patches = append(patches, body)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(g)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	old, oldJSON := daemon, jsonOutput
	daemon, jsonOutput = c, false
	t.Cleanup(func() { daemon, jsonOutput = old, oldJSON })
	return &patches
}

func runRearm(t *testing.T) error {
	t.Helper()
	planRearmCmd.SetContext(context.Background())
	return runPlanRearm(planRearmCmd, []string{"gd-plan"})
}

func TestPlanRearm_ReopensTheEngineClosedSealedGardener(t *testing.T) {
	patches := fakeRearmDaemon(t, rearmGardener("gd-g1", "closed", map[string]any{"gardener_terminated_kind": "max_runs", "enabled": false}))
	if err := runRearm(t); err != nil {
		t.Fatalf("rearm: %v", err)
	}
	if len(*patches) != 1 {
		t.Fatalf("want one PATCH, got %d", len(*patches))
	}
	p := (*patches)[0]
	fields, _ := p["fields"].(map[string]any)
	if p["status"] != "open" || len(p) != 2 || len(fields) != 1 || fields["enabled"] != true {
		t.Fatalf("PATCH = %v, want exactly status=open and fields.enabled=true", p)
	}
}

func TestPlanRearm_Refusals(t *testing.T) {
	for name, tc := range map[string]struct {
		gardener map[string]any
		want     string
	}{
		"closed by hand": {rearmGardener("gd-g1", "closed", nil), "closed by hand"},
		"no valid seal": {rearmGardener("gd-g1", "closed", map[string]any{"gardener_terminated_kind": "max_runs",
			"spawn_lineage": `{"v":1,"author":"oidc:owner@example.com","digest":"stale","iat":1}`}), "no valid owner seal"},
	} {
		t.Run(name, func(t *testing.T) {
			patches := fakeRearmDaemon(t, tc.gardener)
			err := runRearm(t)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "gb plan gardener gd-plan") {
				t.Fatalf("err = %v, want a refusal naming %q and the bind fallback", err, tc.want)
			}
			if len(*patches) != 0 {
				t.Fatalf("a refused re-arm wrote %v", *patches)
			}
		})
	}
}

func TestPlanRearm_OpenGardenerIsLeftAlone(t *testing.T) {
	patches := fakeRearmDaemon(t,
		rearmGardener("gd-g2", "open", map[string]any{"enabled": true}),
		rearmGardener("gd-g1", "closed", map[string]any{"gardener_terminated_kind": "max_runs"}))
	if err := runRearm(t); err != nil {
		t.Fatalf("rearm: %v", err)
	}
	if len(*patches) != 0 {
		t.Fatalf("an open gardener exists, but rearm wrote %v", *patches)
	}
}
