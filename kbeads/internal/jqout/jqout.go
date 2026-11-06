// Package jqout applies an embedded jq filter (gojq) to a value about to be
// emitted as CLI JSON, so `kd --jq '<filter>' …` can project the server's JSON
// response in-process without piping to an external jq.
//
// This is the executable graduation of two settled advice beads:
//
//   - ka-0000002706 (fetch-once, project-many): one server call feeds many
//     projections from a single in-process filter, so agents stop re-running an
//     identical query for each jq projection.
//   - ka-0000003286 (bare-array vs envelope): when a filter indexes an envelope
//     key (.beads[]/.agents[]) against a bare-array response, Apply appends the
//     shape guidance right at the point of the mistake.
//
// An identical copy lives at gardenio/controller/internal/jqout so the `gb` CLI
// gets the same flag; the two modules are decoupled by design (gardenio/controller
// does not depend on kbeads).
package jqout

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/itchyny/gojq"
)

// Filter is a compiled jq program plus its source text (retained for hints).
type Filter struct {
	q   *gojq.Query
	src string
}

// Parse compiles a jq filter string. A compile error is wrapped with the
// original source so the caller can fail fast (e.g. in a pre-run hook) before
// any server round-trip.
func Parse(src string) (*Filter, error) {
	q, err := gojq.Parse(src)
	if err != nil {
		return nil, fmt.Errorf("invalid --jq filter %q: %w", src, err)
	}
	return &Filter{q: q, src: src}, nil
}

// Apply runs the filter over v and writes each result to w. v is first
// round-tripped through JSON so gojq sees plain map[string]any / []any /
// scalars rather than Go structs. Output follows `jq -r` semantics: string
// results print raw (unquoted), every other value prints as compact JSON, one
// result per line. The first runtime error (if any) is returned.
func (f *Filter) Apply(w io.Writer, v any) error {
	input, err := toPlain(v)
	if err != nil {
		return err
	}
	iter := f.q.Run(input)
	for {
		out, ok := iter.Next()
		if !ok {
			break
		}
		if err, ok := out.(error); ok {
			return f.wrapRuntimeErr(err, input)
		}
		if s, ok := out.(string); ok {
			fmt.Fprintln(w, s)
			continue
		}
		if err := writeCompact(w, out); err != nil {
			return fmt.Errorf("--jq: encoding result: %w", err)
		}
	}
	return nil
}

// writeCompact marshals a non-string jq result as compact JSON with HTML
// escaping OFF, matching real jq (which prints `<`, `>`, `&` literally rather
// than < etc.). json.Marshal would HTML-escape; a json.Encoder with
// SetEscapeHTML(false) does not, and it appends the trailing newline for us.
func writeCompact(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// toPlain converts an arbitrary Go value into the map[string]any / []any /
// scalar shape gojq requires, via a JSON round-trip.
func toPlain(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("--jq: encoding value: %w", err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("--jq: decoding value: %w", err)
	}
	return out, nil
}

// wrapRuntimeErr augments the classic "envelope key on a bare array" mistake
// (advice ka-0000003286) with a targeted hint. It only fires when the response
// is a top-level array AND the filter names a known envelope key, so a correct
// `gb news --jq '.agents[]'` (whose response is an object) is never flagged.
func (f *Filter) wrapRuntimeErr(err error, input any) error {
	if _, isArray := input.([]any); isArray && mentionsEnvelopeKey(f.src) {
		return fmt.Errorf("--jq: %w\n"+
			"hint: this output is a BARE JSON array — iterate it with `.[]` "+
			"(e.g. --jq '.[].id'), not an envelope key like `.beads[]`/`.agents[]`. "+
			"Envelope keys apply only to `gb news --json` and the HTTP /v1/beads API "+
			"(advice ka-0000003286).", err)
	}
	return fmt.Errorf("--jq: %w", err)
}

// mentionsEnvelopeKey reports whether src references one of the envelope keys
// that only exist on `gb news` / HTTP /v1/beads, not on the bare-array CLI
// list/ready/search responses.
func mentionsEnvelopeKey(src string) bool {
	for _, k := range []string{".beads", ".agents", ".in_progress", ".unassigned", ".stale", ".closed", ".open_decisions"} {
		if strings.Contains(src, k) {
			return true
		}
	}
	return false
}
