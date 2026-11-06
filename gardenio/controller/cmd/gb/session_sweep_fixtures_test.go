package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// sweepFixtureDir holds one directory per surveyed problem class
// (gardenio/docs/session-sweep-survey.md), each with positive, negative and
// known-miss cases cut from real sessions. testdata/session_sweep/README.md
// says how to capture one.
const sweepFixtureDir = "testdata/session_sweep"

// sweepSurveyClasses are the survey's 26 problem classes; each must have a
// fixture directory.
var sweepSurveyClasses = []string{
	"ci-poll", "cold-compile", "gh-token-scope", "own-bead-403", "stop-did-not-take", "restart-loop",
	"noop-followthrough", "ownership-403", "killed-mid-task", "interrupt-then-crash", "stop-gate-wait",
	"idle-wait", "local-golden-suite", "ci-retrigger-churn", "browser-deps-eacces", "missing-tool",
	"pkill-self-match", "bead-history-reread", "tmpdir-unset", "wrapup-quoting",
	"stop-refused-claimed-schedule", "dispatch-collision", "infra-access-gap", "oom", "repeat-call", "dup-comment",
}

// sweepFixtureCase is a fixture's case.json.
type sweepFixtureCase struct {
	Class   string             `json:"class"`
	Sources []string           `json:"sources"`
	Note    string             `json:"note"`
	Now     time.Time          `json:"now"`
	Since   string             `json:"since"`
	Skip    string             `json:"skip"`
	Want    []sweepFixtureWant `json:"want"`
}

// sweepFixtureWant is one expected finding, compared on these four fields.
type sweepFixtureWant struct {
	Pattern string `json:"pattern"`
	Kind    string `json:"kind"`
	Agent   string `json:"agent"`
	Count   int    `json:"count"`
}

func (w sweepFixtureWant) String() string {
	return fmt.Sprintf("%s[%s] %s ×%d", w.Pattern, w.Kind, w.Agent, w.Count)
}

// sweepFixtureBead is a bead as `kd show --json` prints it; fields carry
// numbers and booleans, which beadsapi.ParseFieldsJSON flattens the way the
// client does for the live sweep.
type sweepFixtureBead struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Status    string          `json:"status"`
	CreatedAt string          `json:"created_at"`
	ClosedAt  string          `json:"closed_at"`
	Fields    json.RawMessage `json:"fields"`
}

func loadSweepFixtureBeads(t *testing.T, path string) []*beadsapi.BeadDetail {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var raw []sweepFixtureBead
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	out := make([]*beadsapi.BeadDetail, 0, len(raw))
	for _, b := range raw {
		out = append(out, &beadsapi.BeadDetail{ID: b.ID, Type: b.Type, Status: b.Status, ClosedAt: b.ClosedAt, Fields: beadsapi.ParseFieldsJSON(b.Fields)})
	}
	return out
}

