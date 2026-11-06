package landmap

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"gardenio/lands/contract"
	"gardenio/lands/contract/landsv1"
)

// The response denylist (D.8 gd-enMSApFne1, contract design gd-EWSKPH7pa5 §6
// "Response denylist"). The map runs it over every live answer it writes
// (writeBody, and each frame of the land feed) and counts what it finds in
// DenylistHits; hygiene_test.go runs the same scanner over every response the
// tests record.
//
// It is two lists in one:
//
//   - the contract's hygiene_denylist.txt (contract.HygieneDenylist), the same
//     file the contract's own hygiene test runs over the spec, fixtures and
//     bindings and D.11's strings tripwire (gd-oQdHmmwZFW) runs over the gl
//     binary. A match is a text hit: land agents write "bead" and "daemon" in
//     titles and comments, and the prose scrub does not remove plain words,
//     so text hits are expected and each one is reviewed.
//   - rules generated from the map's tables, which no static list can keep up
//     with: every bead type the contract has no word for, every daemon field
//     key that is not also a contract key, and every daemon route the map
//     calls. A match is a structural hit, a leak of the map's own, which must
//     stay at zero.

// DenylistHits counts denylist matches in live answers, by rule and kind. rule
// is the hygiene_denylist.txt line for a text hit, else field_key, field_word,
// bead_type, route or header; it is never the matched text.
var DenylistHits = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "gardenio_lands_denylist_hits_total",
	Help: "Denylist matches in live Lands API answers, by rule and kind (structural: a map leak; text: a hygiene_denylist.txt line in free text).",
}, []string{"rule", "kind"})

// The kinds of hit.
const (
	hitStructural = "structural"
	hitText       = "text"
)

// The generated rules' names.
const (
	ruleFieldKey  = "field_key"
	ruleFieldWord = "field_word"
	ruleBeadType  = "bead_type"
	ruleRoute     = "route"
	ruleHeader    = "header"
)

// denyHit is one match: its rule and kind (the counter's labels) and what
// matched, which only tests print.
type denyHit struct {
	rule, kind string
	what       string
}

func (h denyHit) String() string { return h.what }

type textRule struct {
	text string
	re   *regexp.Regexp
}

func (r textRule) match(s string) bool {
	if r.re != nil {
		return r.re.MatchString(s)
	}
	return strings.Contains(strings.ToLower(s), r.text)
}

type denylist struct {
	// text is the contract's list, run over every body line and header.
	text []textRule
	// keys are daemon field keys no response object may carry.
	keys map[string]bool
	// words are the keys distinctive enough (they hold an underscore) to be
	// refused anywhere in a body, prose included.
	words []string
	// types are bead type names no response string may be.
	types map[string]bool
	// routes are the daemon routes the map calls, refused anywhere in a body.
	routes []*regexp.Regexp
}

// liveDenylist is the denylist, built once.
var liveDenylist = sync.OnceValues(buildDenylist)

// CheckDenylist builds the response denylist. kd serve calls it before it
// listens, so a list that does not build stops the daemon rather than going
// unscanned on every answer.
func CheckDenylist() error {
	_, err := liveDenylist()
	return err
}

func buildDenylist() (denylist, error) {
	var d denylist
	sc := bufio.NewScanner(strings.NewReader(contract.HygieneDenylist))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if pat, ok := strings.CutPrefix(line, "re:"); ok {
			re, err := regexp.Compile(pat)
			if err != nil {
				return d, fmt.Errorf("hygiene_denylist.txt: %q: %w", line, err)
			}
			d.text = append(d.text, textRule{text: line, re: re})
			continue
		}
		d.text = append(d.text, textRule{text: strings.ToLower(line)})
	}
	if err := sc.Err(); err != nil {
		return d, err
	}
	if len(d.text) == 0 {
		return d, fmt.Errorf("hygiene_denylist.txt has no rules")
	}

	contractKeys := map[string]bool{}
	for _, v := range contractResponseTypes {
		contractKeysOf(reflect.TypeOf(v), contractKeys, map[reflect.Type]bool{})
	}
	// TypeRows covers every key beadRecord decodes (TestRecords_KeysAreInTables).
	d.keys = map[string]bool{"fields": true}
	daemonKeys := slices.Concat(commentKeys, meKeys)
	for _, r := range TypeRows {
		daemonKeys = slices.Concat(daemonKeys, r.Fields, r.Writes)
	}
	for _, k := range daemonKeys {
		k = strings.TrimPrefix(strings.TrimPrefix(k, "fields."), "lands.")
		if !contractKeys[k] {
			d.keys[k] = true
		}
	}
	for k := range d.keys {
		if strings.Contains(k, "_") {
			d.words = append(d.words, k)
		}
	}
	sort.Strings(d.words)

	vocabulary := map[string]bool{string(landsv1.Task): true, string(landsv1.Bug): true, string(landsv1.Feature): true}
	for _, r := range TypeRows {
		if r.Noun != "" {
			vocabulary[r.Noun] = true
		}
	}
	d.types = map[string]bool{}
	for _, r := range TypeRows {
		if !vocabulary[string(r.Type)] {
			d.types[strings.ToLower(string(r.Type))] = true
		}
	}

	seen := map[string]bool{}
	for _, rt := range routeTable {
		lit, _, _ := strings.Cut(rt.Pattern, "/{")
		if !seen[lit] {
			seen[lit] = true
			d.routes = append(d.routes, regexp.MustCompile(regexp.QuoteMeta(lit)+`\b`))
		}
	}
	return d, nil
}

