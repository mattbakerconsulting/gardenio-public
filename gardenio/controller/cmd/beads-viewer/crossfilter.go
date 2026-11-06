package main

// Phase 8 — cross-filter v2 wire format.
// Plan-of-record: docs/research/incident-intelligence/synthesis.md §Phase 8.
// Bead: ka-0000007379.
//
// The cross-filter is a small set of (key, value) pairs that the user
// composes via filter-chip strips on widgets. The wire format is the same on
// both ends:
//
//   service:study-web,severity:p1,source-kind:mezmo
//
// — comma-separated `<key>:<value>` pairs, percent-encoded when carrying
// special characters. The full string lives in the URL fragment as
// `#ii-f=<encoded>` so shared links round-trip; htmx polls forward it as a
// `?filter=<encoded>` query parameter so the server can apply it where it
// makes sense (event-timeline so far).
//
// Server-side parsing is intentionally permissive: malformed pairs are
// dropped, empty strings are no-ops, duplicate keys keep both values (so a
// user can filter on two services at once without an explicit OR syntax).

import (
	"encoding/json"
	"net/url"
	"sort"
	"strings"
)

// crossFilter is the parsed form of the URL parameter. Keys map to a sorted
// list of values; the "any-of" semantics matches the chip-strip UX.
type crossFilter map[string][]string

// parseCrossFilter decodes the comma-separated wire format into a
// crossFilter. Empty / invalid input returns nil so callers can short-circuit
// with `if filter == nil`.
//
// It performs ONE percent-decode pass, for callers that hold a still-encoded
// wire string (e.g. a value lifted verbatim out of the URL fragment). Callers
// that already hold a decoded value — anything read through
// r.URL.Query().Get, which Go decodes for you — must use
// parseCrossFilterDecoded instead, or a value containing '+'/'%' is decoded a
// second time and corrupted (ka-0000005153 review finding).
func parseCrossFilter(raw string) crossFilter {
	if dec, err := url.QueryUnescape(strings.TrimSpace(raw)); err == nil {
		raw = dec
	}
	return parseCrossFilterDecoded(raw)
}

// parseCrossFilterDecoded parses an ALREADY-DECODED wire string into a
// crossFilter without a further percent-decode. Use this for values read via
// r.URL.Query().Get (Go has decoded them once already) so facet values
// containing '+' (e.g. assignee "bot+ci") or '%' survive the round trip.
func parseCrossFilterDecoded(raw string) crossFilter {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	out := crossFilter{}
	for part := range strings.SplitSeq(raw, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), ":", 2)
		if len(kv) != 2 {
			continue
		}
		k, v := strings.TrimSpace(kv[0]), strings.TrimSpace(kv[1])
		if k == "" || v == "" {
			continue
		}
		out[k] = append(out[k], v)
	}
	if len(out) == 0 {
		return nil
	}
	for k := range out {
		sort.Strings(out[k])
		out[k] = dedupeStrings(out[k])
	}
	return out
}

// dedupeStrings removes adjacent duplicates from a sorted slice.
func dedupeStrings(in []string) []string {
	if len(in) < 2 {
		return in
	}
	out := in[:1]
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}

// String renders the filter back to wire format with stable key order so two
// equivalent filters always round-trip to the same string.
func (f crossFilter) String() string {
	if len(f) == 0 {
		return ""
	}
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		for _, v := range f[k] {
			parts = append(parts, k+":"+v)
		}
	}
	return strings.Join(parts, ",")
}

// matchesAny reports whether the filter matches any of the candidate values
// for a given key. Empty filter or absent key → always true (no filter).
func (f crossFilter) matchesAny(key string, candidates ...string) bool {
	if len(f) == 0 {
		return true
	}
	want, ok := f[key]
	if !ok || len(want) == 0 {
		return true
	}
	for _, c := range candidates {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		for _, w := range want {
			if strings.EqualFold(c, w) {
				return true
			}
		}
	}
	return false
}

// applyServerCrossFilter is the hook handleLiveWidget calls when a widget is
// `ServerFilters=true`. Currently only event-timeline gets server-side
// filtering — the others filter client-side. Returns the (possibly
// re-encoded) JSON payload.
func applyServerCrossFilter(rawPayload, slug, filterParam string) string {
	if rawPayload == "" || filterParam == "" {
		return rawPayload
	}
	f := parseCrossFilter(filterParam)
	if len(f) == 0 {
		return rawPayload
	}
	switch slug {
	case "event-timeline":
		return filterTimelinePayload(rawPayload, f)
	default:
		return rawPayload
	}
}

// filterTimelinePayload drops events that fail the source-kind / service /
// severity match. Keys recognised:
//
//	source-kind: mezmo|gitlab|jira|...
//	service:     study-web|sponsor-api|...   (matches event.service or event.title prefix)
//	severity:    p1|p2|p3
//
// Unknown keys are ignored.
func filterTimelinePayload(rawPayload string, f crossFilter) string {
	var p map[string]any
	if err := json.Unmarshal([]byte(rawPayload), &p); err != nil {
		return rawPayload
	}
	rawEvents, _ := p["events"].([]any)
	if len(rawEvents) == 0 {
		return rawPayload
	}
	kept := make([]any, 0, len(rawEvents))
	for _, raw := range rawEvents {
		ev, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if !f.matchesAny("source-kind", asString(ev["kind"])) {
			continue
		}
		if !f.matchesAny("service", asString(ev["service"])) {
			continue
		}
		if !f.matchesAny("severity", asString(ev["severity"])) {
			continue
		}
		kept = append(kept, ev)
	}
	p["events"] = kept
	out, err := json.Marshal(p)
	if err != nil {
		return rawPayload
	}
	return string(out)
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
