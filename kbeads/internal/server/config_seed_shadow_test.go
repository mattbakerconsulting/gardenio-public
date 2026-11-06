package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// This is the durable guard for ka-0000003677 (generalized in ka-0000000407): it
// stops the ENTIRE e2e shell suite — every script under
// gardenio/tests/e2e/scripts/*.sh, not just the shared seed — from silently
// DRIFTING behind, and thereby SHADOWING, the daemon builtin schemas.
//
// ── The failure mode it guards ───────────────────────────────────────────────
// An e2e script that does
//
//	curl -X PUT "$BEADS_HTTP_URL/v1/configs/type:<T>" -d '{"value":{"fields":[…]}}'
//
// for a type <T> that is AUTHORITATIVE in builtinConfigs (config.go) can only
// shadow the builtin:
//
//   - resolveTypeConfig (the create/update VALIDATION path) merges builtin fields
//     that are MISSING from a stored config, so an omitted OPTIONAL field still
//     round-trips — but it does NOT reconcile a stored field whose required/type/
//     enum constraint is WEAKER than the builtin's (the store row wins there).
//   - listConfigsWithBuiltins / GetConfig / `kd fields` (the READ path) return the
//     stored row verbatim with NO merge at all — so `kd fields <type>` in the e2e
//     namespace reports the drifted shape, not the builtin.
//
// Either way the ephemeral e2e namespace tests a schema prod does NOT run:
// false-green in both directions (a passing round-trip that never exercised the
// real field, or a real regression the weakened constraint hides). This is the
// exact shadowing that masked ka-0000000585 in prod.
//
// ka-0000003677 fixed this for ONE file (seed-types-and-users.sh) and guarded it
// with a single hard-coded path. But 8 OTHER scripts PUT builtin-authoritative
// type configs the same way — test-trigger-system.sh alone dropped 17 builtin
// type:trigger fields (incl. the `gardener` blob the whole gardener-authoring flow
// depends on). A per-file const cannot see them. This guard GLOBS the whole
// scripts dir so a shadow PUT in any e2e module reds the exact same way.
//
// ── The fix and this guard ───────────────────────────────────────────────────
// Every offending PUT was dropped so e2e exercises the builtin the way prod does.
// This test keeps that true: it parses every `-X PUT .../v1/configs/type:<T>`
// request's `-d '…'` body across all e2e scripts and, for each <T> the builtin
// owns, asserts the stored schema is a NON-WEAKENING SUPERSET of the builtin
// (every builtin field present, same type, required at least as strict, enum
// values a superset). With no PUTs left it passes vacuously — but the instant
// anyone RE-ADDS a drifted PUT (the recurrence this bug is about), in ANY script,
// it fails naming the exact script, field, and remedy.
//
// The scripts live in a sibling path (gardenio/tests/e2e/) reached by relative
// path. On a partial checkout without that tree the glob matches nothing and the
// test SKIPS (mirroring config_driftguard_test.go). CI checks out the whole
// monorepo, so the guard always runs there.

// e2eScriptsGlobRelPath globs every e2e shell module (including the shared
// seed-types-and-users.sh) from the package dir (kbeads/internal/server).
const e2eScriptsGlobRelPath = "../../../gardenio/tests/e2e/scripts/*.sh"

var (
	// curlStartRe marks the start of a curl invocation (`curl` or the
	// `beads_curl` wrapper). We split each script on these boundaries so every
	// chunk is exactly one HTTP request — one URL, at most one `-d` body — which
	// lets us tie a `-d` body to the method + URL of its OWN request and never a
	// neighbour's. `\b` before the optional `beads_` prefix means the `curl`
	// inside `beads_curl` is not separately matched (no double-split).
	curlStartRe = regexp.MustCompile(`\b(?:beads_)?curl\b`)

	// configPutURLRe extracts the type name from a `/v1/configs/type:<name>`
	// request path. The path is the AUTHORITATIVE key (the request target), so we
	// key off it rather than the body's optional "key" field — some bodies omit
	// it (`{"value":{…}}`) and rely on the URL.
	configPutURLRe = regexp.MustCompile(`/v1/configs/type:([a-z_-]+)`)

	// curlBodyRe captures a `-d '<body>'` single-quoted argument. The bodies are
	// single-quoted in the shell scripts and never themselves contain a single
	// quote, so `[^']*` captures the whole body exactly — including newlines, so
	// multi-line pretty-printed JSON bodies parse too (verified across all
	// scripts: every config `-d` body is single-quote-free and valid JSON).
	curlBodyRe = regexp.MustCompile(`-d '([^']*)'`)
)

