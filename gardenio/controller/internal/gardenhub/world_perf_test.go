package gardenhub

import (
	"encoding/json"
	"flag"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var updateWorldGolden = flag.Bool("update-world-golden", false, "rewrite testdata/world_tenx_golden.json from the 10x fixture")

// nodeScript runs one of the testdata world scripts under node with the web directory as its first argument and returns
// the JSON report the script prints on its last line. Skipped when node is not installed.
func nodeScript(t *testing.T, script string, args ...string) map[string]any {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	web, err := filepath.Abs("web")
	if err != nil {
		t.Fatal(err)
	}
	path, err := filepath.Abs(filepath.Join("testdata", script))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, append([]string{path, web}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s failed: %v\n%s", script, err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var report map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &report); err != nil {
		t.Fatalf("%s printed no report: %v\n%s", script, err, out)
	}
	return report
}

// The 10x world (50 projects, 360 plans, ~12k task cells; testdata/world_tenx.js) pins the layout golden
// (testdata/world_tenx_golden.json: every project's lattice cell and centre, and a digest of every rectangle), determinism under
// shuffling, what a memo keeps still when beads change (state and title churn, a new project, added plans, closed plans
// closing the gap in packed slots, closed tasks, pruned seeds, a truncated document, the memo caps), the cull bounds of one
// viewport, and the data-time layout budget (homestead-world-design.md sections 3.5, 6, 9).
// Regenerate the golden with: go test ./internal/gardenhub -run WorldTenXLayout -update-world-golden
func TestWorldTenXLayoutUnderNode(t *testing.T) {
	golden, err := filepath.Abs(filepath.Join("testdata", "world_tenx_golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	args := []string{golden}
	if *updateWorldGolden {
		args = append(args, "-update")
	}
	r := nodeScript(t, "world_tenx_test.js", args...)
	t.Logf("10x layout: cold %v ms, from memo %v ms, counts %v, worst visible per level %v, memo %v bytes, document %v bytes",
		r["layout_cold_ms"], r["layout_memo_ms"], r["counts"], r["visible_worst"], r["memo_bytes"], r["doc_bytes"])
}

// The frame budget (homestead-world-design.md section 6.6; cursor-grid spec 10.3): garden.js boots under
// testdata/world_dom_mock.js with the 10x document, plus 60 gardeners on its plots (homestead-agents-as-people.md section
// 7.2), and the cursor is walked through every depth: 40 moves at the world, the densest project, its fullest plan and that
// plan's fullest phase, sibling jumps, ten descends and ten ascends, plus the pointer drags and wheel the free camera still
// takes. Per frame the script asserts the page's own JavaScript time, on the main thread's CPU clock scaled by a calibration
// workload timed beside the frames, so a loaded runner does not fail it (gd-idxhLFHgIq) (median 4 ms, p95 12 ms; a move inside the framing 3 ms at the median and no chunk bake), that layout never runs
// inside a frame, the draw-call, link, label and chunk-bake counts, that a descend settles within 64 frames, and that the gardeners' links are beside the grid inside a plot where one works. Numbers
// are logged; run with -v to see them.
func TestWorldFrameBudgetUnderNode(t *testing.T) {
	r := nodeScript(t, "world_frame_test.js")
	for _, k := range []string{"world_walk", "project_walk", "plan_walk", "phase_walk", "phase_jump0", "project_drag", "phase_wheel", "down_in_plan"} {
		if v, ok := r[k]; ok {
			t.Logf("%s: %v", k, v)
		}
	}
	t.Logf("farm document: first %v ms, patch %v ms; densest biome %v; descend/ascend p90 %v ms; gardener links in a plot %v", r["farmdoc_ms"], r["patch_ms"], r["dense"], r["descend_p90_ms"], r["gardener_hots"])
}
