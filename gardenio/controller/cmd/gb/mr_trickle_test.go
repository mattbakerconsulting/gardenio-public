package main

import (
	"testing"

	"gardenio/controller/internal/bridge"
)

func TestParseTristate(t *testing.T) {
	if parseTristate("") != nil {
		t.Error("empty should be nil (unset)")
	}
	for _, s := range []string{"true", "TRUE", "1", "yes"} {
		if b := parseTristate(s); b == nil || !*b {
			t.Errorf("parseTristate(%q) should be &true", s)
		}
	}
	for _, s := range []string{"false", "0", "no", "nope"} {
		if b := parseTristate(s); b == nil || *b {
			t.Errorf("parseTristate(%q) should be &false", s)
		}
	}
}

func TestInferSingleConcern(t *testing.T) {
	cases := []struct {
		name  string
		files []string
		want  bool
	}{
		{"single tree", []string{"gardenio/a.go", "gardenio/b.go"}, true},
		{"two trees", []string{"gardenio/a.go", "kbeads/b.go"}, false},
		{"lock excluded so single", []string{"gardenio/a.go", "coop/Cargo.lock"}, true},
		{"empty", nil, true},
		{"top-level file", []string{"Makefile"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := inferSingleConcern(c.files); got != c.want {
				t.Errorf("inferSingleConcern(%v) = %v, want %v", c.files, got, c.want)
			}
		})
	}
}

func TestTrickleGlyph(t *testing.T) {
	tru, fal := true, false
	cases := []struct {
		it   bridge.GateItem
		want string
	}{
		{bridge.GateItem{NA: true}, "–"},
		{bridge.GateItem{Pass: nil}, "☐"},
		{bridge.GateItem{Pass: &tru}, "✓"},
		{bridge.GateItem{Pass: &fal}, "✗"},
	}
	for _, c := range cases {
		if got := trickleGlyph(c.it); got != c.want {
			t.Errorf("trickleGlyph(%+v) = %q, want %q", c.it, got, c.want)
		}
	}
}

func TestResolveTrickleSmallDiff_ExcludesGeneratedAndReadsTrailers(t *testing.T) {
	// Reset the package-level override flags this resolver reads.
	origSC, origSA := mrTrickleSingleConcern, mrTrickleStandalone
	t.Cleanup(func() { mrTrickleSingleConcern, mrTrickleStandalone = origSC, origSA })
	mrTrickleSingleConcern = ""
	mrTrickleStandalone = ""

	desc := "Some prose.\nTrickle-Size-Exception: mechanical regen\n"
	files := []string{"gardenio/a.go", "gardenio/go.sum", "kbeads/gen/x.pb.go"}
	stats := &bridge.GitLabMRDiffStats{Additions: 500, Deletions: 20}

	s := resolveTrickleSmallDiff(desc, files, stats, true)
	if s.FilesChanged != 1 {
		t.Errorf("FilesChanged = %d, want 1 (go.sum + generated excluded)", s.FilesChanged)
	}
	if s.NetLOC != 520 {
		t.Errorf("NetLOC = %d, want 520", s.NetLOC)
	}
	if s.SizeExceptionTrailer != "mechanical regen" {
		t.Errorf("SizeExceptionTrailer = %q", s.SizeExceptionTrailer)
	}
	// single-concern inferred: only gardenio/a.go counts → single tree.
	if !s.SingleConcern {
		t.Error("expected single-concern true (only gardenio/ counted)")
	}

	// When the diff could not be read, the leg is pending (blocks under --enforce).
	if p := resolveTrickleSmallDiff(desc, files, stats, false); p.Evaluated {
		t.Error("unresolvable diff should leave small-diff pending (Evaluated=false)")
	}
}

func TestResolveTricklePerf_PendingWhenUnresolvable(t *testing.T) {
	if p := resolveTricklePerf([]string{"kbeads/internal/store/postgres/x.go"}, false); p.Evaluated {
		t.Error("unresolvable changed-files should leave perf pending, not falsely N/A")
	}
	p := resolveTricklePerf([]string{"kbeads/internal/store/postgres/x.go"}, true)
	if !p.Evaluated || !p.PerfSensitivePathsTouched {
		t.Errorf("resolvable perf path should be evaluated+triggered; got %+v", p)
	}
}
