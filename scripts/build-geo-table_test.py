#!/usr/bin/env python3
"""Tests for scripts/build-geo-table.py (task ka-0000000583).

Hermetic: feeds synthetic iptoasn-TSV strings through the converter and asserts
the ParseGeoTable-format output, the range->CIDR split, and the drop/skip
accounting. No network.

Run: python3 scripts/build-geo-table_test.py
"""
from __future__ import annotations

import importlib.util
import io
import os
import unittest

_HERE = os.path.dirname(os.path.abspath(__file__))
_spec = importlib.util.spec_from_file_location(
    "bgt", os.path.join(_HERE, "build-geo-table.py")
)
bgt = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(bgt)


def _convert(tsv: str):
    stats = bgt.BuildStats()
    lines = list(bgt.convert(io.StringIO(tsv), stats))
    return lines, stats


class RangeToCIDRs(unittest.TestCase):
    def test_exact_cidr_range(self):
        self.assertEqual(bgt.range_to_cidrs("1.0.0.0", "1.0.0.255"), ["1.0.0.0/24"])

    def test_non_aligned_range_splits_to_minimal_set(self):
        # 1.0.0.0 - 1.0.1.255 is a clean /23; 1.0.0.0-1.0.0.127 a /25.
        self.assertEqual(bgt.range_to_cidrs("1.0.0.0", "1.0.1.255"), ["1.0.0.0/23"])
        self.assertEqual(bgt.range_to_cidrs("1.0.0.0", "1.0.0.127"), ["1.0.0.0/25"])

    def test_host_range(self):
        self.assertEqual(bgt.range_to_cidrs("9.9.9.9", "9.9.9.9"), ["9.9.9.9/32"])

    def test_v6_range(self):
        out = bgt.range_to_cidrs("2001:db8::", "2001:db8::ffff")
        self.assertEqual(out, ["2001:db8::/112"])

    def test_reversed_range_raises(self):
        with self.assertRaises(ValueError):
            bgt.range_to_cidrs("1.0.0.255", "1.0.0.0")


class Convert(unittest.TestCase):
    def test_basic_row_emits_geo_line(self):
        lines, stats = _convert("1.0.0.0\t1.0.0.255\t13335\tUS\tCLOUDFLARENET\n")
        self.assertEqual(lines, ["1.0.0.0/24 AS13335 US CLOUDFLARENET"])
        self.assertEqual(stats.rows_in, 1)
        self.assertEqual(stats.cidrs_out, 1)

    def test_row_without_description(self):
        lines, _ = _convert("203.0.113.0\t203.0.113.255\t64512\tDE\n")
        self.assertEqual(lines, ["203.0.113.0/24 AS64512 DE"])

    def test_multi_cidr_range_expands(self):
        # A range that is not a single aligned prefix expands to several lines,
        # each carrying the same ASN/country.
        lines, stats = _convert("1.0.0.0\t1.0.0.130\t7\tUS\tEX\n")
        self.assertGreater(len(lines), 1)
        for l in lines:
            self.assertTrue(l.startswith(("1.0.0",)))
            self.assertIn("AS7 US EX", l)
        self.assertEqual(stats.cidrs_out, len(lines))

    def test_as0_is_dropped(self):
        # rows_in counts every well-formed row (including the AS0 one); the AS0
        # row is dropped from OUTPUT, so cidrs_out stays 0 and dropped_as0 is 1.
        lines, stats = _convert("2.0.0.0\t2.0.0.255\t0\tNone\tNot routed\n")
        self.assertEqual(lines, [])
        self.assertEqual(stats.dropped_as0, 1)
        self.assertEqual(stats.cidrs_out, 0)

    def test_none_country_passed_through_verbatim(self):
        # The converter does not normalise "None" — ParseGeoTable does. So the
        # output must carry the literal token for a routed range with no geo.
        lines, _ = _convert("5.0.0.0\t5.0.0.255\t42\tNone\tSOMENET\n")
        self.assertEqual(lines, ["5.0.0.0/24 AS42 None SOMENET"])

    def test_comment_and_blank_lines_skipped(self):
        lines, stats = _convert("# header\n\n1.0.0.0\t1.0.0.255\t1\tUS\tA\n")
        self.assertEqual(lines, ["1.0.0.0/24 AS1 US A"])
        self.assertEqual(stats.rows_in, 1)

    def test_malformed_row_counted_and_skipped(self):
        # Too few columns, and an unparseable range.
        lines, stats = _convert("1.2.3.4\t5.6.7.8\n" + "not-an-ip\talso-bad\t9\tUS\tX\n")
        self.assertEqual(lines, [])
        self.assertEqual(stats.malformed, 2)

    def test_description_with_spaces_preserved(self):
        lines, _ = _convert("1.0.0.0\t1.0.0.255\t13335\tUS\tCLOUDFLARE NET INC\n")
        self.assertEqual(lines, ["1.0.0.0/24 AS13335 US CLOUDFLARE NET INC"])

    def test_output_roundtrips_through_parsegeotable_shape(self):
        # Each emitted line must have at least CIDR + ASN + COUNTRY tokens — the
        # minimum ParseGeoTable requires (>=3 fields).
        lines, _ = _convert("1.0.0.0\t1.0.0.255\t13335\tUS\tCLOUDFLARENET\n")
        for l in lines:
            self.assertGreaterEqual(len(l.split()), 3)


