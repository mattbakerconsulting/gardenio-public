package tests

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Tenant land credit series (plan gd-gh5J0vVbJP step 3.2, gd-DxFIK7tips):
// vmalert recording rules that record each tenant land's spend, burn rate and
// remaining in credits, as the ConfigMap key lands-credits.yaml
// (templates/_lands-credits.tpl).

var landCreditRecords = []string{
	"gardenio:land_spend_credits:total",
	"gardenio:land_spend_credits:today",
	"gardenio:land_spend_credits:rate1h",
	"gardenio:land_spend_credits:rate1d",
	"gardenio:land_spend_credits:increase7d",
	"gardenio:land_spend_credits:increase30d",
	"gardenio:land_spend_credits:by_agent",
	"gardenio:land_credits:granted",
	"gardenio:land_credits:remaining",
}

// On by default wherever vmalert runs, notifier or not: recording rules only.
func TestLandsCredits_RendersByDefaultWithoutNotifier(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml",
		"vmalert.enabled=true",
		"alertmanager.enabled=false",
		"vmalert.notifierURL=",
		"vmalert.notifierBlackhole=false",
	)
	if !strings.Contains(out, "lands-credits.yaml: |") {
		t.Fatalf("vmalert ConfigMap must carry the lands-credits key by default, got:\n%s", out)
	}
	for _, r := range landCreditRecords {
		if !strings.Contains(out, "record: "+r+"\n") {
			t.Errorf("record %s missing", r)
		}
	}
	for _, want := range []string{
		"name: gardenio.lands_credits\n",
		"name: gardenio.lands_credits.long\n",
		`gardenio_account_cost_usd{account=~"lands-.+"}`,
		"max by (land) (gardenio_lands_credit_rate)",
		"max by (land) (gardenio_lands_credits_granted)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("%q missing", want)
		}
	}
}

func TestLandsCredits_Disabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml",
		"vmalert.enabled=true",
		"vmalert.landCredits.enabled=false",
	)
	if strings.Contains(out, "lands-credits.yaml") || strings.Contains(out, "land_spend_credits") {
		t.Errorf("landCredits.enabled=false must render no credit rules, got:\n%s", out)
	}
}

// No series the rules record is in dollars: the tenant charts read only these,
// so a USD-named record here would be a USD figure one query away from a tenant.
// Every record is keyed by land, and every expression multiplies by the rate
// (spend) or reads the ledger (granted, remaining).
func TestLandsCredits_RecordsOnlyCredits(t *testing.T) {
	t.Parallel()
	key := landCreditsKey(t)
	records := regexp.MustCompile(`record: (\S+)`).FindAllStringSubmatch(key, -1)
	if len(records) != len(landCreditRecords) {
		t.Errorf("%d records, want %d: %v", len(records), len(landCreditRecords), records)
	}
	for _, m := range records {
		name := m[1]
		if strings.Contains(strings.ToLower(name), "usd") || !strings.Contains(name, "credits") {
			t.Errorf("record %s: every credit record names credits and never usd", name)
		}
		if !strings.HasPrefix(name, "gardenio:land_spend_credits:") && !strings.HasPrefix(name, "gardenio:land_credits:") {
			t.Errorf("record %s is outside the two families the hub reads", name)
		}
	}
	for _, rule := range strings.Split(key, "- record: ")[1:] {
		name := strings.SplitN(rule, "\n", 2)[0]
		if name != "gardenio:land_credits:granted" && !strings.Contains(rule, "* on (land)") {
			t.Errorf("record %s must multiply its USD by the land's credit rate", name)
		}
	}
}

func landCreditsKey(t *testing.T) string {
	t.Helper()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", "vmalert.enabled=true")
	i := strings.Index(out, "lands-credits.yaml: |")
	if i < 0 {
		t.Fatalf("lands-credits key missing, got:\n%s", out)
	}
	key := out[i+len("lands-credits.yaml: |"):]
	// The key's body is indented past the data map; the next key or document
	// ends it.
	if end := regexp.MustCompile(`\n(  \S|---)`).FindStringIndex(key); end != nil {
		key = key[:end[0]]
	}
	return key
}

