// Command build-dictation-corpus pulls text from every bead in the kbeads
// Postgres store (via the kbeads REST API), cleans it, and emits three
// artifacts into build/dictation/:
//
//	glossary.json  — top ~100 TF-weighted project terms for
//	                 SFSpeechRecognitionRequest.contextualStrings
//	phrases.yaml   — 1k–10k SFCustomLanguageModelData templates
//	corpus.txt     — full clean text, one chunk per line
//
// The tool is intentionally a single small Go binary with no transitive
// deps beyond the stdlib so a nightly regen is fast and hermetic.
//
// Scope lock: Apple Speech framework only — we emit artifacts that feed
// SFSpeechRecognizer + SFCustomLanguageModelData. No Whisper, no cloud
// STT, no third-party speech engines are invoked or referenced.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
)

type options struct {
	httpURL    string
	token      string
	fleet      string
	outDir     string
	glossaryN  int
	maxPhrases int
	workers    int
	pageSize   int
	identifier string
	dryRun     bool
	kind       string
	label      string
}

func parseFlags(args []string) (*options, error) {
	fs := flag.NewFlagSet("build-dictation-corpus", flag.ContinueOnError)
	o := &options{}
	fs.StringVar(&o.httpURL, "http-url", envOr("BEADS_HTTP_URL", "http://gardenio-beads.gardenio.svc.cluster.local:8080"), "kbeads HTTP base URL")
	fs.StringVar(&o.token, "token", os.Getenv("BEADS_TOKEN"), "bearer token (optional; defaults to $BEADS_TOKEN)")
	fs.StringVar(&o.fleet, "fleet", envOr("BEADS_FLEET", ""), "fleet override (X-Beads-Fleet header)")
	fs.StringVar(&o.outDir, "out", "build/dictation", "output directory for artifacts")
	fs.IntVar(&o.glossaryN, "glossary-size", 100, "number of top TF terms to keep in glossary.json")
	fs.IntVar(&o.maxPhrases, "max-phrases", 10000, "cap on phrases emitted to phrases.yaml")
	fs.IntVar(&o.workers, "workers", 16, "parallel comment-fetch workers")
	fs.IntVar(&o.pageSize, "page-size", 500, "bead list page size")
	fs.StringVar(&o.identifier, "identifier", "ai.acme.gardenio.dictation", "SFCustomLanguageModelData identifier")
	fs.BoolVar(&o.dryRun, "dry-run", false, "fetch and process but do not write artifacts")
	fs.StringVar(&o.kind, "kind", envOr("BEADS_KIND", ""), "only fetch beads of this kind (e.g. issue); empty = all kinds")
	fs.StringVar(&o.label, "label", envOr("BEADS_LABEL", ""), "only fetch beads carrying this label (e.g. project:gardenio)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if o.glossaryN <= 0 {
		return nil, fmt.Errorf("-glossary-size must be > 0")
	}
	if o.maxPhrases <= 0 {
		return nil, fmt.Errorf("-max-phrases must be > 0")
	}
	if o.workers <= 0 {
		o.workers = 1
	}
	return o, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	opts, err := parseFlags(os.Args[1:])
	if err != nil {
		if err == flag.ErrHelp {
			os.Exit(0)
		}
		log.Fatalf("flags: %v", err)
	}
	if err := run(context.Background(), opts); err != nil {
		log.Fatalf("build-dictation-corpus: %v", err)
	}
}

func run(ctx context.Context, opts *options) error {
	client := newBeadsClient(opts.httpURL, opts.token, opts.fleet)
	log.Printf("fetching beads from %s", opts.httpURL)

	beads, err := client.listAllBeads(ctx, opts.pageSize, opts.kind, opts.label)
	if err != nil {
		return fmt.Errorf("list beads: %w", err)
	}
	log.Printf("fetched %d beads", len(beads))

	// Fetch comments in parallel. A single bead's comment fetch failing is
	// not fatal — we log and skip. The corpus is best-effort aggregate.
	commentsByBead := fetchAllComments(ctx, client, beads, opts.workers)

	chunks, ids := buildChunks(beads, commentsByBead)
	log.Printf("assembled %d text chunks, %d unique bead ids", len(chunks), len(ids))

	// Tokenize + compute TF for the glossary.
	tf := newTermFreq()
	for _, c := range chunks {
		for _, tok := range tokenize(c) {
			// Bead IDs are handled separately via spelling templates; they
			// would otherwise dominate the glossary with noise.
			if reBeadID.MatchString(tok) {
				continue
			}
			tf.add(tok)
		}
	}
	glossary := tf.topN(opts.glossaryN)

	// Phrases: start with spelling templates for every ID in corpus
	// (highest signal for dictation), then fill up to maxPhrases with
	// real in-corpus sentences that contain at least one glossary term.
	phrases := buildPhrases(chunks, ids, glossary, opts.maxPhrases)

	// Custom words = glossary terms. The builder uses these as a
	// vocabulary boost independent of phrase templates.
	customWords := append([]string{}, glossary...)

	if opts.dryRun {
		log.Printf("[dry-run] glossary=%d phrases=%d custom_words=%d", len(glossary), len(phrases), len(customWords))
		return nil
	}

	if err := os.MkdirAll(opts.outDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", opts.outDir, err)
	}
	if err := writeGlossary(opts.outDir, glossary); err != nil {
		return fmt.Errorf("write glossary: %w", err)
	}
	if err := writePhrases(opts.outDir, phrasesDoc{
		Identifier:  opts.identifier,
		CustomWords: customWords,
		Phrases:     phrases,
	}); err != nil {
		return fmt.Errorf("write phrases: %w", err)
	}
	if err := writeCorpus(opts.outDir, chunks); err != nil {
		return fmt.Errorf("write corpus: %w", err)
	}

	log.Printf("wrote %s/{%s,%s,%s} glossary=%d phrases=%d",
		opts.outDir, artifactGlossary, artifactPhrases, artifactCorpus,
		len(glossary), len(phrases))
	return nil
}

