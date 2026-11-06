package main

// grafana_embed_golden_test.go pins the exact bytes the three viewer surfaces
// embed Grafana with — /costs, the /charts hub and the `:::grafana` directive —
// so lifting the builder into internal/grafanaembed (gd-4sF22T1are) is proven
// byte-identical. testdata/grafana_embed.golden was written by the builder as it
// stood in cmd/beads-viewer before the lift; a diff here means a surface's
// iframe or link card changed. Rewrite it on purpose with
// GRAFANA_EMBED_GOLDEN_UPDATE=1 and review the diff.

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

const grafanaEmbedGoldenPath = "testdata/grafana_embed.golden"

func TestGrafanaEmbedGolden(t *testing.T) {
	var b strings.Builder
	section := func(name, body string) {
		b.WriteString("== " + name + " ==\n" + body + "\n")
	}

	for _, base := range []string{"https://grafana.clonk.tv", ""} {
		tag := "base=" + base

		for i, p := range renderCostsChartPanels(base) {
			section("costs "+tag+" #"+string(rune('1'+i)), string(p.Embed))
		}

		f := dashboardHubFake([]*beadsapi.BeadDetail{
			dashBead("gd-D1", "Fleet Live", "gardenio", "gardenio-fleet-live"),
			dashBead("gd-D2", "Perf", "monorepo", "monorepo.perf_v2"),
		}, nil, nil)
		srv := NewServer(f, slog.Default(), "", "", "", nil)
		srv.grafanaBaseURL = base
		for _, sec := range srv.buildDashboardHub(context.Background()).Sections {
			for _, c := range sec.Dashboards {
				section("charts "+tag+" "+c.UID, string(c.HeroEmbed))
			}
		}

		for _, d := range []string{
			`:::grafana gardenio-fleet`,
			`<p>:::grafana gardenio-fleet panel=3</p>`,
			`<p data-block-id="blk-2">:::grafana gardenio-fleet panel=3 height=240</p>`,
			`:::grafana gardenio-fleet panel=3 height=99999`,
			`:::grafana gardenio-fleet panel=3 height=-5`,
			`:::grafana gardenio-fleet panel=x`,
			`:::grafana gardenio-fleet panel=7 var-agent=gardenio-foo-123 var-model=claude-opus-4.8`,
			`:::grafana gardenio-fleet var-project=gardenio`,
			`:::grafana a.b_c-D`,
		} {
			section("directive "+tag+" "+d, embedGrafanaDirectives(d, base))
		}
	}
	got := b.String()

	if os.Getenv("GRAFANA_EMBED_GOLDEN_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(grafanaEmbedGoldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(grafanaEmbedGoldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(grafanaEmbedGoldenPath)
	if err != nil {
		t.Fatalf("read golden (set GRAFANA_EMBED_GOLDEN_UPDATE=1 to write it): %v", err)
	}
	if got != string(want) {
		gl, wl := strings.Split(got, "\n"), strings.Split(string(want), "\n")
		for i := 0; i < len(gl) || i < len(wl); i++ {
			var g, w string
			if i < len(gl) {
				g = gl[i]
			}
			if i < len(wl) {
				w = wl[i]
			}
			if g != w {
				t.Fatalf("embed markup drifted from %s at line %d:\n got: %s\nwant: %s", grafanaEmbedGoldenPath, i+1, g, w)
			}
		}
	}
}