class NoDescription(unittest.TestCase):
    """--no-description drops the trailing AS-description column so the geo-table
    carrier image (gardenio/images/geo-table) stays small. The resolver ignores
    everything after the country column, so CIDR/ASN/country are unchanged."""

    def test_description_dropped(self):
        stats = bgt.BuildStats()
        lines = list(bgt.convert(
            io.StringIO("1.0.0.0\t1.0.0.255\t13335\tUS\tCLOUDFLARE NET INC\n"),
            stats, include_description=False))
        self.assertEqual(lines, ["1.0.0.0/24 AS13335 US"])

    def test_asn_and_country_preserved(self):
        # The detection group-keys (ASN, country) survive the drop verbatim.
        stats = bgt.BuildStats()
        lines = list(bgt.convert(
            io.StringIO("5.0.0.0\t5.0.0.255\t42\tNone\tSOMENET\n"),
            stats, include_description=False))
        self.assertEqual(lines, ["5.0.0.0/24 AS42 None"])

    def test_row_without_description_is_identical(self):
        # A row that carried no description already emits the 3-field form in both
        # modes — the flag only affects rows that HAVE a description.
        with_desc = list(bgt.convert(
            io.StringIO("203.0.113.0\t203.0.113.255\t64512\tDE\n"),
            bgt.BuildStats(), include_description=True))
        no_desc = list(bgt.convert(
            io.StringIO("203.0.113.0\t203.0.113.255\t64512\tDE\n"),
            bgt.BuildStats(), include_description=False))
        self.assertEqual(with_desc, no_desc)
        self.assertEqual(no_desc, ["203.0.113.0/24 AS64512 DE"])

    def test_run_no_description_header_and_body(self):
        out, err = io.StringIO(), io.StringIO()
        rc = bgt.run(io.StringIO("1.0.0.0\t1.0.0.255\t13335\tUS\tCF\n"),
                     out, err, include_description=False)
        self.assertEqual(rc, 0)
        body = out.getvalue()
        # Header advertises the 3-field format; the description must be absent.
        self.assertIn("# Format: <CIDR> AS<number> <COUNTRY>  ", body)
        self.assertIn("1.0.0.0/24 AS13335 US\n", body)
        self.assertNotIn("CF", body.split("\n", 3)[-1])


class Run(unittest.TestCase):
    def test_run_writes_header_and_reports_stats(self):
        out, err = io.StringIO(), io.StringIO()
        rc = bgt.run(io.StringIO("1.0.0.0\t1.0.0.255\t13335\tUS\tCF\n"), out, err)
        self.assertEqual(rc, 0)
        body = out.getvalue()
        self.assertIn("# CIDR->{ASN,country}", body)
        self.assertIn("1.0.0.0/24 AS13335 US CF", body)
        self.assertIn("cidrs_out=1", err.getvalue())

    def test_run_fails_when_nonempty_input_yields_no_cidrs(self):
        # Every row AS0 => zero output from non-empty input => hard failure so a
        # blank table is never published.
        out, err = io.StringIO(), io.StringIO()
        rc = bgt.run(io.StringIO("2.0.0.0\t2.0.0.255\t0\tNone\tNR\n"), out, err)
        self.assertEqual(rc, 1)
        self.assertIn("produced no CIDRs", err.getvalue())

    def test_run_empty_input_is_ok(self):
        out, err = io.StringIO(), io.StringIO()
        rc = bgt.run(io.StringIO(""), out, err)
        self.assertEqual(rc, 0)


if __name__ == "__main__":
    unittest.main()
