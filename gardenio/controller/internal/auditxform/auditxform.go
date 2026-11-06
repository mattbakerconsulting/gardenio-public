// Package auditxform is the edge log-field contract: the field names an L0 edge
// shipper (internal/traefikshipper) writes into VictoriaLogs, and the resolvers
// that map a source IP to its ASN and country. The edge geomap dashboard and the
// L2 edge detections (design ka-0000007287) query these names, so a shipper and
// its consumers agree on them here.
//
// The resolver is a pluggable interface, not a baked-in geo database: the data
// is an operator-provided CIDR->ASN/country table (the offline ip2asn form, see
// PrefixTableResolver / ParseASNTable / ParseGeoTable in asn.go). This package
// performs no network lookups.
package auditxform

// Field names of the edge log-field contract. They MUST match the defaults of
// the edge geomap dashboard's field variables, or its panels query a field the
// shipper never wrote.
const (
	// FieldClientIP is the scalar, ipv4_range-queryable source IP.
	FieldClientIP = "client_ip"
	// FieldSourceASN is the enriched scalar ASN field the edge geomap console
	// groups by. It is written ONLY when an ASNResolver resolves the row's
	// client_ip; otherwise it is absent (the record still ingests — a missing
	// ASN never drops or suppresses a row).
	FieldSourceASN = "sourceASN"
	// FieldSourceCountry is the enriched scalar ISO 3166-1 alpha-2 country field
	// the edge geomap console groups by (request choropleth) and the L2
	// novel-geo-for-known-principal detections key on. It is written ONLY when
	// the resolver is a GeoResolver AND resolves the row's client_ip to a
	// NON-EMPTY country; otherwise it is absent (a missing country never drops or
	// suppresses a row, and an ASN-only resolver never writes it — additive to
	// the pre-geo sourceASN contract).
	FieldSourceCountry = "sourceCountry"
	// FieldPrincipal is the surface-agnostic authenticated-principal field of the
	// normalized edge event (design ka-0000007287#c5: {ts, client_ip, principal,
	// host, path, status, ua, geo, asn}). It is the group key for the geomap
	// console's post-compromise panels ("successful authenticated requests by
	// principal × country × ASN" and "novel geo for a known principal") and the
	// L2 novel-geo-for-known-principal detection.
	//
	// Each L0 shipper is responsible for lifting its surface's identity into this
	// ONE contract field so a single geomap f_principal variable and a single
	// detection rule work across every edge surface: the Traefik adapter
	// carries the access log's ClientUsername (traefikshipper).
	//
	// It is stamped on EVERY row: an authenticated identity when known, otherwise
	// the sentinel PrincipalUnauthenticated ("-", the Traefik ClientUsername
	// convention) — NEVER an empty string. LogsQL's `field:*` matches only a
	// NON-EMPTY value, and the geomap scopes every panel by `${f_principal}:*`;
	// stamping "-" (not "") keeps the volume/choropleth panels binding every row
	// while still letting the post-compromise panels filter the sentinel out,
	// rather than blanking every panel the way a wholly-absent field did
	// (ka-0000008216 / ka-0000005007).
	FieldPrincipal = "principal"
	// PrincipalUnauthenticated is the sentinel stamped into FieldPrincipal when a
	// row carries no authenticated identity. It mirrors the Traefik access log's
	// "-" ClientUsername convention so both edge surfaces read identically and the
	// geomap's `${f_principal}:*` scope keeps matching the row.
	PrincipalUnauthenticated = "-"
)

// ASNResolver maps a source IP to the Autonomous System Number that originates
// it. It is the injection point for the ASN data source (an offline CIDR->ASN
// table, a geo database, or a per-env stub) so this package stays free of any
// bundled dataset.
//
// Implementations MUST be safe for concurrent use: a shipper enriches records
// from multiple goroutines against one shared resolver.
type ASNResolver interface {
	// LookupASN returns the ASN token for ip (whatever form the data source
	// uses — e.g. "AS64512" or "64512", emitted verbatim) and true when the IP
	// is found. It returns ("", false) for an empty/unparseable IP or an IP the
	// data source does not cover; the caller then omits the sourceASN field for
	// that row.
	LookupASN(ip string) (string, bool)
}

// Geo is the resolved origin of a source IP: its ASN and ISO 3166-1 alpha-2
// country. ASN is always non-empty for a covered IP; Country may be empty when
// the data source has no geo for that prefix (an ASN-only table, or a covered
// range the upstream dump marks unknown), in which case the caller omits the
// sourceCountry field.
type Geo struct {
	ASN     string
	Country string
}

// GeoResolver additionally maps a source IP to its country. It is an OPTIONAL
// extension of ASNResolver: a shipper type-asserts its resolver to GeoResolver
// and, when the assertion holds, also emits sourceCountry. A resolver that
// implements only ASNResolver keeps the pre-geo behaviour (no sourceCountry),
// so the country dimension is purely additive and enrichment stays free of any
// bundled geo dataset.
//
// Implementations MUST be safe for concurrent use.
type GeoResolver interface {
	ASNResolver
	// LookupGeo returns the {ASN, country} for ip and true when the IP is found.
	// It returns (Geo{}, false) for an empty/unparseable IP or an uncovered IP.
	// A covered IP always yields a non-empty ASN but MAY yield an empty Country
	// (geo unknown); the caller then omits only the sourceCountry field.
	LookupGeo(ip string) (Geo, bool)
}