// e2eConfigPut is one `-X PUT /v1/configs/type:<T>` request found in an e2e
// script, carrying the source basename for provenance in failure messages.
type e2eConfigPut struct {
	script string // basename, e.g. "test-trigger-system.sh"
	key    string // "type:trigger", derived from the URL path
	tc     *model.TypeConfig
}

// extractE2EConfigPuts globs every e2e script and returns each type-config PUT it
// issues, plus the number of scripts scanned. Returns (nil, 0) — and skips the
// test — when the sibling scripts tree is not checked out.
func extractE2EConfigPuts(t *testing.T) (puts []e2eConfigPut, scriptCount int) {
	t.Helper()
	absGlob := mustAbs(t, e2eScriptsGlobRelPath)
	scripts, err := filepath.Glob(absGlob)
	if err != nil {
		t.Fatalf("glob %s: %v", absGlob, err)
	}
	if len(scripts) == 0 {
		t.Skipf("no e2e scripts matched %s; seed-shadow guard runs "+
			"where the gardenio sibling tree is present", e2eScriptsGlobRelPath)
	}
	sort.Strings(scripts)

	for _, path := range scripts {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		base := filepath.Base(path)
		for _, chunk := range splitCurlInvocations(string(data)) {
			// Only PUTs to /v1/configs/type: shadow a config. A GET (e.g.
			// test-trigger-action-fidelity.sh reading type:bug) or a bead POST
			// (the seed script's POST /v1/beads bodies, which also carry a
			// "fields" object) must NOT be treated as a shadow.
			if !strings.Contains(chunk, "-X PUT") {
				continue
			}
			urlMatch := configPutURLRe.FindStringSubmatch(chunk)
			if urlMatch == nil {
				continue
			}
			key := "type:" + urlMatch[1]

			bodyMatch := curlBodyRe.FindStringSubmatch(chunk)
			if bodyMatch == nil {
				// A config PUT with no `-d '…'` body means the request shape
				// changed (a heredoc, @file, or a `--data-raw`/variable) and the
				// extractor can no longer see the schema it must guard. Fail
				// loudly rather than silently under-report (the vacuity trap).
				t.Fatalf("%s: PUT to /v1/configs/%s has no `-d '…'` body the guard "+
					"can parse — the request idiom changed; update curlBodyRe in "+
					"config_seed_shadow_test.go", base, key)
			}
			var envelope struct {
				Key   string            `json:"key"`
				Value *model.TypeConfig `json:"value"`
			}
			if err := json.Unmarshal([]byte(bodyMatch[1]), &envelope); err != nil {
				t.Fatalf("%s: parse PUT body for %s %q: %v", base, key, bodyMatch[1], err)
			}
			if envelope.Value == nil {
				t.Fatalf("%s: PUT body for %s missing a \"value\" schema: %q", base, key, bodyMatch[1])
			}
			puts = append(puts, e2eConfigPut{script: base, key: key, tc: envelope.Value})
		}
	}
	return puts, len(scripts)
}

// splitCurlInvocations splits a script's source on curl-invocation boundaries so
// each returned chunk is exactly one HTTP request. The final chunk runs to EOF.
func splitCurlInvocations(src string) []string {
	locs := curlStartRe.FindAllStringIndex(src, -1)
	if len(locs) == 0 {
		return nil
	}
	chunks := make([]string, 0, len(locs))
	for i, loc := range locs {
		end := len(src)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		chunks = append(chunks, src[loc[0]:end])
	}
	return chunks
}

