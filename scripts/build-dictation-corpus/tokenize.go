package main

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// reTokenSplit splits on any run of whitespace or punctuation that isn't
// useful for in-token context. We keep internal hyphens, slashes, and dots
// by carving tokens on whitespace and a conservative punctuation set first,
// then filtering. This preserves things like "go.mod", "scripts/release.sh",
// "ka-0000005040", "gardenio/controller".
var reTokenSplit = regexp.MustCompile(`[\s,;:!?"()\[\]{}<>|]+`)

// stopwords is a minimal english stoplist tuned for engineering prose:
// pronouns, articles, auxiliaries, and top modal/prepositional words.
// Deliberately small so jargon (dev, repo, bead, etc.) still surfaces.
var stopwords = map[string]struct{}{
	"a": {}, "an": {}, "the": {},
	"and": {}, "or": {}, "but": {}, "so": {}, "if": {}, "then": {}, "than": {},
	"is": {}, "was": {}, "are": {}, "were": {}, "be": {}, "been": {}, "being": {},
	"do": {}, "does": {}, "did": {}, "done": {},
	"have": {}, "has": {}, "had": {},
	"to": {}, "from": {}, "for": {}, "of": {}, "on": {}, "in": {}, "at": {},
	"by": {}, "with": {}, "without": {}, "into": {}, "out": {}, "over": {},
	"as": {}, "that": {}, "this": {}, "these": {}, "those": {},
	"it": {}, "its": {}, "i": {}, "we": {}, "you": {}, "they": {}, "he": {}, "she": {},
	"will": {}, "would": {}, "can": {}, "could": {}, "should": {}, "shall": {}, "may": {}, "might": {}, "must": {},
	"not": {}, "no": {}, "yes": {},
	"there": {}, "here": {}, "when": {}, "where": {}, "what": {}, "which": {}, "who": {}, "how": {}, "why": {},
	"just": {}, "only": {}, "also": {}, "too": {}, "very": {}, "more": {}, "most": {}, "less": {},
	"some": {}, "any": {}, "all": {}, "each": {}, "every": {}, "other": {}, "another": {},
	"about": {}, "after": {}, "before": {}, "again": {}, "still": {}, "now": {},
	"up": {}, "down": {}, "off": {}, "on.": {},
	"my": {}, "your": {}, "our": {}, "his": {}, "her": {}, "their": {}, "its.": {},
}

// tokenize splits text into candidate glossary tokens. It preserves case
// (so "Gardenio" and "gardenio" count separately — we fold in a second pass
// only when building the glossary, to keep the TF decision explicit).
// Tokens that are pure punctuation, empty, or a single char that isn't a
// letter/digit are discarded.
func tokenize(s string) []string {
	if s == "" {
		return nil
	}
	parts := reTokenSplit.Split(s, -1)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		t := trimTokenEdges(p)
		if t == "" {
			continue
		}
		if !hasLetterOrDigit(t) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// trimTokenEdges strips leading/trailing dots, hyphens, slashes, asterisks,
// and similar markers that typically bracket tokens in markdown or prose.
func trimTokenEdges(s string) string {
	return strings.Trim(s, ".-_/*'`~#@$%^&+=")
}

func hasLetterOrDigit(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// termFreq counts lower-cased tokens, skipping stopwords and very short
// tokens (< 3 chars) that rarely carry dictation signal. Bead IDs are
// intentionally kept in their canonical case (they match reBeadID before
// the TF pass, so we can identify and preserve them).
type termFreq struct {
	total  int
	counts map[string]int
	// canonical maps lowercased key -> preferred display form (first seen
	// that isn't all-lower, else the lowered form).
	canonical map[string]string
}

func newTermFreq() *termFreq {
	return &termFreq{
		counts:    make(map[string]int, 4096),
		canonical: make(map[string]string, 4096),
	}
}

func (tf *termFreq) add(tok string) {
	low := strings.ToLower(tok)
	if len(low) < 3 {
		return
	}
	if _, stop := stopwords[low]; stop {
		return
	}
	tf.counts[low]++
	tf.total++
	// Prefer the first non-all-lower form we see (e.g. "Gardenio" over "gardenio").
	if existing, ok := tf.canonical[low]; !ok || existing == low {
		if existing == "" || (existing == low && tok != low) {
			tf.canonical[low] = tok
		}
	}
}

// topN returns the top-N terms by raw frequency, breaking ties lexicographically.
// The returned strings are canonical (case-preserved) forms.
func (tf *termFreq) topN(n int) []string {
	type kv struct {
		term  string
		count int
	}
	entries := make([]kv, 0, len(tf.counts))
	for k, v := range tf.counts {
		entries = append(entries, kv{k, v})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].count != entries[j].count {
			return entries[i].count > entries[j].count
		}
		return entries[i].term < entries[j].term
	})
	if n > len(entries) {
		n = len(entries)
	}
	out := make([]string, 0, n)
	for _, e := range entries[:n] {
		if canon, ok := tf.canonical[e.term]; ok && canon != "" {
			out = append(out, canon)
		} else {
			out = append(out, e.term)
		}
	}
	return out
}
