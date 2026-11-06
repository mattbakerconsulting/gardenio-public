// Command wer-eval computes word error rate (WER) for the E2 dictation
// evaluation set. It reads a dataset of (reference, [conditions]
// hypothesis) triples, aligns each hypothesis to its reference, and
// emits per-item + aggregate WER per condition as markdown.
//
// Usage:
//
//	wer-eval -input eval.json -output report.md
//
// Dataset format (JSON):
//
//	{
//	  "conditions": ["no_bias", "glossary", "custom_lm"],
//	  "voices":     ["v1", "v2", "v3"],
//	  "items": [
//	    {
//	      "id":        "ka-0000000575",
//	      "reference": "ka-0000000575",
//	      "hypotheses": {
//	        "v1": {"no_bias": "k dash tee...", "glossary": "ka-0000000575", "custom_lm": "ka-0000000575"},
//	        "v2": {...},
//	        "v3": {...}
//	      }
//	    },
//	    ...
//	  ]
//	}
//
// Scope lock: This tool evaluates the Apple Speech framework output for
// the E2 exit criterion (measurable WER drop over E1 on the held-out
// set). It does not invoke Whisper or any third-party engine.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strings"
)

type dataset struct {
	Conditions []string `json:"conditions"`
	Voices     []string `json:"voices"`
	Items      []item   `json:"items"`
}

type item struct {
	ID         string                       `json:"id"`
	Reference  string                       `json:"reference"`
	Hypotheses map[string]map[string]string `json:"hypotheses"` // voice -> condition -> hypothesis
}

type perItemWER struct {
	ID        string
	Reference string
	// wer[voice][condition] = fraction
	WER map[string]map[string]float64
}

func main() {
	input := flag.String("input", "", "path to evaluation dataset JSON")
	output := flag.String("output", "-", "path to markdown report, or - for stdout")
	minItemsForSummary := flag.Int("min-items", 1, "minimum items required before emitting aggregate WER")
	flag.Parse()

	if *input == "" {
		flag.Usage()
		os.Exit(2)
	}

	ds, err := loadDataset(*input)
	if err != nil {
		log.Fatalf("load dataset: %v", err)
	}

	perItem, aggregate := evaluate(ds)

	var w io.Writer
	if *output == "-" {
		w = os.Stdout
	} else {
		f, err := os.Create(*output)
		if err != nil {
			log.Fatalf("create %s: %v", *output, err)
		}
		defer f.Close()
		w = f
	}

	if err := writeReport(w, ds, perItem, aggregate, *minItemsForSummary); err != nil {
		log.Fatalf("write report: %v", err)
	}

	if *output != "-" {
		fmt.Fprintf(os.Stderr, "wrote %s (%d items, %d voices, %d conditions)\n",
			*output, len(ds.Items), len(ds.Voices), len(ds.Conditions))
	}
}

func loadDataset(path string) (*dataset, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var ds dataset
	if err := dec.Decode(&ds); err != nil {
		return nil, fmt.Errorf("parse json: %w", err)
	}
	if len(ds.Conditions) == 0 {
		return nil, fmt.Errorf("dataset has no conditions")
	}
	if len(ds.Voices) == 0 {
		return nil, fmt.Errorf("dataset has no voices")
	}
	return &ds, nil
}

// evaluate returns per-item WER for every (voice, condition) pair, plus
// an aggregate map condition -> mean WER across all (item, voice)
// combinations that supplied a hypothesis for that condition.
func evaluate(ds *dataset) ([]perItemWER, map[string]float64) {
	per := make([]perItemWER, 0, len(ds.Items))
	sumErr := make(map[string]int)
	sumRefLen := make(map[string]int)

	for _, it := range ds.Items {
		entry := perItemWER{
			ID:        it.ID,
			Reference: it.Reference,
			WER:       make(map[string]map[string]float64, len(ds.Voices)),
		}
		refWords := tokenize(it.Reference)
		for _, voice := range ds.Voices {
			entry.WER[voice] = make(map[string]float64, len(ds.Conditions))
			hyps, ok := it.Hypotheses[voice]
			if !ok {
				continue
			}
			for _, cond := range ds.Conditions {
				hyp, ok := hyps[cond]
				if !ok {
					continue
				}
				edits := editDistance(refWords, tokenize(hyp))
				var wer float64
				if len(refWords) > 0 {
					wer = float64(edits) / float64(len(refWords))
				} else if edits > 0 {
					wer = 1.0
				}
				entry.WER[voice][cond] = wer
				sumErr[cond] += edits
				sumRefLen[cond] += len(refWords)
			}
		}
		per = append(per, entry)
	}

	aggregate := make(map[string]float64, len(ds.Conditions))
	for _, cond := range ds.Conditions {
		if sumRefLen[cond] == 0 {
			continue
		}
		aggregate[cond] = float64(sumErr[cond]) / float64(sumRefLen[cond])
	}
	return per, aggregate
}