// TestE2ESeedTypes_NoBuiltinShadow asserts that every type:* schema ANY e2e
// script PUTs for a builtin-authoritative type is a non-weakening superset of the
// builtin — i.e. it cannot shadow-and-drift the builtin (ka-0000003677,
// generalized across all scripts in ka-0000000407).
func TestE2ESeedTypes_NoBuiltinShadow(t *testing.T) {
	puts, scriptCount := extractE2EConfigPuts(t)

	// Anti-vacuity floor. The whole point of generalizing this guard (ka-0000000407)
	// is that it must see EVERY e2e script, not just the shared seed — the original
	// single-file guard was blind to the 8 other shadowing scripts. If the glob ever
	// narrows to a single file (as it once was), the guard would silently go
	// vacuous — unable to catch a re-added shadow PUT anywhere else, the exact
	// defect this bead closed. The e2e scripts dir holds ~220 modules; require a
	// healthy floor so a narrowed glob fails loudly HERE instead of quietly
	// disarming the guard. (When the sibling
	// tree is absent entirely, extractE2EConfigPuts already t.Skip-ped above, so
	// this only fires on a PARTIAL stage.)
	const minE2EScripts = 50
	if scriptCount < minE2EScripts {
		t.Fatalf("seed-shadow guard globbed only %d e2e script(s) via %s — expected "+
			"at least %d. The .github/workflows/ci.yml kbeads job must stage the whole "+
			"gardenio/tests/e2e/scripts/*.sh dir, or the guard goes vacuous (blind to "+
			"shadow PUTs in the unstaged scripts).", scriptCount, e2eScriptsGlobRelPath, minE2EScripts)
	}

	for _, put := range puts {
		builtin, ok := builtinConfigs[put.key]
		if !ok {
			// No builtin: the script is authoritative for this type — nothing to
			// shadow, so any hand-maintained schema is legitimate here.
			continue
		}
		var btc model.TypeConfig
		if err := json.Unmarshal(builtin.Value, &btc); err != nil {
			t.Fatalf("parse builtin %s: %v", put.key, err)
		}
		for _, v := range seedShadowViolations(put.key, put.tc, &btc) {
			t.Errorf("%s: %s", put.script, v)
		}
	}
}

// seedShadowViolations returns one message per way the stored (seed) schema
// WEAKENS the builtin: a missing builtin field, a type mismatch, a required
// builtin field the seed left optional, or an enum whose builtin values the seed
// does not fully cover. Empty slice ⇒ the seed is a non-weakening superset (or
// there is nothing to compare). Each message names the exact remedy. Kept a pure
// function (no *testing.T) so its detection logic is unit-tested directly in
// TestSeedShadowViolations_DetectsDrift without needing the sibling file.
func seedShadowViolations(key string, seed, builtin *model.TypeConfig) []string {
	seedByName := make(map[string]model.FieldDef, len(seed.Fields))
	for _, f := range seed.Fields {
		seedByName[f.Name] = f
	}

	var out []string
	for _, bf := range builtin.Fields {
		sf, present := seedByName[bf.Name]
		if !present {
			out = append(out, key+": e2e seed schema OMITS builtin field "+quote(bf.Name)+" — it shadows and "+
				"drifts behind the daemon builtin (config.go). Do NOT hand-maintain this schema in "+
				"seed-types-and-users.sh: drop the whole `"+key+"` PUT so e2e exercises the builtin prod runs, "+
				"or (if a stored row is truly needed) add the missing field.")
			continue
		}
		if sf.Type != bf.Type {
			out = append(out, key+": field "+quote(bf.Name)+" typed "+quote(string(sf.Type))+" in the e2e seed "+
				"but "+quote(string(bf.Type))+" in the builtin — a weaker/wrong type lets the e2e namespace accept "+
				"writes the real daemon would reject (or vice versa). Drop the `"+key+"` PUT from "+
				"seed-types-and-users.sh, or match the builtin type.")
		}
		if bf.Required && !sf.Required {
			out = append(out, key+": builtin marks field "+quote(bf.Name)+" REQUIRED but the e2e seed leaves it "+
				"optional — the e2e namespace then accepts a create the real daemon rejects (false-green). Drop "+
				"the `"+key+"` PUT from seed-types-and-users.sh, or mark the field required.")
		}
		if bf.Type == model.FieldTypeEnum || bf.Type == model.FieldTypeEnums {
			if missing := valuesNotCovered(bf.Values, sf.Values); len(missing) > 0 {
				out = append(out, key+": enum field "+quote(bf.Name)+" is missing builtin value(s) "+
					fmt.Sprintf("%v", missing)+" in the e2e seed — a write using one of those values passes "+
					"against the real daemon but 400s in the e2e namespace (false-red) or the reverse. Drop the `"+
					key+"` PUT from seed-types-and-users.sh, or add the value(s).")
			}
		}
	}
	return out
}

func quote(s string) string { return `"` + s + `"` }

// valuesNotCovered returns the elements of want not present in have.
func valuesNotCovered(want, have []string) []string {
	haveSet := make(map[string]struct{}, len(have))
	for _, v := range have {
		haveSet[v] = struct{}{}
	}
	var missing []string
	for _, v := range want {
		if _, ok := haveSet[v]; !ok {
			missing = append(missing, v)
		}
	}
	sort.Strings(missing)
	return missing
}