// loadSweepFixture builds the sweep's input from a fixture directory. Bus
// events go through parseSweepBusLine, as the live replay's do.
func loadSweepFixture(t *testing.T, dir string) (sweepFixtureCase, sweepInput) {
	t.Helper()
	var c sweepFixtureCase
	data, err := os.ReadFile(filepath.Join(dir, "case.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("case.json: %v", err)
	}
	since, err := time.ParseDuration(c.Since)
	if err != nil || c.Now.IsZero() {
		t.Fatalf("case.json needs now and since (got %q, %q)", c.Now, c.Since)
	}
	in := sweepInput{
		Now: c.Now, Since: since, Comments: map[string][]beadsapi.Comment{},
		Sessions: loadSweepFixtureBeads(t, filepath.Join(dir, "sessions.json")),
		Agents:   loadSweepFixtureBeads(t, filepath.Join(dir, "agents.json")),
	}
	if f, err := os.Open(filepath.Join(dir, "hooks.jsonl")); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			if strings.TrimSpace(sc.Text()) == "" {
				continue
			}
			if ev, _, keep := parseSweepBusLine("data:" + sc.Text()); keep {
				in.addBusEvents([]sweepHookEvent{ev})
			}
		}
		if err := sc.Err(); err != nil {
			t.Fatal(err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "comments.json")); err == nil {
		if err := json.Unmarshal(data, &in.Comments); err != nil {
			t.Fatalf("comments.json: %v", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return c, in
}

// TestSweepDetectsSurveyedClasses runs the sweep with its default thresholds
// over every fixture: a positive must yield exactly its findings, a negative
// none of its own class (only another class's the excerpt also shows), and a
// known miss is skipped naming the bead that tracks the gap.
func TestSweepDetectsSurveyedClasses(t *testing.T) {
	classes, err := os.ReadDir(sweepFixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	var seen int
	have := map[string]bool{}
	for _, cl := range classes {
		if !cl.IsDir() {
			continue
		}
		have[cl.Name()] = true
		cases, err := os.ReadDir(filepath.Join(sweepFixtureDir, cl.Name()))
		if err != nil {
			t.Fatal(err)
		}
		// A known miss stands in for the positive while the gap is open: the
		// class is present in it, and its skip names the bead to close.
		var pos, neg int
		for _, cs := range cases {
			if !cs.IsDir() {
				continue
			}
			name := cs.Name()
			switch {
			case strings.HasPrefix(name, "positive"):
				pos++
			case strings.HasPrefix(name, "negative"):
				neg++
			case strings.HasPrefix(name, "known-miss"):
				pos++
			default:
				t.Errorf("%s/%s: a case name starts with positive, negative or known-miss", cl.Name(), name)
				continue
			}
			seen++
			dir := filepath.Join(sweepFixtureDir, cl.Name(), name)
			t.Run(cl.Name()+"/"+name, func(t *testing.T) {
				c, in := loadSweepFixture(t, dir)
				if c.Class != cl.Name() {
					t.Errorf("case.json class %q, directory %q", c.Class, cl.Name())
				}
				if strings.HasPrefix(name, "known-miss") {
					if c.Skip == "" {
						t.Fatal("a known miss names the bead tracking the gap in skip")
					}
					t.Skipf("known miss, tracked in %s: %s", c.Skip, c.Note)
				}
				if strings.HasPrefix(name, "positive") && len(c.Want) == 0 {
					t.Fatal("a positive case wants at least one finding")
				}
				if strings.HasPrefix(name, "negative") {
					for _, w := range c.Want {
						if w.Pattern == cl.Name() {
							t.Fatalf("a negative case wants no %s finding", w.Pattern)
						}
					}
				}
				findings := sweepDetect(in, defaultSweepThresholds)
				got := make([]string, 0, len(findings))
				for _, f := range findings {
					got = append(got, sweepFixtureWant{f.Pattern, f.Kind, f.Agent, f.Count}.String())
				}
				want := make([]string, 0, len(c.Want))
				for _, w := range c.Want {
					want = append(want, w.String())
				}
				sort.Strings(got)
				sort.Strings(want)
				if !slices.Equal(got, want) {
					var detail strings.Builder
					for _, f := range findings {
						fmt.Fprintf(&detail, "\n  %s[%s] %s ×%d: %s", f.Pattern, f.Kind, f.Agent, f.Count, f.Detail)
					}
					t.Errorf("%s\nwant %q\ngot  %q%s", c.Note, want, got, detail.String())
				}
			})
		}
		if pos == 0 || neg == 0 {
			t.Errorf("class %s needs a positive (or known-miss) and a negative case (has %d and %d)", cl.Name(), pos, neg)
		}
	}
	for _, c := range sweepSurveyClasses {
		if !have[c] {
			t.Errorf("surveyed class %s has no fixtures under %s", c, sweepFixtureDir)
		}
	}
	if seen == 0 {
		t.Fatal("no fixtures found")
	}
}

// TestSweepFixturesStaySmall keeps each fixture under 50 KB, so the excerpts
// stay excerpts.
func TestSweepFixturesStaySmall(t *testing.T) {
	sizes := map[string]int64{}
	err := filepath.WalkDir(sweepFixtureDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if rel, _ := filepath.Rel(sweepFixtureDir, p); strings.Count(rel, string(filepath.Separator)) == 2 {
			sizes[filepath.Dir(p)] += info.Size()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for dir, n := range sizes {
		if n > 50*1024 {
			t.Errorf("%s is %d bytes, over 50 KB", dir, n)
		}
	}
}
