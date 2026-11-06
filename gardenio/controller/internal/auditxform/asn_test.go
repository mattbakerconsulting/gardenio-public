package auditxform

import (
	"strings"
	"testing"
)

func mustTable(t *testing.T, text string) *PrefixTableResolver {
	t.Helper()
	r, err := ParseASNTable(strings.NewReader(text))
	if err != nil {
		t.Fatalf("ParseASNTable(%q): %v", text, err)
	}
	return r
}

func mustGeoTable(t *testing.T, text string) *PrefixTableResolver {
	t.Helper()
	r, err := ParseGeoTable(strings.NewReader(text))
	if err != nil {
		t.Fatalf("ParseGeoTable(%q): %v", text, err)
	}
	return r
}

func TestPrefixTableResolver_LongestPrefixWins(t *testing.T) {
	// A specific /24 announcement inside a larger /16 aggregate: the /24 must win
	// even though it is loaded AFTER the /16, so origin-AS resolution matches how
	// a routing table honours the more-specific prefix.
	r := mustTable(t, strings.Join([]string{
		"203.0.0.0/16   AS64500  AGGREGATE",
		"203.0.113.0/24 AS64512  SPECIFIC",
	}, "\n"))

	cases := []struct {
		ip, want string
	}{
		{"203.0.113.7", "AS64512"}, // inside the /24 — most specific
		{"203.0.55.9", "AS64500"},  // inside the /16 only
	}
	for _, c := range cases {
		got, ok := r.LookupASN(c.ip)
		if !ok || got != c.want {
			t.Errorf("LookupASN(%s) = (%q,%v), want (%q,true)", c.ip, got, ok, c.want)
		}
	}
}

func TestPrefixTableResolver_NoMatchAndBadIP(t *testing.T) {
	r := mustTable(t, "203.0.113.0/24 AS64512\n")
	for _, ip := range []string{"198.51.100.1", "", "not-an-ip", "   "} {
		if got, ok := r.LookupASN(ip); ok {
			t.Errorf("LookupASN(%q) = (%q,true), want no match", ip, got)
		}
	}
}

func TestPrefixTableResolver_NilAndZeroValueSafe(t *testing.T) {
	var nilR *PrefixTableResolver
	if _, ok := nilR.LookupASN("203.0.113.7"); ok {
		t.Error("nil *PrefixTableResolver must resolve nothing")
	}
	var zero PrefixTableResolver
	if _, ok := zero.LookupASN("203.0.113.7"); ok {
		t.Error("zero PrefixTableResolver must resolve nothing")
	}
}

func TestPrefixTableResolver_HostRouteAndV6(t *testing.T) {
	r := mustTable(t, strings.Join([]string{
		"198.51.100.7   AS65000  # bare IP => host route",
		"2001:db8::/32  AS65001",
	}, "\n"))
	if got, ok := r.LookupASN("198.51.100.7"); !ok || got != "AS65000" {
		t.Errorf("host route: got (%q,%v), want (AS65000,true)", got, ok)
	}
	if got, ok := r.LookupASN("198.51.100.8"); ok {
		t.Errorf("host route must not match a neighbour, got %q", got)
	}
	if got, ok := r.LookupASN("2001:db8::1"); !ok || got != "AS65001" {
		t.Errorf("v6: got (%q,%v), want (AS65001,true)", got, ok)
	}
}

func TestParseASNTable_SkipsCommentsAndBlanks(t *testing.T) {
	r := mustTable(t, strings.Join([]string{
		"# header comment",
		"",
		"   # indented comment",
		"203.0.113.0/24 AS64512 EXAMPLE with trailing description ignored",
	}, "\n"))
	if got, ok := r.LookupASN("203.0.113.7"); !ok || got != "AS64512" {
		t.Errorf("got (%q,%v), want (AS64512,true)", got, ok)
	}
}

func TestParseASNTable_MalformedFailsLoudly(t *testing.T) {
	cases := []struct{ name, text string }{
		{"missing ASN", "203.0.113.0/24\n"},
		{"comment as ASN", "203.0.113.0/24 #note\n"},
		{"bad CIDR", "not-a-cidr AS64512\n"},
		{"bad mask", "10.0.0.0/33 AS1\n"},
	}
	for _, c := range cases {
		if _, err := ParseASNTable(strings.NewReader(c.text)); err == nil {
			t.Errorf("%s: expected an error, got nil", c.name)
		}
	}
}

// --- geo (country) dimension — additive to the ASN contract -----------------

// TestPrefixTableResolver_ImplementsGeoResolver pins the interface promise the
// enrichment path relies on: the concrete resolver is a GeoResolver, so
// traefikshipper's type-assertion succeeds and sourceCountry can be emitted. If
// this ever regresses (e.g. LookupGeo is dropped), the geomap console silently
// loses its group key.
func TestPrefixTableResolver_ImplementsGeoResolver(t *testing.T) {
	var _ ASNResolver = (*PrefixTableResolver)(nil)
	var _ GeoResolver = (*PrefixTableResolver)(nil)
}

