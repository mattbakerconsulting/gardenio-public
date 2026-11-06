package auditxform

import (
	"bufio"
	"fmt"
	"io"
	"net/netip"
	"sort"
	"strings"
)

// PrefixTableResolver resolves a source IP to its origin {ASN, country} by
// longest-prefix match over a static CIDR->geo table — the offline form of
// ip2asn/ip2country data (a BGP/geo dump reduced to prefixes). It is the default
// ASNResolver AND GeoResolver: traefik-log-shipper loads an operator-provided
// table so the geomap console can group by sourceASN and sourceCountry, without
// this package bundling any dataset or making a network call.
//
// The country dimension is ADDITIVE: an ASN-only table (ParseASNTable /
// NewPrefixTableResolver) leaves every entry's country empty, so LookupASN and
// the emitted fields are byte-identical to the pre-geo behaviour. A geo table
// (ParseGeoTable / NewGeoPrefixTableResolver) additionally carries an ISO
// 3166-1 alpha-2 country per prefix.
//
// Longest-prefix wins: when several table entries contain the queried address,
// the most specific (largest prefix length) — and, for equal specificity, the
// first loaded — is returned. This mirrors how a routing table resolves an
// origin AS, so a specific announcement inside a larger aggregate is honoured.
//
// A zero PrefixTableResolver resolves nothing (LookupASN/LookupGeo always return
// false), so it is a safe no-op default.
//
// The resolver is immutable after construction and therefore safe for
// concurrent use by many shipper goroutines.
type PrefixTableResolver struct {
	// entries are sorted by descending prefix length so the first containing
	// match is the longest-prefix match. Insertion order is preserved within a
	// single prefix length (stable sort) so an earlier line wins a tie.
	entries []geoEntry
}

// geoEntry is one CIDR->{ASN, country} row. country is "" for an ASN-only table
// or an entry whose geo is unknown.
type geoEntry struct {
	prefix  netip.Prefix
	asn     string
	country string
}

// LookupASN returns the ASN for ip by longest-prefix match, or ("", false) when
// ip is empty/unparseable or no table entry covers it. It is the ASNResolver
// half of the resolver and is byte-identical to the pre-geo behaviour: the
// country dimension never changes what an ASN lookup returns.
func (t *PrefixTableResolver) LookupASN(ip string) (string, bool) {
	if t == nil {
		return "", false
	}
	e, ok := t.lookup(ip)
	if !ok {
		return "", false
	}
	return e.asn, true
}

// LookupGeo returns the {ASN, country} for ip by longest-prefix match, or
// (Geo{}, false) when ip is empty/unparseable or no table entry covers it. The
// Country field is "" for an ASN-only table or a covered entry whose geo is
// unknown — a matched entry always carries a non-empty ASN (enforced at
// construction) but MAY carry an empty country, so a consumer omits the
// sourceCountry field rather than emitting an empty one.
func (t *PrefixTableResolver) LookupGeo(ip string) (Geo, bool) {
	if t == nil {
		return Geo{}, false
	}
	e, ok := t.lookup(ip)
	if !ok {
		return Geo{}, false
	}
	return Geo{ASN: e.asn, Country: e.country}, true
}

// lookup returns the longest-prefix entry covering ip, or (_, false) when ip is
// empty/unparseable or uncovered. It is the shared core of LookupASN/LookupGeo.
func (t *PrefixTableResolver) lookup(ip string) (geoEntry, bool) {
	if ip == "" {
		return geoEntry{}, false
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return geoEntry{}, false
	}
	// Unmap so a v4-in-v6 address matches a v4 prefix (and vice-versa).
	addr = addr.Unmap()
	for _, e := range t.entries {
		if e.prefix.Contains(addr) {
			return e, true
		}
	}
	return geoEntry{}, false
}

// NewPrefixTableResolver builds an ASN-only resolver from the given entries.
// Each key is a CIDR (e.g. "203.0.113.0/24") and each value the ASN token to
// emit verbatim (e.g. "AS64512"). An unparseable CIDR or an empty ASN is an
// error — a silently dropped mapping is a blind spot in a detective control. A
// bare IP with no prefix length is accepted as a host route (/32 or /128).
//
// The resulting resolver carries no country, so LookupGeo returns an empty
// Country and enrichment omits sourceCountry — byte-identical to the pre-geo
// behaviour. Use NewGeoPrefixTableResolver to also carry country.
func NewPrefixTableResolver(entries []struct {
	CIDR string
	ASN  string
}) (*PrefixTableResolver, error) {
	geo := make([]GeoTableEntry, len(entries))
	for i, e := range entries {
		geo[i] = GeoTableEntry{CIDR: e.CIDR, ASN: e.ASN}
	}
	return NewGeoPrefixTableResolver(geo)
}

// GeoTableEntry is one row of a CIDR->{ASN, country} table. Country is the ISO
// 3166-1 alpha-2 code (e.g. "US") and may be empty for an ASN-only entry or an
// entry whose geo is unknown; ASN is required.
type GeoTableEntry struct {
	CIDR    string
	ASN     string
	Country string
}

// NewGeoPrefixTableResolver builds a resolver from CIDR->{ASN, country} entries.
// An unparseable CIDR or an empty ASN is an error (a dropped mapping is a blind
// spot). Country is optional and emitted verbatim; an empty country means "geo
// unknown" and simply omits the sourceCountry field for addresses it covers.
func NewGeoPrefixTableResolver(entries []GeoTableEntry) (*PrefixTableResolver, error) {
	out := make([]geoEntry, 0, len(entries))
	for _, e := range entries {
		pfx, err := parsePrefix(e.CIDR)
		if err != nil {
			return nil, err
		}
		asn := strings.TrimSpace(e.ASN)
		if asn == "" {
			return nil, fmt.Errorf("auditxform: empty ASN for CIDR %q", e.CIDR)
		}
		out = append(out, geoEntry{prefix: pfx, asn: asn, country: strings.TrimSpace(e.Country)})
	}
	// Stable sort by descending prefix length: the first entry that Contains an
	// address is then the longest-prefix (most specific) match, ties broken by
	// load order.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].prefix.Bits() > out[j].prefix.Bits()
	})
	return &PrefixTableResolver{entries: out}, nil
}

