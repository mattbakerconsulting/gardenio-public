package obspii

import (
	_ "embed"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// embeddedCatalog is a verbatim copy of the canonical pii-gateway catalog at
// observability/pii-gateway/catalog.yaml (repo root). We embed it from
// testdata/ so the obspii tests never traverse out of the gardenio/controller
// module. Under a filtered checkout (gardenio/controller/** only) the
// repo-root observability/ tree is absent, which previously caused
// TestCatalog* to fail with ENOENT (gd-SvsMGuomfp).
//
// Keep testdata/catalog.yaml in sync with the canonical file;
// TestCatalogFixtureInSync guards this whenever the repo-root copy is present
// (i.e. when running against a full working tree).
//
//go:embed testdata/catalog.yaml
var embeddedCatalog []byte

// canonicalCatalogPath locates observability/pii-gateway/catalog.yaml from the
// repo root (four levels up from gardenio/controller/internal/obspii). It is
// used only by the in-sync guard test, which skips when the file is absent
// (e.g. under a filtered CI checkout).
func canonicalCatalogPath() string {
	return filepath.Join("..", "..", "..", "..", "observability", "pii-gateway", "catalog.yaml")
}

type catalogRule struct {
	ID          string `yaml:"id"`
	Description string `yaml:"description"`
	Class       string `yaml:"class"`
	Action      string `yaml:"action"`
	Match       struct {
		AttributeKey      string `yaml:"attribute_key"`
		AttributeKeyRegex string `yaml:"attribute_key_regex"`
		SpanName          string `yaml:"span_name"`
		LogBody           bool   `yaml:"log_body"`
		LogBodyRegex      string `yaml:"log_body_regex"`
		ResourceAttribute string `yaml:"resource_attribute"`
	} `yaml:"match"`
	AppliesTo struct {
		Services any      `yaml:"services"`
		Signals  []string `yaml:"signals"`
	} `yaml:"applies_to"`
}

type catalog struct {
	Version       int           `yaml:"version"`
	DefaultClass  string        `yaml:"default_class"`
	DefaultAction string        `yaml:"default_action"`
	Rules         []catalogRule `yaml:"rules"`
}

func loadCatalog(t *testing.T) catalog {
	t.Helper()
	var c catalog
	if err := yaml.Unmarshal(embeddedCatalog, &c); err != nil {
		t.Fatalf("parsing embedded pii-gateway catalog: %v", err)
	}
	return c
}

// TestCatalogFixtureInSync ensures the embedded testdata/catalog.yaml copy stays
// byte-identical to the canonical observability/pii-gateway/catalog.yaml at the
// repo root. It skips under a filtered CI checkout where the canonical file is
// absent — the embedded copy is authoritative for the other tests, and this
// guard catches drift whenever the full tree is available (local runs, full
// scheduled E2E).
func TestCatalogFixtureInSync(t *testing.T) {
	canonical, err := os.ReadFile(canonicalCatalogPath())
	if err != nil {
		if os.IsNotExist(err) {
			t.Skip("canonical observability/pii-gateway/catalog.yaml not present (filtered checkout) — skipping in-sync guard")
		}
		t.Fatalf("reading canonical pii-gateway catalog: %v", err)
	}
	if string(canonical) != string(embeddedCatalog) {
		t.Errorf("embedded testdata/catalog.yaml is out of sync with observability/pii-gateway/catalog.yaml; " +
			"re-copy it: cp observability/pii-gateway/catalog.yaml gardenio/controller/internal/obspii/testdata/catalog.yaml")
	}
}

var validClasses = map[string]struct{}{
	"phi-direct": {}, "pii-direct": {}, "pii-quasi": {},
	"auth-secret": {}, "infra-secret": {}, "free-text": {},
}

// classDefaultActions mirrors the doc §2.7 override matrix: which actions are
// allowed for each class.
var classAllowedActions = map[string]map[string]struct{}{
	"phi-direct":   {"drop": {}, "hash": {}},
	"pii-direct":   {"hash": {}, "drop": {}, "placeholder": {}},
	"pii-quasi":    {"truncate": {}, "drop": {}},
	"auth-secret":  {"drop": {}},
	"infra-secret": {"pass": {}, "hash": {}, "drop": {}},
	"free-text":    {"placeholder": {}, "drop": {}, "truncate": {}},
}

// TestCatalogGatewayRulePresent is acceptance criterion (1): the
// pii-direct.operator.email catch-all rule is present, classified pii-direct,
// action=hash, scoped to signals=[metrics], per obs-centralization-w2-pii.md §6.
func TestCatalogGatewayRulePresent(t *testing.T) {
	c := loadCatalog(t)

	var found *catalogRule
	for i := range c.Rules {
		if c.Rules[i].ID == "pii-direct.operator.email" {
			found = &c.Rules[i]
			break
		}
	}
	if found == nil {
		t.Fatal("catalog is missing required rule id=pii-direct.operator.email (W2 §6 catch-all)")
	}
	if found.Class != "pii-direct" {
		t.Errorf("pii-direct.operator.email: class=%q, want pii-direct", found.Class)
	}
	if found.Action != "hash" {
		t.Errorf("pii-direct.operator.email: action=%q, want hash", found.Action)
	}
	if len(found.AppliesTo.Signals) != 1 || found.AppliesTo.Signals[0] != "metrics" {
		t.Errorf("pii-direct.operator.email: signals=%v, want [metrics]", found.AppliesTo.Signals)
	}
	if found.Match.AttributeKeyRegex == "" && found.Match.AttributeKey == "" {
		t.Error("pii-direct.operator.email: match must select an attribute key")
	}
	// The catch-all regex must actually match an email-shaped operator label.
	if found.Match.AttributeKeyRegex != "" {
		re, err := regexp.Compile(found.Match.AttributeKeyRegex)
		if err != nil {
			t.Fatalf("pii-direct.operator.email: match regex does not compile: %v", err)
		}
		for _, key := range []string{"operator_email", "oidc_email", "user.email", "slack.user.email"} {
			if !re.MatchString(key) {
				t.Errorf("pii-direct.operator.email: regex %q should match operator label %q", found.Match.AttributeKeyRegex, key)
			}
		}
	}
}

// TestCatalogSchemaWellFormed validates the §6 schema invariants on every rule:
// unique ids, valid class, action allowed for the class, exactly one match
// selector, regexes compile, and a non-empty signals subset. This is the static
// equivalent of the boot-time fail-closed validator (doc §6.4).
func TestCatalogSchemaWellFormed(t *testing.T) {
	c := loadCatalog(t)

	if c.Version != 1 {
		t.Errorf("catalog version=%d, want 1", c.Version)
	}
	if _, ok := validClasses[c.DefaultClass]; !ok {
		t.Errorf("default_class=%q is not one of the six classes", c.DefaultClass)
	}
	if len(c.Rules) == 0 {
		t.Fatal("catalog has no rules")
	}

	validSignals := map[string]struct{}{"traces": {}, "logs": {}, "metrics": {}}
	seenIDs := map[string]struct{}{}

	for _, r := range c.Rules {
		if r.ID == "" {
			t.Error("rule with empty id")
			continue
		}
		if _, dup := seenIDs[r.ID]; dup {
			t.Errorf("duplicate rule id %q", r.ID)
		}
		seenIDs[r.ID] = struct{}{}

		if _, ok := validClasses[r.Class]; !ok {
			t.Errorf("rule %q: class %q is not one of the six classes", r.ID, r.Class)
		}
		if allowed, ok := classAllowedActions[r.Class]; ok {
			if _, ok := allowed[r.Action]; !ok {
				t.Errorf("rule %q: action %q not allowed for class %q (doc §2.7)", r.ID, r.Action, r.Class)
			}
		}

		// Exactly one match selector populated.
		selectors := 0
		if r.Match.AttributeKey != "" {
			selectors++
		}
		if r.Match.AttributeKeyRegex != "" {
			selectors++
			if _, err := regexp.Compile(r.Match.AttributeKeyRegex); err != nil {
				t.Errorf("rule %q: attribute_key_regex does not compile: %v", r.ID, err)
			}
		}
		if r.Match.SpanName != "" {
			selectors++
		}
		if r.Match.LogBody {
			selectors++
		}
		if r.Match.LogBodyRegex != "" {
			selectors++
		}
		if r.Match.ResourceAttribute != "" {
			selectors++
		}
		if selectors != 1 {
			t.Errorf("rule %q: must have exactly one match selector, found %d", r.ID, selectors)
		}

		if len(r.AppliesTo.Signals) == 0 {
			t.Errorf("rule %q: applies_to.signals must be non-empty", r.ID)
		}
		for _, s := range r.AppliesTo.Signals {
			if _, ok := validSignals[s]; !ok {
				t.Errorf("rule %q: invalid signal %q", r.ID, s)
			}
		}
	}
}