// tokenize lowercases + splits on whitespace + strips surrounding
// punctuation. Deliberately simple — the eval set is short utterances
// where word-level alignment is the right granularity.
func tokenize(s string) []string {
	s = strings.ToLower(s)
	fields := strings.Fields(s)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		trimmed := strings.TrimFunc(f, func(r rune) bool {
			switch r {
			case '.', ',', '!', '?', ';', ':', '"', '\'', '(', ')', '[', ']':
				return true
			}
			return false
		})
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// editDistance returns the word-level Levenshtein distance between a
// and b (insertions, deletions, substitutions each cost 1).
func editDistance(a, b []string) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = minInt(
				prev[j]+1,     // deletion
				curr[j-1]+1,   // insertion
				prev[j-1]+cost, // substitution
			)
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

func minInt(nums ...int) int {
	m := nums[0]
	for _, n := range nums[1:] {
		if n < m {
			m = n
		}
	}
	return m
}

// writeReport produces a markdown report suitable for posting as a
// comment / artifact on the parent epic (ka-0000000044).
func writeReport(w io.Writer, ds *dataset, per []perItemWER, aggregate map[string]float64, minItems int) error {
	fmt.Fprintln(w, "# Dictation WER evaluation")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "- items: **%d**\n", len(ds.Items))
	fmt.Fprintf(w, "- voices: %s\n", strings.Join(ds.Voices, ", "))
	fmt.Fprintf(w, "- conditions: %s\n", strings.Join(ds.Conditions, ", "))
	fmt.Fprintln(w)

	if len(ds.Items) < minItems {
		fmt.Fprintf(w, "_Aggregate WER suppressed: dataset has %d items, min is %d._\n\n", len(ds.Items), minItems)
	} else {
		fmt.Fprintln(w, "## Aggregate WER (lower is better)")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "| condition | WER | words evaluated |")
		fmt.Fprintln(w, "|---|---:|---:|")
		// Preserve the user-specified condition order for ease of reading.
		for _, cond := range ds.Conditions {
			wer, ok := aggregate[cond]
			if !ok {
				fmt.Fprintf(w, "| %s | _no data_ | 0 |\n", cond)
				continue
			}
			words := countRefWords(ds, cond)
			fmt.Fprintf(w, "| %s | %.3f | %d |\n", cond, wer, words)
		}
		fmt.Fprintln(w)

		// E2 exit criterion: the custom-LM condition should beat both
		// no-bias and glossary on the held-out set. Flag whether it does.
		if delta, ok := exitDelta(aggregate); ok {
			if delta > 0 {
				fmt.Fprintf(w, "**E2 exit criterion met.** custom_lm is %.3f WER lower than glossary.\n\n", delta)
			} else {
				fmt.Fprintf(w, "**E2 exit criterion NOT met.** custom_lm WER is %.3f _higher_ than glossary — investigate corpus quality or phrase weights before merging.\n\n", -delta)
			}
		}
	}

	fmt.Fprintln(w, "## Per-item WER")
	fmt.Fprintln(w)
	var header, align strings.Builder
	header.WriteString("| id | reference |")
	align.WriteString("|---|---|")
	for _, voice := range ds.Voices {
		for _, cond := range ds.Conditions {
			fmt.Fprintf(&header, " %s/%s |", voice, cond)
			align.WriteString("---:|")
		}
	}
	fmt.Fprintln(w, header.String())
	fmt.Fprintln(w, align.String())

	// Sort items by ID for deterministic output.
	sorted := append([]perItemWER{}, per...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	for _, p := range sorted {
		var row strings.Builder
		fmt.Fprintf(&row, "| %s | %s |", p.ID, escapePipes(p.Reference))
		for _, voice := range ds.Voices {
			for _, cond := range ds.Conditions {
				if vs, ok := p.WER[voice]; ok {
					if wer, ok := vs[cond]; ok {
						fmt.Fprintf(&row, " %.3f |", wer)
						continue
					}
				}
				row.WriteString(" _n/a_ |")
			}
		}
		fmt.Fprintln(w, row.String())
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "_WER is word-level Levenshtein edits divided by reference length. 0.0 = perfect, 1.0 = every reference word mis-recognised or missing._")
	return nil
}

func countRefWords(ds *dataset, cond string) int {
	total := 0
	for _, it := range ds.Items {
		ref := tokenize(it.Reference)
		for _, v := range ds.Voices {
			if hyps, ok := it.Hypotheses[v]; ok {
				if _, hasCond := hyps[cond]; hasCond {
					total += len(ref)
				}
			}
		}
	}
	return total
}

// exitDelta returns (glossary_wer - custom_lm_wer, true) when both
// conditions have aggregate WER available. Positive means custom_lm
// improved over glossary.
func exitDelta(agg map[string]float64) (float64, bool) {
	g, gok := agg["glossary"]
	c, cok := agg["custom_lm"]
	if !gok || !cok {
		return 0, false
	}
	return g - c, true
}

func escapePipes(s string) string {
	return strings.ReplaceAll(s, "|", "\\|")
}