// ParseASNTable reads a whitespace-delimited CIDR->ASN table and builds an
// ASN-only PrefixTableResolver (no country). The format, one entry per line:
//
//	<CIDR-or-IP> <ASN> [description...]
//	203.0.113.0/24   AS64512   EXAMPLE-NET
//	198.51.100.7     AS65000   # a host route
//
// Blank lines and lines whose first non-space rune is '#' are skipped, as is any
// trailing description after the ASN token. A malformed line (missing CIDR/ASN,
// unparseable CIDR) is an error naming the line number — the table is a security
// input, so a typo fails loudly rather than silently under-covering.
//
// This is the pre-geo format and is unchanged; use ParseGeoTable to also carry a
// country column.
func ParseASNTable(r io.Reader) (*PrefixTableResolver, error) {
	var entries []GeoTableEntry
	err := scanTable(r, "ASN", func(line int, fields []string) error {
		// fields[0]=CIDR, fields[1]=ASN, fields[2:]=description (ignored).
		if strings.HasPrefix(fields[1], "#") {
			return fmt.Errorf("auditxform: ASN table line %d: missing ASN after CIDR %q", line, fields[0])
		}
		entries = append(entries, GeoTableEntry{CIDR: fields[0], ASN: fields[1]})
		return nil
	}, 2)
	if err != nil {
		return nil, err
	}
	return NewGeoPrefixTableResolver(entries)
}

// ParseGeoTable reads a whitespace-delimited CIDR->{ASN, country} table and
// builds a resolver that carries both dimensions. The format, one entry per
// line:
//
//	<CIDR-or-IP> <ASN> <COUNTRY> [description...]
//	203.0.113.0/24   AS64512   US   EXAMPLE-NET
//	198.51.100.7     AS65000   DE   # a host route
//
// COUNTRY is the ISO 3166-1 alpha-2 code emitted verbatim onto the sourceCountry
// field. The literal token "None" (case-insensitive) — the sentinel the upstream
// iptoasn ip2asn+country dump uses for an unknown country — is normalised to an
// EMPTY country so the choropleth is not polluted by a bogus "None" bucket; such
// an address still resolves its ASN, it just omits sourceCountry (fail-open on
// geo, the same posture an unresolved IP takes).
//
// Blank lines and '#'-comment lines are skipped, as is any trailing description
// after the country token. A malformed line (missing CIDR/ASN/COUNTRY,
// unparseable CIDR) is an error naming the line number — a geo table is a
// security-adjacent input, so a typo fails loudly rather than silently
// under-covering.
//
// This offline CIDR form is what a per-env transport mounts; converting the
// upstream range-based dump to CIDRs is the artifact builder's job (scripts/build-geo-table.py), so
// this parser stays free of any bundled dataset or range-splitting logic.
func ParseGeoTable(r io.Reader) (*PrefixTableResolver, error) {
	var entries []GeoTableEntry
	err := scanTable(r, "geo", func(line int, fields []string) error {
		// fields[0]=CIDR, fields[1]=ASN, fields[2]=COUNTRY, fields[3:]=desc.
		if strings.HasPrefix(fields[1], "#") {
			return fmt.Errorf("auditxform: geo table line %d: missing ASN after CIDR %q", line, fields[0])
		}
		if strings.HasPrefix(fields[2], "#") {
			return fmt.Errorf("auditxform: geo table line %d: missing country after ASN %q", line, fields[1])
		}
		country := fields[2]
		// iptoasn emits "None" for ranges with no known country; treat it as
		// unknown (empty) so sourceCountry is omitted rather than bucketed as
		// the literal "None".
		if strings.EqualFold(country, "None") {
			country = ""
		}
		entries = append(entries, GeoTableEntry{CIDR: fields[0], ASN: fields[1], Country: country})
		return nil
	}, 3)
	if err != nil {
		return nil, err
	}
	return NewGeoPrefixTableResolver(entries)
}

// scanTable is the shared line reader for ParseASNTable/ParseGeoTable. It skips
// blank and '#'-comment lines, requires each remaining line to have at least
// minFields whitespace-separated fields (else an error naming kind + line), and
// invokes emit(line, fields) for each valid line. emit performs the format's
// column-specific validation and accumulation.
func scanTable(r io.Reader, kind string, emit func(line int, fields []string) error, minFields int) error {
	sc := bufio.NewScanner(r)
	// These tables can be large (a full ip2asn dump); raise the line cap.
	sc.Buffer(make([]byte, 0, 64*1024), 1*1024*1024)

	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Fields(text)
		if len(fields) < minFields {
			return fmt.Errorf("auditxform: %s table line %d: expected at least %d fields, got %q", kind, line, minFields, text)
		}
		if err := emit(line, fields); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("auditxform: read %s table: %w", kind, err)
	}
	return nil
}

// parsePrefix accepts either a CIDR ("10.0.0.0/8") or a bare IP treated as a
// host route ("10.1.2.3" -> /32, "2001:db8::1" -> /128). The prefix is masked to
// its network address so Contains behaves canonically.
func parsePrefix(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "/") {
		pfx, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("auditxform: bad CIDR %q: %w", s, err)
		}
		return pfx.Masked(), nil
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("auditxform: bad IP/CIDR %q: %w", s, err)
	}
	addr = addr.Unmap()
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}
