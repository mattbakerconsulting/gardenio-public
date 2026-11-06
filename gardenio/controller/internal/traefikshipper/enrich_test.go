package traefikshipper

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/auditxform"
)

// geoTable builds a GeoResolver over a small CIDR->{ASN,country} table.
func geoTable(t *testing.T, rows string) auditxform.GeoResolver {
	t.Helper()
	r, err := auditxform.ParseGeoTable(strings.NewReader(rows))
	if err != nil {
		t.Fatalf("ParseGeoTable: %v", err)
	}
	return r
}

// asnTable builds an ASN-only resolver (no country) so the ASNResolver-only
// branch of enrichLine is exercised.
func asnTable(t *testing.T, rows string) auditxform.ASNResolver {
	t.Helper()
	r, err := auditxform.ParseASNTable(strings.NewReader(rows))
	if err != nil {
		t.Fatalf("ParseASNTable: %v", err)
	}
	return r
}

// decode unmarshals a shipped JSON line for field assertions.
func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode %q: %v", b, err)
	}
	return m
}

func TestClientIPFromXFF(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"single public ip", "203.0.113.77", "203.0.113.77"},
		{"rightmost wins (depth 1)", "203.0.113.7, 198.51.100.9", "198.51.100.9"},
		{"rightmost wins three hops", "1.1.1.1, 2.2.2.2, 3.3.3.3", "3.3.3.3"},
		{"whitespace trimmed", "  203.0.113.77  ", "203.0.113.77"},
		{"v4-in-v6 unmapped", "::ffff:203.0.113.77", "203.0.113.77"},
		{"ipv6", "2001:db8::1", "2001:db8::1"},
		{"empty string", "", ""},
		{"absent (nil)", nil, ""},
		{"non-string non-slice", 42, ""},
		{"unparseable rightmost => empty (no spoofable fallback)", "203.0.113.7, not-an-ip", ""},
		{"slice of strings joined, rightmost", []any{"1.2.3.4", "5.6.7.8"}, "5.6.7.8"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := clientIPFromXFF(c.in); got != c.want {
				t.Fatalf("clientIPFromXFF(%v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// A GeoResolver stamps client_ip + sourceASN + sourceCountry from the row's XFF.
func TestEnrichLineGeo(t *testing.T) {
	s := &Shipper{asn: geoTable(t, "203.0.113.0/24 AS64512 US EXAMPLE-NET\n")}
	in := []byte(`{"DownstreamStatus":403,"RequestMethod":"GET","RequestPath":"/wp-login.php","request_X-Forwarded-For":"203.0.113.77"}`)

	out, keep := s.processLine(in)
	if !keep {
		t.Fatal("access record must be kept")
	}
	m := decode(t, out)
	if m[auditxform.FieldClientIP] != "203.0.113.77" {
		t.Errorf("client_ip = %v, want 203.0.113.77", m[auditxform.FieldClientIP])
	}
	if m[auditxform.FieldSourceASN] != "AS64512" {
		t.Errorf("sourceASN = %v, want AS64512", m[auditxform.FieldSourceASN])
	}
	if m[auditxform.FieldSourceCountry] != "US" {
		t.Errorf("sourceCountry = %v, want US", m[auditxform.FieldSourceCountry])
	}
	// The raw forensic fields must survive.
	if m["RequestPath"] != "/wp-login.php" || m["request_X-Forwarded-For"] != "203.0.113.77" {
		t.Errorf("raw fields not preserved: %v", m)
	}
}

// An ASN-only resolver stamps client_ip + sourceASN but never sourceCountry.
func TestEnrichLineASNOnly(t *testing.T) {
	s := &Shipper{asn: asnTable(t, "203.0.113.0/24 AS64512 EXAMPLE-NET\n")}
	in := []byte(`{"DownstreamStatus":200,"RequestMethod":"GET","request_X-Forwarded-For":"203.0.113.77"}`)

	out, _ := s.processLine(in)
	m := decode(t, out)
	if m[auditxform.FieldSourceASN] != "AS64512" {
		t.Errorf("sourceASN = %v, want AS64512", m[auditxform.FieldSourceASN])
	}
	if _, ok := m[auditxform.FieldSourceCountry]; ok {
		t.Errorf("sourceCountry must be ABSENT for an ASN-only table; got %v", m[auditxform.FieldSourceCountry])
	}
	if m[auditxform.FieldClientIP] != "203.0.113.77" {
		t.Errorf("client_ip = %v, want 203.0.113.77", m[auditxform.FieldClientIP])
	}
}

// A covered IP whose geo entry has no country (iptoasn "None" sentinel) gets a
// sourceASN but omits sourceCountry — fail-open on geo, ASN still emitted.
func TestEnrichLineGeoUnknownCountry(t *testing.T) {
	s := &Shipper{asn: geoTable(t, "203.0.113.0/24 AS64512 None SOME-NET\n")}
	in := []byte(`{"DownstreamStatus":200,"RequestMethod":"GET","request_X-Forwarded-For":"203.0.113.77"}`)

	out, _ := s.processLine(in)
	m := decode(t, out)
	if m[auditxform.FieldSourceASN] != "AS64512" {
		t.Errorf("sourceASN = %v, want AS64512", m[auditxform.FieldSourceASN])
	}
	if _, ok := m[auditxform.FieldSourceCountry]; ok {
		t.Errorf("sourceCountry must be ABSENT when the country is the 'None' sentinel; got %v", m[auditxform.FieldSourceCountry])
	}
}

// An IP the table does not cover: client_ip is still stamped (it is known), but
// no sourceASN/sourceCountry — the row ships, it just isn't geolocated.
func TestEnrichLineUncoveredIP(t *testing.T) {
	s := &Shipper{asn: geoTable(t, "203.0.113.0/24 AS64512 US\n")}
	in := []byte(`{"DownstreamStatus":200,"RequestMethod":"GET","request_X-Forwarded-For":"8.8.8.8"}`)

	out, _ := s.processLine(in)
	m := decode(t, out)
	if m[auditxform.FieldClientIP] != "8.8.8.8" {
		t.Errorf("client_ip = %v, want 8.8.8.8", m[auditxform.FieldClientIP])
	}
	if _, ok := m[auditxform.FieldSourceASN]; ok {
		t.Errorf("sourceASN must be ABSENT for an uncovered IP; got %v", m[auditxform.FieldSourceASN])
	}
	if _, ok := m[auditxform.FieldSourceCountry]; ok {
		t.Errorf("sourceCountry must be ABSENT for an uncovered IP; got %v", m[auditxform.FieldSourceCountry])
	}
}

// Internal traffic carries no X-Forwarded-For (only the SNAT'd ClientHost). The
// row still ships, with an empty client_ip and no ASN/country: enrichment must
// never DROP a record, and a detective control keeps the full raw record.
func TestEnrichLineNoXFF(t *testing.T) {
	s := &Shipper{asn: geoTable(t, "203.0.113.0/24 AS64512 US\n")}
	in := []byte(`{"DownstreamStatus":200,"RequestMethod":"GET","ClientHost":"10.50.49.75","request_X-Real-Ip":"10.50.49.75"}`)

	out, keep := s.processLine(in)
	if !keep {
		t.Fatal("a row without XFF must still ship (never dropped)")
	}
	m := decode(t, out)
	if m[auditxform.FieldClientIP] != "" {
		t.Errorf("client_ip = %v, want empty for a row with no XFF", m[auditxform.FieldClientIP])
	}
	if _, ok := m[auditxform.FieldSourceASN]; ok {
		t.Errorf("sourceASN must be ABSENT with no client IP; got %v", m[auditxform.FieldSourceASN])
	}
	// Raw fields survive.
	if m["ClientHost"] != "10.50.49.75" {
		t.Errorf("ClientHost not preserved: %v", m)
	}
}

// publicClientHost is the PROXY-protocol fallback: only a globally-routable
// ClientHost is treated as the client IP; every private/SNAT/CGNAT value is
// rejected so internal traffic keeps emitting no client_ip (ka-0000000358).
func TestPublicClientHost(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"public v4 (PROXY-reconstructed client)", "203.0.113.77", "203.0.113.77"},
		{"public v6", "2001:db8::1", "2001:db8::1"},
		{"host:port tolerated", "203.0.113.77:54321", "203.0.113.77"},
		{"v4-in-v6 unmapped", "::ffff:203.0.113.77", "203.0.113.77"},
		{"whitespace trimmed", "  203.0.113.77  ", "203.0.113.77"},
		{"private RFC1918 (SNAT hop) rejected", "10.50.49.75", ""},
		{"private 172.16/12 rejected", "172.16.0.9", ""},
		{"private 192.168 rejected", "192.168.1.1", ""},
		{"CGNAT 100.64/10 rejected", "100.64.1.1", ""},
		{"loopback rejected", "127.0.0.1", ""},
		{"link-local rejected", "169.254.1.1", ""},
		{"unspecified rejected", "0.0.0.0", ""},
		{"ULA v6 rejected", "fd00::1", ""},
		{"empty string", "", ""},
		{"absent (nil)", nil, ""},
		{"non-string", 42, ""},
		{"unparseable", "not-an-ip", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := publicClientHost(c.in); got != c.want {
				t.Fatalf("publicClientHost(%v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// With PROXY protocol trusted on the webhook entrypoint, Traefik reconstructs the
// real client into ClientHost and injects no XFF. The enricher must geolocate off
// that public ClientHost (ka-0000000358).
func TestEnrichLineClientHostPublic(t *testing.T) {
	s := &Shipper{asn: geoTable(t, "203.0.113.0/24 AS64512 US EXAMPLE-NET\n")}
	in := []byte(`{"DownstreamStatus":200,"RequestMethod":"GET","ClientHost":"203.0.113.77","request_X-Real-Ip":"10.50.11.197"}`)

	out, keep := s.processLine(in)
	if !keep {
		t.Fatal("access record must be kept")
	}
	m := decode(t, out)
	if m[auditxform.FieldClientIP] != "203.0.113.77" {
		t.Errorf("client_ip = %v, want 203.0.113.77 (from public ClientHost)", m[auditxform.FieldClientIP])
	}
	if m[auditxform.FieldSourceASN] != "AS64512" {
		t.Errorf("sourceASN = %v, want AS64512", m[auditxform.FieldSourceASN])
	}
	if m[auditxform.FieldSourceCountry] != "US" {
		t.Errorf("sourceCountry = %v, want US", m[auditxform.FieldSourceCountry])
	}
}

// XFF, when present, still wins over ClientHost — a proxy that injects XFF is the
// authoritative client source and ClientHost may be its own hop.
func TestEnrichLineXFFWinsOverClientHost(t *testing.T) {
	s := &Shipper{asn: geoTable(t, "203.0.113.0/24 AS64512 US\n198.51.100.0/24 AS64513 GB\n")}
	in := []byte(`{"DownstreamStatus":200,"RequestMethod":"GET","request_X-Forwarded-For":"203.0.113.77","ClientHost":"198.51.100.9"}`)

	out, _ := s.processLine(in)
	m := decode(t, out)
	if m[auditxform.FieldClientIP] != "203.0.113.77" {
		t.Errorf("client_ip = %v, want 203.0.113.77 (XFF precedence)", m[auditxform.FieldClientIP])
	}
	if m[auditxform.FieldSourceASN] != "AS64512" {
		t.Errorf("sourceASN = %v, want AS64512 (from XFF, not ClientHost)", m[auditxform.FieldSourceASN])
	}
}

// An enriched row carries the surface-agnostic principal contract field lifted
// from ClientUsername, so the geomap's post-compromise panels can attribute a geo
// to a principal (ka-0000005007).
func TestEnrichLinePrincipal(t *testing.T) {
	s := &Shipper{asn: geoTable(t, "203.0.113.0/24 AS64512 US\n")}
	cases := map[string]struct {
		in   string
		want string
	}{
		"authenticated user":  {`{"DownstreamStatus":200,"ClientUsername":"alice@example.com","request_X-Forwarded-For":"203.0.113.77"}`, "alice@example.com"},
		"traefik dash sentinel": {`{"DownstreamStatus":200,"ClientUsername":"-","request_X-Forwarded-For":"203.0.113.77"}`, "-"},
		"absent username":      {`{"DownstreamStatus":200,"request_X-Forwarded-For":"203.0.113.77"}`, "-"},
		"empty username":       {`{"DownstreamStatus":200,"ClientUsername":"","request_X-Forwarded-For":"203.0.113.77"}`, "-"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			out, keep := s.processLine([]byte(c.in))
			if !keep {
				t.Fatal("access record must be kept")
			}
			m := decode(t, out)
			if got := m[auditxform.FieldPrincipal]; got != c.want {
				t.Errorf("principal = %q, want %q", got, c.want)
			}
		})
	}
}

// With a NIL resolver, processLine is byte-identical to acceptLine — no
// client_ip/sourceASN/sourceCountry is stamped. This is the deploy-raw-first path.
func TestProcessLineNilResolverByteIdentical(t *testing.T) {
	s := &Shipper{asn: nil}
	in := []byte(`{"DownstreamStatus":403,"RequestMethod":"GET","request_X-Forwarded-For":"203.0.113.77"}`)

	gotOut, gotKeep := s.processLine(in)
	wantOut, wantKeep := acceptLine(in)
	if gotKeep != wantKeep {
		t.Fatalf("keep mismatch: got %v want %v", gotKeep, wantKeep)
	}
	if string(gotOut) != string(wantOut) {
		t.Fatalf("nil-resolver output must be byte-identical to acceptLine:\n got %q\nwant %q", gotOut, wantOut)
	}
	m := decode(t, gotOut)
	for _, f := range []string{auditxform.FieldClientIP, auditxform.FieldSourceASN, auditxform.FieldSourceCountry, auditxform.FieldPrincipal} {
		if _, ok := m[f]; ok {
			t.Errorf("field %q must be ABSENT with a nil resolver; got %v", f, m[f])
		}
	}
}

// A dropped (non-access) line stays dropped whether or not a resolver is wired:
// enrichment runs only on records that pass the access filter.
func TestProcessLineDropsNonAccessWithResolver(t *testing.T) {
	s := &Shipper{asn: geoTable(t, "203.0.113.0/24 AS64512 US\n")}
	for _, in := range []string{
		`{"level":"error","msg":"tls config failed"}`,
		`2026-08-08T20:09:07Z ERR boom`,
		``,
	} {
		if _, keep := s.processLine([]byte(in)); keep {
			t.Errorf("non-access line must be dropped: %q", in)
		}
	}
}

// End-to-end through the stream loop: a GeoResolver-wired Shipper ships a batch
// whose records carry the enriched fields, and non-2xx is never sampled out.
func TestRunEnrichesEndToEnd(t *testing.T) {
	payload := strings.Join([]string{
		`{"DownstreamStatus":200,"RequestMethod":"GET","RequestPath":"/","request_X-Forwarded-For":"203.0.113.77"}`,
		`{"DownstreamStatus":401,"RequestMethod":"POST","RequestPath":"/login","request_X-Forwarded-For":"198.51.100.9"}`,
	}, "\n") + "\n"

	sink := &fakeSink{}
	st := &fakeStreamer{payload: payload}
	resolver := geoTable(t, "203.0.113.0/24 AS64512 US\n198.51.100.0/24 AS65000 DE\n")
	sh := New(Config{FlushInterval: 20 * time.Millisecond, ReconnectBackoff: time.Hour}, st, sink, nil, resolver)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_ = sh.Run(ctx)

	got := sink.all()
	for _, want := range []string{`"sourceCountry":"US"`, `"sourceASN":"AS64512"`, `"client_ip":"203.0.113.77"`, `"sourceCountry":"DE"`, `"client_ip":"198.51.100.9"`} {
		if !strings.Contains(got, want) {
			t.Errorf("shipped batch missing %s:\n%s", want, got)
		}
	}
}