func TestLookupGeo_ReturnsASNAndCountry(t *testing.T) {
	r := mustGeoTable(t, strings.Join([]string{
		"203.0.0.0/16   AS64500  US  AGGREGATE",
		"203.0.113.0/24 AS64512  DE  SPECIFIC",
	}, "\n"))

	cases := []struct{ ip, wantASN, wantCountry string }{
		{"203.0.113.7", "AS64512", "DE"}, // longest-prefix wins for geo too
		{"203.0.55.9", "AS64500", "US"},
	}
	for _, c := range cases {
		g, ok := r.LookupGeo(c.ip)
		if !ok || g.ASN != c.wantASN || g.Country != c.wantCountry {
			t.Errorf("LookupGeo(%s) = (%+v,%v), want ({%s %s},true)", c.ip, g, ok, c.wantASN, c.wantCountry)
		}
		// LookupASN must agree with LookupGeo's ASN — the country dimension never
		// changes the ASN answer.
		if a, aok := r.LookupASN(c.ip); !aok || a != c.wantASN {
			t.Errorf("LookupASN(%s) = (%q,%v), want (%s,true)", c.ip, a, aok, c.wantASN)
		}
	}
	// An uncovered / bad IP resolves neither.
	for _, ip := range []string{"198.51.100.1", "", "not-an-ip"} {
		if g, ok := r.LookupGeo(ip); ok {
			t.Errorf("LookupGeo(%q) = (%+v,true), want no match", ip, g)
		}
	}
}

func TestLookupGeo_NilAndZeroValueSafe(t *testing.T) {
	var nilR *PrefixTableResolver
	if _, ok := nilR.LookupGeo("203.0.113.7"); ok {
		t.Error("nil *PrefixTableResolver must resolve no geo")
	}
	var zero PrefixTableResolver
	if _, ok := zero.LookupGeo("203.0.113.7"); ok {
		t.Error("zero PrefixTableResolver must resolve no geo")
	}
}

// TestParseGeoTable_NoneCountryNormalisedToEmpty pins the iptoasn "None"
// sentinel handling: a covered range with an unknown country still resolves its
// ASN but carries an EMPTY country, so enrichment omits sourceCountry rather than
// bucketing a bogus "None" on the choropleth.
func TestParseGeoTable_NoneCountryNormalisedToEmpty(t *testing.T) {
	r := mustGeoTable(t, strings.Join([]string{
		"203.0.113.0/24 AS64512 US",
		"198.51.100.0/24 AS65000 None  # upstream had no geo for this range",
	}, "\n"))
	if g, ok := r.LookupGeo("203.0.113.7"); !ok || g.Country != "US" {
		t.Errorf("known-geo: got (%+v,%v), want country US", g, ok)
	}
	g, ok := r.LookupGeo("198.51.100.9")
	if !ok {
		t.Fatal("None-country range must still resolve its ASN")
	}
	if g.ASN != "AS65000" {
		t.Errorf("None-country ASN = %q, want AS65000", g.ASN)
	}
	if g.Country != "" {
		t.Errorf("None country must normalise to empty, got %q", g.Country)
	}
}

// TestParseASNTable_YieldsEmptyCountry confirms the pre-geo ASN-only format
// still parses and produces resolvable entries whose country is empty — the
// backward-compat guarantee that keeps every existing operator table valid.
func TestParseASNTable_YieldsEmptyCountry(t *testing.T) {
	r := mustTable(t, "203.0.113.0/24 AS64512 EXAMPLE-NET\n")
	g, ok := r.LookupGeo("203.0.113.7")
	if !ok || g.ASN != "AS64512" || g.Country != "" {
		t.Errorf("ASN-only table LookupGeo = (%+v,%v), want ({AS64512 \"\"},true)", g, ok)
	}
}

func TestParseGeoTable_MalformedFailsLoudly(t *testing.T) {
	cases := []struct{ name, text string }{
		{"missing ASN+country", "203.0.113.0/24\n"},
		{"missing country", "203.0.113.0/24 AS64512\n"},
		{"comment as ASN", "203.0.113.0/24 #note\n"},
		{"comment as country", "203.0.113.0/24 AS64512 #note\n"},
		{"bad CIDR", "not-a-cidr AS64512 US\n"},
		{"bad mask", "10.0.0.0/33 AS1 US\n"},
	}
	for _, c := range cases {
		if _, err := ParseGeoTable(strings.NewReader(c.text)); err == nil {
			t.Errorf("%s: expected an error, got nil", c.name)
		}
	}
}

func TestParseGeoTable_SkipsCommentsAndBlanksAndDescription(t *testing.T) {
	r := mustGeoTable(t, strings.Join([]string{
		"# header",
		"",
		"   # indented comment",
		"203.0.113.0/24 AS64512 US trailing free-form description ignored",
	}, "\n"))
	if g, ok := r.LookupGeo("203.0.113.7"); !ok || g.ASN != "AS64512" || g.Country != "US" {
		t.Errorf("got (%+v,%v), want ({AS64512 US},true)", g, ok)
	}
}
