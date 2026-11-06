package tests

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A duplicate mapping key in a values file is not an error to helm: it
// unmarshals through sigs.k8s.io/yaml, which is last-key-wins. So a second
// top-level `beads:` block merged in beside the first (PR #308, branched
// before #324, 2026-10-14) silently DROPS the first block -- there that was
// beads.machineClients, and with it the machine principal the live /costs fix
// depends on. git merges two hunks that add the same key at different places
// without a conflict, so nothing complains until the rollout misbehaves.
// yaml.v3 does not have that blind spot: it keeps both key nodes, so this
// walks them and names every repeat as file:line (gd-ofqTZwZ4xn).

// valuesFileGlobs are the values files the guard covers, relative to the repo
// root. Each is a path.Match-style glob for filepath.Glob. deploy-values/ is
// matched on *.yaml only: its *.yaml.tmpl siblings carry ${VAR} placeholders
// and are not YAML on their own.
var valuesFileGlobs = []string{
	"deploy-values/*.yaml",
	"gardenio/helm/*/values.yaml",
	"gardenio/helm/values*.yaml",
}

// duplicateKey is one repeated mapping key.
type duplicateKey struct {
	Path      string // dotted path of the mapping that holds the key; "" at the top level
	Key       string
	Line, Col int
	FirstLine int
}

func (d duplicateKey) String(file string) string {
	where := d.Path
	if where == "" {
		where = "(top level)"
	}
	return fmt.Sprintf("%s:%d:%d: duplicate key %q in %s (first defined at line %d; last-key-wins silently drops that one)",
		file, d.Line, d.Col, d.Key, where, d.FirstLine)
}

// findDuplicateKeys parses every document in src and returns each repeated
// mapping key at any depth, in source order. A parse error is returned as-is.
//
// Keys are compared by their text, not their YAML type: helm round-trips the
// values through JSON, where the keys 1 and "1" are the same string, so they
// collide in practice even though YAML calls them distinct.
func findDuplicateKeys(src []byte) ([]duplicateKey, error) {
	dec := yaml.NewDecoder(bytes.NewReader(src))
	var dups []duplicateKey
	for {
		var doc yaml.Node
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		walkDuplicateKeys(&doc, "", &dups)
	}
	sort.SliceStable(dups, func(i, j int) bool {
		if dups[i].Line != dups[j].Line {
			return dups[i].Line < dups[j].Line
		}
		return dups[i].Col < dups[j].Col
	})
	return dups, nil
}

func walkDuplicateKeys(n *yaml.Node, path string, out *[]duplicateKey) {
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			walkDuplicateKeys(c, path, out)
		}
	case yaml.SequenceNode:
		for i, c := range n.Content {
			walkDuplicateKeys(c, fmt.Sprintf("%s[%d]", path, i), out)
		}
	case yaml.MappingNode:
		seen := map[string]int{} // key text -> line of first definition
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Kind == yaml.ScalarNode {
				if first, dup := seen[k.Value]; dup {
					*out = append(*out, duplicateKey{Path: path, Key: k.Value, Line: k.Line, Col: k.Column, FirstLine: first})
				} else {
					seen[k.Value] = k.Line
				}
			}
			child := k.Value
			if path != "" {
				child = path + "." + k.Value
			}
			// An alias points at a node already walked where its anchor is
			// defined; following it would report that node's repeats twice.
			if v.Kind != yaml.AliasNode {
				walkDuplicateKeys(v, child, out)
			}
		}
	}
}

// valuesFiles resolves valuesFileGlobs under repoRoot. It fails when the
// deploy-values glob matches nothing: a guard whose input vanished (a rename,
// a path change) would otherwise pass forever on zero files.
func valuesFiles(t *testing.T, repoRoot string) []string {
	t.Helper()
	seen := map[string]bool{}
	var files []string
	for _, g := range valuesFileGlobs {
		matches, err := filepath.Glob(filepath.Join(repoRoot, g))
		if err != nil {
			t.Fatalf("bad glob %q: %v", g, err)
		}
		if len(matches) == 0 && (g == "deploy-values/*.yaml" || g == "gardenio/helm/*/values.yaml") {
			t.Fatalf("glob %q matched no files under %s: the guard would pass on nothing", g, repoRoot)
		}
		for _, m := range matches {
			if !seen[m] {
				seen[m] = true
				files = append(files, m)
			}
		}
	}
	sort.Strings(files)
	return files
}

