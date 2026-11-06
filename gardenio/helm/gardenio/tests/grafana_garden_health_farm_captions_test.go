package tests

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The "Farm: bird captions" row of gardenio-garden-health (gd-FOzMlrXga6, step 10 of plan gd-jMl4oF2cJO; spec
// gardenio/docs/superpowers/specs/2026-10-28-bird-captions-design.md sections 8 and 11.3). Board only: the garden
// embeds none of these, so they are pinned here rather than in gardenHealthPanelIDs, and they are fleet-wide (a
// caption is made once in the viewer, for every land). The table in gardenio/docs/garden-grafana-panels.md ("After
// 4.1") lists them as board only; change both together.

var gardenHealthFarmPanels = []struct {
	id    int
	typ   string
	title string
	reads string // the series the panel must read
}{
	{14, "timeseries", "Caption sources", "gardenio_farm_caption_total"},
	{15, "timeseries", "Haiku caption latency (p50, p95)", "gardenio_farm_caption_haiku_seconds_bucket"},
	{16, "stat", "Haiku caption spend (window)", "gardenio_farm_caption_haiku_tokens_total"},
	{17, "timeseries", "Haiku calls skipped", "gardenio_farm_caption_haiku_skipped_total"},
	{18, "timeseries", "Transcript bytes per viewer", "gardenio_farm_transcript_bytes_total"},
}

func TestGardenHealthDashboard_FarmCaptionsRow(t *testing.T) {
	t.Parallel()
	b := parseGardenHealthBoard(t)
	if p := gardenHealthPanel(t, b, 103); p.Type != "row" || p.Title != "Farm: bird captions" {
		t.Errorf("panel 103 = %s %q, want the row \"Farm: bird captions\"", p.Type, p.Title)
	}
	for _, want := range gardenHealthFarmPanels {
		p := gardenHealthPanel(t, b, want.id)
		if p.Type != want.typ || p.Title != want.title {
			t.Errorf("panel %d = %s %q, want %s %q", want.id, p.Type, p.Title, want.typ, want.title)
		}
		if p.Datasource == nil || p.Datasource.UID != "victoriametrics" {
			t.Errorf("panel %d %q must bind the victoriametrics datasource", p.ID, p.Title)
		}
		if p.Description == "" {
			t.Errorf("panel %d %q has no description", p.ID, p.Title)
		}
		if len(p.Targets) == 0 || !strings.Contains(p.Targets[0].Expr, want.reads) {
			t.Errorf("panel %d %q must read %s: %+v", p.ID, p.Title, want.reads, p.Targets)
		}
		for _, tgt := range p.Targets {
			if strings.Contains(tgt.Expr, "$project") {
				t.Errorf("farm panel %d %q is cut by the project variable (captions are fleet-wide): %s", p.ID, p.Title, tgt.Expr)
			}
		}
	}
}

// The spend is priced from the tokens each reply reported, at Haiku 4.5's $1 / $5 per million, over the picker's
// range, as one instant number: a stat that does not move with the step.
func TestGardenHealthDashboard_FarmCaptionSpendIsPricedTokens(t *testing.T) {
	t.Parallel()
	p := gardenHealthPanel(t, parseGardenHealthBoard(t), 16)
	const want = `sum(increase(gardenio_farm_caption_haiku_tokens_total{type="input"}[$__range])) * 1e-06 + ` +
		`sum(increase(gardenio_farm_caption_haiku_tokens_total{type="output"}[$__range])) * 5e-06`
	if len(p.Targets) != 1 || p.Targets[0].Expr != want || !p.Targets[0].Instant {
		t.Fatalf("panel 16 targets = %+v, want the single instant expr %s", p.Targets, want)
	}
}

// Transcript bytes are per viewer: the bytes rate over the streams holding a transcript open, never divided by zero.
func TestGardenHealthDashboard_FarmTranscriptBytesPerViewer(t *testing.T) {
	t.Parallel()
	p := gardenHealthPanel(t, parseGardenHealthBoard(t), 18)
	const want = `sum(rate(gardenio_farm_transcript_bytes_total[$__rate_interval])) / clamp_min(sum(gardenio_farm_transcript_clients), 1)`
	if len(p.Targets) == 0 || p.Targets[0].Expr != want {
		t.Fatalf("panel 18 first target = %+v, want %s", p.Targets, want)
	}
}

// Every gardenio_farm_* series the row reads is one beads-viewer registers, so a renamed metric reds here rather
// than leaving an empty panel on the deployed farm.
func TestGardenHealthDashboard_FarmSeriesAreRegistered(t *testing.T) {
	t.Parallel()
	var src strings.Builder
	for _, f := range []string{
		"../../controller/internal/gardenhub/captioner/metrics.go",
		"../../controller/cmd/beads-viewer/main.go",
	} {
		b, err := os.ReadFile(filepath.Join(chartDir(t), f))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		src.Write(b)
	}
	registered := src.String()
	series := regexp.MustCompile(`gardenio_farm_[a-z_]+`)
	b := parseGardenHealthBoard(t)
	for _, want := range gardenHealthFarmPanels {
		for _, tgt := range gardenHealthPanel(t, b, want.id).Targets {
			for _, name := range series.FindAllString(tgt.Expr, -1) {
				base := strings.TrimSuffix(name, "_bucket")
				if !strings.Contains(registered, `"`+base+`"`) {
					t.Errorf("panel %d reads %s, which beads-viewer does not register", want.id, name)
				}
			}
		}
	}
}
