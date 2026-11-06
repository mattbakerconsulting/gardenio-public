// coverage-baselines is a one-shot tool that computes per-component
// coverage baselines from a window of merged LCOV tracefiles and emits a
// baselines.json suitable for upload to the GitLab Generic Package
// Registry.
//
// One-shot per docs/research/coverage-phase2b-design.md §8: invoked once
// at the 2a→2b boundary and re-run on operator request after major
// refactors. NOT continuous — see docs/research/coverage-2b-w6-baselines.md
// for the rationale.
//
// Inputs:
//
//	--inputs-dir <dir>   Directory of <pipeline-id>.lcov tracefiles. The
//	                     filename stem is recorded as the pipeline id.
//	--commits <path>     Optional 2-column TSV (pipeline-id\tcommit-sha) so
//	                     the output records the commit SHAs alongside the
//	                     pipeline ids. Missing rows fall back to "unknown".
//	--output <path>      Where to write baselines.json (default ./baselines.json).
//
// Output: writes baselines.json. Exits non-zero on any I/O error or if
// inputs-dir contains zero LCOV files (a baseline cannot be derived from
// nothing — operator should rerun once main pipelines have populated).
//
// Usage:
//
//	coverage-baselines \
//	  --inputs-dir ./lcov-window \
//	  --commits   ./pipelines.tsv \
//	  --output    ./baselines.json
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/coverage/baselines"
	"gardenio/controller/internal/coverage/lcov"
)

func main() {
	inputsDir := flag.String("inputs-dir", "", "directory of <pipeline-id>.lcov files (required)")
	commitsPath := flag.String("commits", "", "optional TSV mapping pipeline-id\\tcommit-sha")
	output := flag.String("output", "baselines.json", "output path for baselines.json")
	flag.Parse()

	if *inputsDir == "" {
		log.Fatal("--inputs-dir is required")
	}

	commitMap, err := loadCommits(*commitsPath)
	if err != nil {
		log.Fatalf("load commits: %v", err)
	}

	samples, err := loadSamples(*inputsDir, commitMap)
	if err != nil {
		log.Fatalf("load samples: %v", err)
	}
	if len(samples) == 0 {
		log.Fatalf("no LCOV files found under %s — cannot derive baseline from zero pipelines", *inputsDir)
	}

	out := baselines.Compute(samples, time.Now().UTC())

	f, err := os.Create(*output)
	if err != nil {
		log.Fatalf("create output: %v", err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		log.Fatalf("encode: %v", err)
	}
	if err := f.Close(); err != nil {
		log.Fatalf("close: %v", err)
	}

	fmt.Printf("Wrote %s (%d pipelines, %d components)\n",
		*output, len(samples), len(out.Components))
	for c, b := range out.Components {
		fmt.Printf("  %-12s baseline=%.1f%%  threshold=%.1f%%\n", c, b.BaselinePct, b.ThresholdPct)
	}
}

// loadCommits parses a TSV of pipeline-id\tcommit-sha into a map. Missing
// path → empty map.
func loadCommits(path string) (map[string]string, error) {
	out := map[string]string{}
	if path == "" {
		return out, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		out[parts[0]] = parts[1]
	}
	return out, sc.Err()
}

// loadSamples reads every *.lcov file under dir, parses each, and returns
// the samples sorted by pipeline id (stable for reproducible JSON output).
func loadSamples(dir string, commits map[string]string) ([]baselines.PipelineSample, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.lcov"))
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	var out []baselines.PipelineSample
	for _, m := range matches {
		pipelineID := strings.TrimSuffix(filepath.Base(m), ".lcov")
		f, err := lcov.ParseFile(m)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", m, err)
		}
		commit := commits[pipelineID]
		if commit == "" {
			commit = "unknown"
		}
		out = append(out, baselines.PipelineSample{
			Pipeline: pipelineID,
			Commit:   commit,
			LCOV:     f,
		})
	}
	return out, nil
}
