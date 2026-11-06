package planwindow

import (
	"bufio"
	_ "embed"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// WithheldText replaces a field the hygiene check withholds. The page renders
// it as is; the owner's preview marks it.
const WithheldText = "withheld: contains an internal reference"

// RuleEmail is the rule name of an email-address hit.
const RuleEmail = "email"

// The denylist is lands/contract/hygiene_denylist.txt (PW-D5: the Window reuses
// the contract's vocabulary and denylist). This is a copy because the
// controller images build from gardenio/ alone and cannot reach lands/;
// TestDenylistMatchesContract fails the build the moment the two differ.
//
//go:embed hygiene_denylist.txt
var denylistText string

// emailPattern matches an email address anywhere in a string (the same
// pattern internal/obspii scrubs logs with). Human names and emails never
// leave the daemon (PW-D4); this catches one a human typed into a title or the
// narrative.
var emailPattern = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

// WithheldTotal counts fields the hygiene check withheld, by field and rule.
// field is a Field* name; rule is the denylist line that hit, or RuleEmail.
// Neither is ever the withheld text. Not registered here: the viewer registers
// it on its own registry (PW.3).
var WithheldTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "gardenio_window_withheld_total",
	Help: "Window fields withheld at render time because they matched the Lands hygiene denylist or an email address, by field and rule.",
}, []string{"field", "rule"})

// The fields hygiene runs over, as WithheldTotal's field label and Withheld's
// Field.
const (
	FieldPlanTitle      = "plan_title"
	FieldPhaseTitle     = "phase_title"
	FieldStepTitle      = "step_title"
	FieldDecisionTitle  = "decision_title"
	FieldDecisionChosen = "decision_chosen"
	FieldAgentName      = "agent_name"
	FieldNarrative      = "narrative"
	FieldRecentTitle    = "recent_title"
)

type denyRule struct {
	line string
	sub  string
	re   *regexp.Regexp
}

// rules is the parsed denylist, built once. It is embedded, so
// TestDenylistParses catches a list that does not parse before any binary
// ships; if one ships anyway, Check fails closed.
var rules = sync.OnceValues(func() ([]denyRule, error) { return parseDenylist(denylistText) })

// RuleUnreadable is the rule name Check reports for every non-empty string
// when the embedded denylist does not parse: a page withholds everything
// rather than show text it could not check.
const RuleUnreadable = "denylist_unreadable"

// parseDenylist reads hygiene_denylist.txt the way the contract and the map
// do: a plain line is a case-insensitive substring, a `re:` line a Go regular
// expression, and blank lines and # comments are skipped.
func parseDenylist(text string) ([]denyRule, error) {
	var out []denyRule
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if pat, ok := strings.CutPrefix(line, "re:"); ok {
			re, err := regexp.Compile(pat)
			if err != nil {
				return nil, fmt.Errorf("planwindow: hygiene_denylist.txt: %q: %w", line, err)
			}
			out = append(out, denyRule{line: line, re: re})
			continue
		}
		out = append(out, denyRule{line: line, sub: strings.ToLower(line)})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("planwindow: hygiene_denylist.txt has no rules")
	}
	return out, nil
}

// Check reports the first rule s breaks: a denylist line, or RuleEmail. ok is
// false for clean text.
func Check(s string) (rule string, ok bool) {
	if s == "" {
		return "", false
	}
	rs, err := rules()
	if err != nil {
		return RuleUnreadable, true
	}
	lower := strings.ToLower(s)
	for _, r := range rs {
		if r.re != nil {
			if r.re.MatchString(s) {
				return r.line, true
			}
			continue
		}
		if strings.Contains(lower, r.sub) {
			return r.line, true
		}
	}
	if emailPattern.MatchString(s) {
		return RuleEmail, true
	}
	return "", false
}

// Withheld is one field the hygiene check withheld from a View. Path names
// the field positionally (phases[1].steps[0].title) so the owner's preview can
// mark it; it never holds the withheld text.
type Withheld struct {
	Path  string
	Field string
	Rule  string
}

// scrub is one Build's hygiene pass: it withholds each field that breaks a
// rule, counts it, and records it for the owner's preview.
type scrub struct {
	withheld []Withheld
}

func (sc *scrub) text(field, path, s string) string {
	rule, hit := Check(s)
	if !hit {
		return s
	}
	WithheldTotal.WithLabelValues(field, rule).Inc()
	sc.withheld = append(sc.withheld, Withheld{Path: path, Field: field, Rule: rule})
	return WithheldText
}
