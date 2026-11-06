package landmap

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gardenio/lands/contract/landsv1"
)

// The response denylist (denylist.go) over every response a test here
// records: assertHygiene runs the shipped scanner on each one, and
// TestResponseHygiene_Golden replays the contract's request fixtures through
// every verb to record them. The tests and the live counter run one scanner,
// so they cannot drift apart.

// contractFixturesDir is the contract's golden fixtures.
const contractFixturesDir = "../../../lands/contract/fixtures"

func loadDenylist(t testing.TB) denylist {
	t.Helper()
	d, err := liveDenylist()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// fixtureLeaks are this file's fixture identities: the other land (zeta, its
// zz- ids), the daemon's principals and its operator-only text.
var fixtureLeaks = []string{"zeta", "zz-", "project:", "oidc:", "@example.com", "operator-only", "secret"}

func assertHygiene(t *testing.T, what string, r resp) {
	t.Helper()
	for _, hit := range loadDenylist(t).scan(r.header, r.body) {
		t.Errorf("%s: denylisted %s:\n%s", what, hit, r.body)
	}
	for _, l := range fixtureLeaks {
		if strings.Contains(r.body, l) {
			t.Errorf("%s: response carries fixture identity %q:\n%s", what, l, r.body)
		}
	}
	if got := r.header.Get(landsv1.HeaderContract); got != landsv1.ContractVersion {
		t.Errorf("%s: %s = %q", what, landsv1.HeaderContract, got)
	}
}

// The denylist must bite: each generated rule catches its canary, and the
// contract's own golden fixtures, which are what a clean response looks like,
// pass it.
func TestDenylist_BitesAndPassesTheContractsFixtures(t *testing.T) {
	d := loadDenylist(t)
	for _, k := range []string{"fields", "labels", "created_by", "plan_state", "operator_id", "operated_by_display", "requesting_agent"} {
		if !d.keys[k] {
			t.Errorf("daemon key %q is not denylisted", k)
		}
	}
	for _, k := range []string{"id", "title", "status", "kind", "code", "correlation_id", "updated_at", "next_cursor"} {
		if d.keys[k] {
			t.Errorf("contract key %q is denylisted", k)
		}
	}
	for _, typ := range []string{"epic", "chore", "advice", "molecule", "agent", "land_invite"} {
		if !d.types[typ] {
			t.Errorf("bead type %q is not denylisted", typ)
		}
	}
	for _, typ := range []string{"task", "bug", "feature", "decision", "escalation"} {
		if d.types[typ] {
			t.Errorf("contract word %q is denylisted as a bead type", typ)
		}
	}
	for canary, want := range map[string]string{
		`{"id":"x","fields":{}}`:            "daemon field key",
		`{"kind":"epic"}`:                   "bead type",
		`{"title":"see /v1/beads?x=1"}`:     "daemon route",
		`{"title":"see /v1/me"}`:            "daemon route",
		`{"title":"plan_state is ready"}`:   "daemon field",
		`{"title":"from kbeads"}`:           `"kbeads"`,
		`{"title":"blocks gd-AbCdEf1234"}`:  "gd|ka",
		`{"title":"POST /v1/decisions/x"}`:  "decisions",
		`{"title":"a bead"}`:                `"bead"`,
		`{"title":"see /v1/members"}`:       "",
		`{"title":"a decision, a ticket"}`:  "",
		`{"title":"an or-AbCdEf1234 ref"}`:  "",
		`{"correlation_id":"a","code":"x"}`: "",
		// artifact_type's enum fixes its words, so epic passes under that
		// key only; a bead type outside the enum still bites there.
		`{"artifact_type":"epic"}`:  "",
		`{"artifact_type":"chore"}`: "bead type",
		`{"label":"epic"}`:          "bead type",
		// A minted id that spells a token is masked; the same hex anywhere
		// else, or a correlation_id not in the minted shape, is not.
		`{"code":"x","correlation_id":"4bead31d6905e1493668ced4"}`: "",
		`{"title":"4bead31d6905e1493668ced4"}`:                     `"bead"`,
		`{"correlation_id":"4bead31d6905e1493668ced4x"}`:           `"bead"`,
		`{"correlation_id":"kbeads-0000000000000000"}`:             `"kbeads"`,
		// So is a minted bearer or refresh, and only in that shape.
		`{"bearer":"4bead31d6905e1493668ced44bead31d6905e1493668ced4000000000000000a"}`:  "",
		`{"refresh":"4bead31d6905e1493668ced44bead31d6905e1493668ced4000000000000000a"}`: "",
		`{"name":"4bead31d6905e1493668ced44bead31d6905e1493668ced4000000000000000a"}`:    `"bead"`,
		`{"bearer":"4bead31d"}`: `"bead"`,
	} {
		var got string
		hits := d.scan(nil, canary)
		for _, h := range hits {
			got += h.String() + "; "
		}
		switch {
		case want == "" && len(hits) > 0:
			t.Errorf("clean %s: %s", canary, got)
		case want != "" && !strings.Contains(got, want):
			t.Errorf("%s: hits %q, want one naming %q", canary, got, want)
		}
	}
	// The mask follows the minter: an id it mints is in the masked shape.
	for range 100 {
		if body := `"correlation_id":"` + newCorrelationID() + `"`; !mintedCorrelationID.MatchString(body) {
			t.Fatalf("newCorrelationID minted %s, which the scan does not mask", body)
		}
	}
	if hits := d.scan(http.Header{"X-Kbeads-Version": {"v"}}, "{}"); len(hits) == 0 {
		t.Error("a daemon header passed the denylist")
	}

	n := 0
	err := filepath.WalkDir(contractFixturesDir, func(p string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() && e.Name() == "_invalid" {
			return filepath.SkipDir
		}
		if e.IsDir() || !strings.HasSuffix(p, ".json") {
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		n++
		for _, hit := range d.scan(nil, string(raw)) {
			t.Errorf("contract fixture %s: denylisted %s", p, hit)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no contract fixtures found")
	}
}