func mustAbs(t *testing.T, rel string) string {
	t.Helper()
	abs, err := filepath.Abs(rel)
	if err != nil {
		t.Fatalf("resolve %s: %v", rel, err)
	}
	return abs
}

// TestSeedShadowViolations_DetectsDrift exercises the detection logic directly
// with synthetic (seed, builtin) pairs — no file dependency, so it runs even on
// a partial checkout where the sibling seed script is absent. It is
// the anti-vacuity companion to TestE2ESeedTypes_NoBuiltinShadow: the real guard
// passes vacuously today (the seed PUTs nothing), so without this a broken
// detector (e.g. one that never returns a violation) would look "green" forever.
func TestSeedShadowViolations_DetectsDrift(t *testing.T) {
	req := func(name string, typ model.FieldType, required bool, values ...string) model.FieldDef {
		return model.FieldDef{Name: name, Type: typ, Required: required, Values: values}
	}

	tests := []struct {
		name        string
		seed        *model.TypeConfig
		builtin     *model.TypeConfig
		wantCount   int
		wantSubstrs []string
	}{
		{
			name:      "identical schema is not a violation",
			seed:      &model.TypeConfig{Fields: []model.FieldDef{req("a", model.FieldTypeString, false)}},
			builtin:   &model.TypeConfig{Fields: []model.FieldDef{req("a", model.FieldTypeString, false)}},
			wantCount: 0,
		},
		{
			name:      "seed superset (extra field) is not a violation",
			seed:      &model.TypeConfig{Fields: []model.FieldDef{req("a", model.FieldTypeString, false), req("extra", model.FieldTypeString, false)}},
			builtin:   &model.TypeConfig{Fields: []model.FieldDef{req("a", model.FieldTypeString, false)}},
			wantCount: 0,
		},
		{
			name:        "omitted builtin field is a violation",
			seed:        &model.TypeConfig{Fields: []model.FieldDef{req("a", model.FieldTypeString, false)}},
			builtin:     &model.TypeConfig{Fields: []model.FieldDef{req("a", model.FieldTypeString, false), req("status_message", model.FieldTypeString, false)}},
			wantCount:   1,
			wantSubstrs: []string{"OMITS builtin field", "status_message"},
		},
		{
			name:        "weakened type (string vs float) is a violation",
			seed:        &model.TypeConfig{Fields: []model.FieldDef{req("max_budget", model.FieldTypeString, false)}},
			builtin:     &model.TypeConfig{Fields: []model.FieldDef{req("max_budget", model.FieldTypeFloat, false)}},
			wantCount:   1,
			wantSubstrs: []string{"max_budget", "wrong type"},
		},
		{
			name:        "required-in-builtin but optional-in-seed is a violation",
			seed:        &model.TypeConfig{Fields: []model.FieldDef{req("provider", model.FieldTypeString, false)}},
			builtin:     &model.TypeConfig{Fields: []model.FieldDef{req("provider", model.FieldTypeString, true)}},
			wantCount:   1,
			wantSubstrs: []string{"provider", "REQUIRED"},
		},
		{
			name:        "enum missing a builtin value is a violation",
			seed:        &model.TypeConfig{Fields: []model.FieldDef{req("source", model.FieldTypeEnum, false, "jira", "gitlab")}},
			builtin:     &model.TypeConfig{Fields: []model.FieldDef{req("source", model.FieldTypeEnum, false, "jira", "gitlab", "schedule")}},
			wantCount:   1,
			wantSubstrs: []string{"source", "missing builtin value", "schedule"},
		},
		{
			name:      "seed enum superset (extra value) is not a violation",
			seed:      &model.TypeConfig{Fields: []model.FieldDef{req("source", model.FieldTypeEnum, false, "jira", "gitlab", "extra")}},
			builtin:   &model.TypeConfig{Fields: []model.FieldDef{req("source", model.FieldTypeEnum, false, "jira", "gitlab")}},
			wantCount: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := seedShadowViolations("type:example", tc.seed, tc.builtin)
			if len(got) != tc.wantCount {
				t.Fatalf("got %d violations, want %d: %v", len(got), tc.wantCount, got)
			}
			for _, sub := range tc.wantSubstrs {
				found := false
				for _, g := range got {
					if strings.Contains(g, sub) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("no violation message contained %q; got %v", sub, got)
				}
			}
		})
	}
}