// contractResponseTypes is every object the contract answers with.
var contractResponseTypes = []any{
	landsv1.Land{}, landsv1.Plot{}, landsv1.PlotPage{}, landsv1.Member{}, landsv1.MemberPage{},
	landsv1.Ticket{}, landsv1.TicketPage{}, landsv1.Comment{}, landsv1.Decision{}, landsv1.DecisionPage{},
	landsv1.Escalation{}, landsv1.EscalationPage{}, landsv1.Plan{}, landsv1.PlanPage{},
	landsv1.Thread{}, landsv1.ThreadPage{}, landsv1.Message{}, landsv1.MessagePage{},
	landsv1.Budget{}, landsv1.Event{}, landsv1.Problem{},
	landsv1.Agent{}, landsv1.AgentPage{}, landsv1.AgentGrant{},
}

// contractKeysOf adds every JSON key of t, and of the structs it holds, to into.
func contractKeysOf(t reflect.Type, into map[string]bool, seen map[reflect.Type]bool) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || seen[t] {
		return
	}
	seen[t] = true
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name != "" && name != "-" {
			into[name] = true
		}
		contractKeysOf(f.Type, into, seen)
	}
}

// contractEnumValue is true for a value of a closed contract enum whose words
// are also daemon bead types (DecisionArtifactType's report, epic, bundle):
// the enum fixes the value, so under its own key it cannot leak a type.
func contractEnumValue(key, value string) bool {
	return key == "artifact_type" && landsv1.DecisionArtifactType(value).Valid()
}

// mintedCorrelationID is a correlation_id exactly as newCorrelationID mints
// it: 12 random bytes in hex. Random hex can spell a denylisted token ("bead"
// in 4bead31d..., CI 39795791504), so scan masks it before matching. Only that
// exact shape is masked: anything else under the key is scanned as written.
var mintedCorrelationID = regexp.MustCompile(`"correlation_id":"[0-9a-f]{24}"`)

// mintedCredential is an AgentGrant's bearer or refresh exactly as the server
// mints them, 32 random bytes in hex, masked for the same reason.
var mintedCredential = regexp.MustCompile(`"(bearer|refresh)":"[0-9a-f]{64}"`)

// scan returns every denylisted thing in a response.
func (d denylist) scan(header http.Header, body string) []denyHit {
	var hits []denyHit
	body = mintedCorrelationID.ReplaceAllString(body, `"correlation_id":"minted"`)
	body = mintedCredential.ReplaceAllString(body, `"$1":"minted"`)
	for k, vs := range header {
		if !slices.Contains(ResponseHeaders, k) {
			hits = append(hits, denyHit{ruleHeader, hitStructural, fmt.Sprintf("header %s is not in ResponseHeaders", k)})
		}
		for _, v := range vs {
			for _, r := range d.text {
				if r.match(k + ": " + v) {
					hits = append(hits, denyHit{r.text, hitText, fmt.Sprintf("header %s: %q", k, r.text)})
				}
			}
		}
	}
	for _, line := range strings.Split(body, "\n") {
		for _, r := range d.text {
			if r.match(line) {
				hits = append(hits, denyHit{r.text, hitText, fmt.Sprintf("%q", r.text)})
			}
		}
	}
	for _, w := range d.words {
		if strings.Contains(body, w) {
			hits = append(hits, denyHit{ruleFieldWord, hitStructural, fmt.Sprintf("daemon field %q", w)})
		}
	}
	// The contract's own paths as the land door serves them (an evidence
	// picture's url, /land/<name>/v1/lands/v1/lands/<name>/...) begin with
	// apiPrefix, which no daemon route the map calls does; they are taken out
	// so /v1/lands (GET /v1/lands/{land}/members) is caught only on its own.
	routeText := strings.ReplaceAll(body, apiPrefix, "/")
	for _, re := range d.routes {
		if re.MatchString(routeText) {
			hits = append(hits, denyHit{ruleRoute, hitStructural, fmt.Sprintf("daemon route %q", re)})
		}
	}
	var v any
	if json.Unmarshal([]byte(body), &v) == nil {
		d.walk(v, &hits)
	}
	return hits
}

func (d denylist) walk(v any, hits *[]denyHit) {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if d.keys[k] {
				*hits = append(*hits, denyHit{ruleFieldKey, hitStructural, fmt.Sprintf("daemon field key %q", k)})
			}
			if s, ok := e.(string); ok && contractEnumValue(k, s) {
				continue
			}
			d.walk(e, hits)
		}
	case []any:
		for _, e := range x {
			d.walk(e, hits)
		}
	case string:
		if d.types[strings.ToLower(x)] {
			*hits = append(*hits, denyHit{ruleBeadType, hitStructural, fmt.Sprintf("bead type %q", x)})
		}
	}
}

// scanLive scans one live answer, counts each hit and logs it by rule, kind,
// op, land and correlation id (correlationID is "" for an answer that carries
// none). The answer itself is never logged.
func scanLive(t *tally, header http.Header, body []byte, correlationID string) {
	d, err := liveDenylist()
	if err != nil {
		return
	}
	for _, h := range d.scan(header, string(body)) {
		DenylistHits.WithLabelValues(h.rule, h.kind).Inc()
		op, land := t.opLand()
		slog.Warn("landmap: denylisted answer", "rule", h.rule, "kind", h.kind,
			"op", op, "land", land, "correlation_id", correlationID)
	}
}