// fetchAllComments fans out comment fetches across `workers` goroutines.
// Failures are logged and the bead is left with no comments. Returns a
// map bead-id -> []*comment.
func fetchAllComments(ctx context.Context, c *beadsClient, beads []*bead, workers int) map[string][]*comment {
	out := make(map[string][]*comment, len(beads))
	var mu sync.Mutex

	type job struct{ b *bead }
	jobs := make(chan job)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				comments, err := c.getComments(ctx, j.b.ID)
				if err != nil {
					log.Printf("warn: comments %s: %v", j.b.ID, err)
					continue
				}
				if len(comments) == 0 {
					continue
				}
				mu.Lock()
				out[j.b.ID] = comments
				mu.Unlock()
			}
		}()
	}
	for _, b := range beads {
		jobs <- job{b: b}
	}
	close(jobs)
	wg.Wait()
	return out
}

// buildChunks extracts every useful text fragment from a bead and its
// comments, cleans each fragment, and returns the list of clean chunks
// plus the union of bead IDs referenced anywhere in them.
//
// Sources per bead (per the epic plan):
//
//	title, description, notes, comments,
//	decision/report string fields (prompt, rationale, response_text,
//	content, context, report_type),
//	agent bead "prompt" and "wrapup" fields (these often carry MR
//	descriptions and crew instructions).
func buildChunks(beads []*bead, comments map[string][]*comment) ([]string, []string) {
	chunks := make([]string, 0, len(beads)*2)
	idSet := make(map[string]struct{})

	add := func(s string) {
		c := cleanText(s)
		if c == "" {
			return
		}
		chunks = append(chunks, c)
		for _, id := range findBeadIDs(c) {
			idSet[id] = struct{}{}
		}
	}

	// String-valued fields we lift from the Fields JSON blob. These keys
	// intentionally cover decision prompts/reports AND agent prompts
	// (agent beads carry the operator's task description, which tends to
	// be the highest-signal natural-language source in the whole store).
	stringFields := []string{
		"prompt",
		"rationale",
		"response_text",
		"content",
		"context",
		"report_type",
		"wrapup",
		"close_reason",
		"schedule_title",
	}

	for _, b := range beads {
		// Record every bead's own id so dictation has coverage even for
		// ids that aren't cross-referenced by any other bead text.
		if reBeadID.MatchString(b.ID) {
			idSet[b.ID] = struct{}{}
		}
		add(b.Title)
		add(b.Description)
		add(b.Notes)
		if len(b.Fields) > 0 {
			var fm map[string]any
			if err := json.Unmarshal(b.Fields, &fm); err == nil {
				for _, k := range stringFields {
					if v, ok := fm[k].(string); ok && v != "" {
						add(v)
					}
				}
			}
		}
		for _, cm := range comments[b.ID] {
			add(cm.Text)
		}
	}

	ids := make([]string, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return chunks, ids
}

// buildPhrases combines ID-spelling templates with in-corpus sentences,
// capped at maxPhrases. Spelling templates come first (highest priority
// for the custom LM), then short sentences that contain a glossary term.
func buildPhrases(chunks, ids, glossary []string, maxPhrases int) []string {
	out := make([]string, 0, maxPhrases)
	seen := make(map[string]struct{}, maxPhrases)

	appendOnce := func(p string) bool {
		p = strings.TrimSpace(p)
		if p == "" {
			return false
		}
		if _, ok := seen[p]; ok {
			return false
		}
		seen[p] = struct{}{}
		out = append(out, p)
		return len(out) >= maxPhrases
	}

	// 1) spelling templates for every referenced bead id
	for _, id := range ids {
		for _, tpl := range spellingTemplates(id) {
			if appendOnce(tpl) {
				return out
			}
		}
	}

	// 2) in-corpus sentences that mention a glossary term — these teach
	// the LM real n-grams the team actually uses.
	glossarySet := make(map[string]struct{}, len(glossary))
	for _, g := range glossary {
		glossarySet[strings.ToLower(g)] = struct{}{}
	}
	for _, c := range chunks {
		for _, sent := range splitSentences(c) {
			if !containsAnyFold(sent, glossarySet) {
				continue
			}
			if len(sent) < 8 || len(sent) > 240 {
				continue
			}
			if appendOnce(sent) {
				return out
			}
		}
	}
	return out
}

// splitSentences is a cheap sentence splitter: cut on . ! ? followed by
// whitespace or end-of-string. Good enough for corpus sampling; we are
// not doing grammar.
func splitSentences(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '.' || c == '!' || c == '?' || c == '\n' {
			if i > start {
				out = append(out, strings.TrimSpace(s[start:i]))
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, strings.TrimSpace(s[start:]))
	}
	return out
}

// containsAnyFold reports whether s (case-insensitive) contains any key
// in needles. Keys in needles must already be lowercased.
func containsAnyFold(s string, needles map[string]struct{}) bool {
	low := strings.ToLower(s)
	for n := range needles {
		if strings.Contains(low, n) {
			return true
		}
	}
	return false
}
