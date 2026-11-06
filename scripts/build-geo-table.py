#!/usr/bin/env python3
# build-geo-table.py
#
# Convert the upstream iptoasn "ip2asn" range dump into the CIDR->{ASN,country}
# table the traefik-log-shipper resolver consumes
# (auditxform.ParseGeoTable), for the edge IDP L1 geo enrichment (task
# ka-0000000583, design ka-0000007287#c3).
#
# WHY A CONVERTER, NOT THE RAW DUMP: the upstream iptoasn dump
# (https://iptoasn.com/data/ip2asn-*.tsv.gz) is RANGE-based — each row is
#     range_start<TAB>range_end<TAB>AS_number<TAB>country_code<TAB>AS_description
# e.g. `1.0.0.0  1.0.0.255  13335  US  CLOUDFLARENET`. The offline resolver does
# longest-PREFIX (CIDR) matching, not range matching (it keeps the resolver free
# of any range-splitting logic and identical to the ASN-only path). So the
# range->CIDR split is done ONCE here, at artifact-build time, rather than in the
# hot request path. Python's stdlib ipaddress.summarize_address_range emits the
# minimal set of CIDRs covering [start, end], so a single range becomes 1..N
# prefix lines.
#
# OUTPUT FORMAT (one entry per line, ParseGeoTable-compatible):
#     <CIDR> AS<number> <COUNTRY> <description...>
#     1.0.0.0/24 AS13335 US CLOUDFLARENET
# with a '#'-comment header naming the source + build provenance.
#
# DROPPED ROWS (a range is skipped, never emitted as a partial/blind entry):
#   - AS_number 0 — iptoasn's "not routed / unannounced" sentinel. It is not a
#     real origin AS; emitting it would bucket unrouted space under a bogus AS0.
#   The count of dropped rows is reported to stderr so a sudden spike in
#   unrouted space is visible rather than silent.
#
# COUNTRY: emitted verbatim (ISO 3166-1 alpha-2). iptoasn uses the literal
# "None" for ranges with no known country; ParseGeoTable normalises that to an
# empty country (sourceCountry omitted), so this script passes it through as-is
# rather than second-guessing the parser.
#
# Usage:
#   build-geo-table.py < ip2asn-v4.tsv > geo-table.txt
#   zcat ip2asn-combined.tsv.gz | build-geo-table.py > geo-table.txt
#
# Hermetic + streaming: reads stdin, writes stdout, no network. The geo-table
# image build (gardenio/images/geo-table/Dockerfile) fetches the upstream dump
# and runs this.
from __future__ import annotations

import ipaddress
import sys
from typing import Iterable, Iterator, TextIO


class BuildStats:
    """Counters reported to stderr so a build's shape is observable."""

    def __init__(self) -> None:
        self.rows_in = 0
        self.cidrs_out = 0
        self.dropped_as0 = 0
        self.malformed = 0


def range_to_cidrs(start: str, end: str) -> list[str]:
    """Return the minimal set of CIDRs exactly covering [start, end].

    Raises ValueError on an unparseable/reversed range so the caller can count
    and skip it rather than emit a partial entry.
    """
    a = ipaddress.ip_address(start)
    b = ipaddress.ip_address(end)
    if a.version != b.version:
        raise ValueError(f"mixed IP versions in range {start}-{end}")
    if int(b) < int(a):
        raise ValueError(f"reversed range {start}-{end}")
    return [str(net) for net in ipaddress.summarize_address_range(a, b)]


def convert(lines: Iterable[str], stats: BuildStats, include_description: bool = True) -> Iterator[str]:
    """Yield ParseGeoTable-format output lines for the iptoasn TSV in `lines`.

    Blank lines and '#'-comment lines are passed over. A row with fewer than 4
    tab-separated columns, or an unparseable range, is counted as malformed and
    skipped (never emitted as a partial entry). AS 0 rows are counted and
    dropped. The AS number is emitted with the "AS" prefix the detections and
    the resolver treat as an opaque token.

    include_description=False drops the trailing AS-description column. The
    offline resolver (auditxform.ParseGeoTable) IGNORES everything after the
    country column, so the emitted table is byte-equivalent for lookup purposes,
    and its in-memory footprint is IDENTICAL — but the on-disk/on-wire table
    roughly halves (measured: ~49.5 MiB -> ~26.4 MiB on the full combined dump).
    This is what makes the geo-table carrier image (gardenio/images/geo-table)
    small enough to pull cheaply on every shipper pod start. The country/ASN
    dimensions the detections group by are fully preserved.
    """
    for raw in lines:
        line = raw.rstrip("\n")
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        # iptoasn is strictly tab-separated; the description may contain spaces
        # so split on TAB, not arbitrary whitespace.
        cols = line.split("\t")
        if len(cols) < 4:
            stats.malformed += 1
            continue
        stats.rows_in += 1
        start, end, asn_raw, country = cols[0].strip(), cols[1].strip(), cols[2].strip(), cols[3].strip()
        desc = cols[4].strip() if len(cols) >= 5 else ""

        if asn_raw == "0":
            # Not routed — not a real origin AS.
            stats.dropped_as0 += 1
            continue

        try:
            cidrs = range_to_cidrs(start, end)
        except ValueError:
            stats.malformed += 1
            continue

        asn = f"AS{asn_raw}"
        for cidr in cidrs:
            stats.cidrs_out += 1
            # Country + description are already whitespace-free (country) or
            # trailing free-form (description, which ParseGeoTable ignores after
            # the country column), so a single space join is safe.
            if desc and include_description:
                yield f"{cidr} {asn} {country} {desc}"
            else:
                yield f"{cidr} {asn} {country}"


def run(src: TextIO, out: TextIO, err: TextIO, include_description: bool = True) -> int:
    stats = BuildStats()
    fmt = "<CIDR> AS<number> <COUNTRY> [description]" if include_description else "<CIDR> AS<number> <COUNTRY>"
    out.write("# CIDR->{ASN,country} geo table for the edge IDP resolver.\n")
    out.write("# Generated by scripts/build-geo-table.py from the iptoasn ip2asn dump.\n")
    out.write("# Format: %s  (auditxform.ParseGeoTable)\n" % fmt)
    for line in convert(src, stats, include_description=include_description):
        out.write(line)
        out.write("\n")
    err.write(
        "build-geo-table: rows_in=%d cidrs_out=%d dropped_as0=%d malformed=%d\n"
        % (stats.rows_in, stats.cidrs_out, stats.dropped_as0, stats.malformed)
    )
    # A run that produced zero CIDRs from a non-empty input is a hard failure —
    # publishing an empty geo table would silently blind the console.
    if stats.rows_in > 0 and stats.cidrs_out == 0:
        err.write("build-geo-table: ERROR produced no CIDRs from a non-empty input\n")
        return 1
    return 0


if __name__ == "__main__":
    # --no-description drops the trailing AS-description column (the resolver
    # ignores it; halves the output size). Any other arg is a usage error rather
    # than a silently-ignored flag.
    include_description = True
    for arg in sys.argv[1:]:
        if arg == "--no-description":
            include_description = False
        else:
            sys.stderr.write("build-geo-table: unknown argument %r (only --no-description)\n" % arg)
            sys.exit(2)
    sys.exit(run(sys.stdin, sys.stdout, sys.stderr, include_description=include_description))