// TestValuesFilesHaveNoDuplicateKeys fails on a repeated mapping key, at any
// depth, in deploy-values/*.yaml and the chart values files.
func TestValuesFilesHaveNoDuplicateKeys(t *testing.T) {
	t.Parallel()
	repoRoot := filepath.Join(chartDir(t), "..", "..", "..")
	for _, f := range valuesFiles(t, repoRoot) {
		rel, err := filepath.Rel(repoRoot, f)
		if err != nil {
			rel = f
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Errorf("%s: %v", rel, err)
			continue
		}
		dups, err := findDuplicateKeys(src)
		if err != nil {
			t.Errorf("%s: not parseable as YAML: %v", rel, err)
			continue
		}
		for _, d := range dups {
			t.Error(d.String(rel))
		}
	}
}

// TestFindDuplicateKeys is the positive control for the guard above: planted
// duplicates MUST be reported, with the right line, so a parser change that
// started swallowing them would turn the real check into a no-op that passes.
func TestFindDuplicateKeys(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		src  string
		want []string // "line:key" per expected finding
	}{
		{
			name: "clean",
			src:  "beads:\n  machineClients: [a]\nviewer:\n  beads: x\n",
		},
		{
			// The PR #308 shape: the same top-level key added twice.
			name: "top-level key repeated",
			src:  "beads:\n  machineClients: [a]\nviewer:\n  on: true\nbeads:\n  other: 1\n",
			want: []string{"5:beads"},
		},
		{
			name: "nested key repeated",
			src:  "a:\n  b:\n    c: 1\n    d: 2\n    c: 3\n",
			want: []string{"5:c"},
		},
		{
			name: "repeat inside a sequence item",
			src:  "list:\n  - name: x\n    v: 1\n    name: y\n",
			want: []string{"4:name"},
		},
		{
			name: "same key under different parents is fine",
			src:  "a:\n  k: 1\nb:\n  k: 2\n",
		},
		{
			name: "every repeat is reported",
			src:  "x: 1\nx: 2\nx: 3\n",
			want: []string{"2:x", "3:x"},
		},
		{
			name: "second document is checked too",
			src:  "a: 1\n---\nb: 1\nb: 2\n",
			want: []string{"4:b"},
		},
		{
			name: "quoted and bare keys collide (helm round-trips through JSON)",
			src:  "\"1\": a\n1: b\n",
			want: []string{"2:1"},
		},
		{
			name: "key text inside a block scalar is not a key",
			src:  "cfg: |\n  a: 1\n  a: 2\nother: 1\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dups, err := findDuplicateKeys([]byte(tc.src))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			var got []string
			for _, d := range dups {
				got = append(got, fmt.Sprintf("%d:%s", d.Line, d.Key))
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("duplicates = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestValuesDuplicateKeyCheckFailsOnPlantedFile runs the same per-file path the
// real check uses against a planted file on disk and asserts the failure
// message names file:line -- the actionable part for whoever hits the red.
func TestValuesDuplicateKeyCheckFailsOnPlantedFile(t *testing.T) {
	t.Parallel()
	src := "beads:\n  machineClients: [a]\nbeads:\n  other: 1\n"
	dups, err := findDuplicateKeys([]byte(src))
	if err != nil || len(dups) != 1 {
		t.Fatalf("planted duplicate not detected: dups=%v err=%v", dups, err)
	}
	msg := dups[0].String("deploy-values/planted.yaml")
	if !strings.HasPrefix(msg, "deploy-values/planted.yaml:3:1: duplicate key \"beads\"") {
		t.Fatalf("message does not name file:line:col, got %q", msg)
	}
	if !strings.Contains(msg, "first defined at line 1") {
		t.Fatalf("message does not name the first definition, got %q", msg)
	}
}