// renderLandCreditsRules writes the lands-credits.yaml key, as vmalert loads
// it, to a temp file and checks it with promtool. Skips without helm, yq or
// promtool on PATH.
func renderLandCreditsRules(t *testing.T) string {
	t.Helper()
	for _, bin := range []string{"helm", "yq", "promtool"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH; skipping promtool proof", bin)
		}
	}
	path := filepath.Join(t.TempDir(), "lands_credits_rules.yaml")
	render := exec.Command("bash", "-c",
		"helm template test "+chartDir(t)+" --show-only templates/vmalert/configmap.yaml "+
			"--set vmalert.enabled=true | yq '.data.\"lands-credits.yaml\"'")
	out, err := render.CombinedOutput()
	if err != nil {
		t.Fatalf("render rules failed: %v\n%s", err, out)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
	if res, err := exec.Command("promtool", "check", "rules", path).CombinedOutput(); err != nil {
		t.Fatalf("promtool check rules failed: %v\n%s", err, res)
	}
	return path
}

func promtoolTestRules(t *testing.T, name, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if res, err := exec.Command("promtool", "test", "rules", path).CombinedOutput(); err != nil {
		t.Fatalf("promtool test rules failed: %v\n%s", err, res)
	}
}

// TestLandsCredits_Rules runs lands_credits_rules_test.yaml: spent, remaining,
// today across midnight UTC, the trailing windows, a scrape gap, a reset, a
// rate change, per-agent credits, and no series for an operator account or a
// land with no rate in force.
func TestLandsCredits_Rules(t *testing.T) {
	t.Parallel()
	rules := renderLandCreditsRules(t)
	tmpl, err := os.ReadFile(filepath.Join(chartDir(t), "tests", "lands_credits_rules_test.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	promtoolTestRules(t, "lands_credits_test.yaml", strings.ReplaceAll(string(tmpl), "__RULES_FILE__", rules))
}

// TestLandsCredits_PurseCrossCheck holds the rules to the purse: for the farm
// in internal/lands/purse/testdata/credits_crosscheck.json, the recorded
// spent, granted and remaining equal what GET /api/lands/{land}/purse returns
// for it (cmd/beads-viewer TestLandPurseAPI_CreditsCrossCheck asserts the same
// wants against the API, internal/metrics TestCollectLandCredits_CrossCheck
// the gauges these inputs stand for).
func TestLandsCredits_PurseCrossCheck(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../../controller/internal/lands/purse/testdata/credits_crosscheck.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		LLMMultiplier float64 `json:"llm_multiplier"`
		Lands         []struct {
			Land         string    `json:"land"`
			Grants       []float64 `json:"grants"`
			TotalCostUSD float64   `json:"total_cost_usd"`
			Want         struct {
				GrantedCredits   float64 `json:"granted_credits"`
				SpentCredits     float64 `json:"spent_credits"`
				RemainingCredits float64 `json:"remaining_credits"`
			} `json:"want"`
		} `json:"lands"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if len(fx.Lands) == 0 {
		t.Fatal("cross-check fixture has no lands")
	}
	rules := renderLandCreditsRules(t)

	var in, exp strings.Builder
	series := func(sel string, v float64) {
		fmt.Fprintf(&in, "      - series: '%s'\n        values: '%gx10'\n", sel, v)
	}
	sample := func(record, land string, v float64) {
		fmt.Fprintf(&exp, "      - expr: %s{land=%q}\n        eval_time: 10m\n        exp_samples:\n          - labels: '%s{land=%q}'\n            value: %g\n", record, land, record, land, v)
	}
	for _, l := range fx.Lands {
		var granted float64
		for _, g := range l.Grants {
			granted += g // purse.SumCredits, what the controller exports
		}
		series(fmt.Sprintf(`gardenio_account_cost_usd{account="lands-%s",env="prod"}`, l.Land), l.TotalCostUSD)
		series(fmt.Sprintf(`gardenio_lands_credit_rate{land=%q}`, l.Land), fx.LLMMultiplier)
		series(fmt.Sprintf(`gardenio_lands_credits_granted{land=%q}`, l.Land), granted)
		sample("gardenio:land_spend_credits:total", l.Land, l.Want.SpentCredits)
		sample("gardenio:land_credits:granted", l.Land, l.Want.GrantedCredits)
		sample("gardenio:land_credits:remaining", l.Land, l.Want.RemainingCredits)
	}
	body := fmt.Sprintf("rule_files:\n  - %s\nevaluation_interval: 1m\ntests:\n  - name: purse_cross_check\n    interval: 1m\n    input_series:\n%s    promql_expr_test:\n%s",
		rules, in.String(), exp.String())
	promtoolTestRules(t, "lands_credits_purse_test.yaml", body)
}
