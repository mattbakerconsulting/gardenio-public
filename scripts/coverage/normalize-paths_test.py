#!/usr/bin/env python3
"""Unit tests for normalize-paths.py.

These cover the path-rewrite table, the exclusion list, and the
deterministic streaming behaviour required for round-tripping merged
LCOV through `lcov --summary` (verified by the smoke test in
scripts/coverage/merge_test.sh).
"""
import io
import os
import subprocess
import sys
import unittest
from importlib import util as _imputil

HERE = os.path.dirname(os.path.abspath(__file__))
NORMALIZER_PATH = os.path.join(HERE, "normalize-paths.py")

_spec = _imputil.spec_from_file_location("normalize_paths", NORMALIZER_PATH)
_mod = _imputil.module_from_spec(_spec)
_spec.loader.exec_module(_mod)  # type: ignore[union-attr]


def run_normalize(src: str, *, workspace_root=None, excludes=None,
                  no_default_excludes=False, strict=False) -> str:
    out = io.StringIO()
    excludes = list(excludes) if excludes else []
    if not no_default_excludes:
        excludes = _mod.DEFAULT_EXCLUDES + excludes
    for chunk in _mod.normalize_lcov(
        io.StringIO(src),
        workspace_root=workspace_root,
        excludes=excludes,
        prefixes=_mod.KNOWN_PREFIXES,
        strict=strict,
    ):
        out.write(chunk)
    return out.getvalue()


def make_record(sf: str, *, lf=3, lh=2, fnf=1, fnh=1, da_lines=None) -> str:
    da_lines = da_lines or [(1, 1), (2, 1), (3, 0)]
    da = "\n".join(f"DA:{ln},{hits}" for ln, hits in da_lines)
    return (
        f"TN:\nSF:{sf}\nFN:1,Foo\nFNDA:1,Foo\nFNF:{fnf}\nFNH:{fnh}\n"
        f"{da}\nLF:{lf}\nLH:{lh}\nend_of_record\n"
    )


class TestPathRewrites(unittest.TestCase):
    def test_kbeads_module_path(self):
        self.assertEqual(
            _mod.normalize_path("gardenio/kbeads/internal/server/server.go", None),
            "kbeads/internal/server/server.go",
        )

    def test_gardenio_github_origin_module(self):
        self.assertEqual(
            _mod.normalize_path("github.com/mattbakerconsulting/gardenio/gardenio/controller/x.go", None),
            "gardenio/controller/x.go",
        )

    def test_gardenio_github_module(self):
        self.assertEqual(
            _mod.normalize_path("github.com/Acme/gardenio/gardenio/controller/x.go", None),
            "gardenio/controller/x.go",
        )

    def test_gardenio_controller_passthrough(self):
        self.assertEqual(
            _mod.normalize_path("gardenio/controller/internal/foo.go", None),
            "gardenio/controller/internal/foo.go",
        )

    def test_workspace_root_strip(self):
        self.assertEqual(
            _mod.normalize_path(
                "/home/agent/bot/gardenio/work/coop/crates/cli/src/main.rs",
                workspace_root="/home/agent/bot/gardenio/work",
            ),
            "coop/crates/cli/src/main.rs",
        )

    def test_workspace_root_with_trailing_slash(self):
        self.assertEqual(
            _mod.normalize_path(
                "/builds/x/y/coop/crates/cli/src/main.rs",
                workspace_root="/builds/x/y/",
            ),
            "coop/crates/cli/src/main.rs",
        )

    def test_dot_slash_prefix_stripped(self):
        self.assertEqual(
            _mod.normalize_path("./gardenio/controller/foo.go", None),
            "gardenio/controller/foo.go",
        )

    def test_backslashes_normalized(self):
        self.assertEqual(
            _mod.normalize_path("gardenio\\controller\\x.go", None),
            "gardenio/controller/x.go",
        )

    def test_idempotent(self):
        once = _mod.normalize_path("gardenio/controller/foo.go", "/home/agent")
        twice = _mod.normalize_path(once, "/home/agent")
        self.assertEqual(once, twice)


class TestExclusion(unittest.TestCase):
    def setUp(self):
        self.excl = _mod.DEFAULT_EXCLUDES

    def test_excludes_pb_go(self):
        self.assertTrue(_mod.is_excluded("kbeads/gen/foo.pb.go", self.excl))

    def test_excludes_test_go(self):
        self.assertTrue(_mod.is_excluded("gardenio/controller/internal/x_test.go", self.excl))

    def test_excludes_proto_dir(self):
        self.assertTrue(_mod.is_excluded("coop/proto/coop.proto", self.excl))

    def test_excludes_gen_dir(self):
        self.assertTrue(_mod.is_excluded("kbeads/internal/gen/queries.go", self.excl))

    def test_excludes_dist_dir(self):
        self.assertTrue(_mod.is_excluded("beads3d/dist/main.js", self.excl))

    def test_excludes_node_modules(self):
        self.assertTrue(_mod.is_excluded("beads3d/node_modules/lib.js", self.excl))

    def test_excludes_target_dir(self):
        self.assertTrue(_mod.is_excluded("coop/target/release/build.rs", self.excl))

    def test_excludes_tests_dir(self):
        self.assertTrue(_mod.is_excluded("gardenio/controller/tests/integration.go", self.excl))

    def test_excludes_fixtures(self):
        self.assertTrue(_mod.is_excluded("kbeads/fixtures/sample.json", self.excl))

    def test_excludes_testdata(self):
        self.assertTrue(_mod.is_excluded("gardenio/controller/testdata/sample.txt", self.excl))

    def test_excludes_cmd_probe(self):
        # e2e-only probe binaries (ka-0000007875): built only for e2e
        # runs, never shipped in a prod image, and MR-event
        # coverage runs are unit-only so their patch coverage is
        # structurally 0%. Mirrors exclusions_test.go.
        self.assertTrue(_mod.is_excluded("gardenio/controller/cmd/jira-conv-probe/main.go", self.excl))
        self.assertTrue(_mod.is_excluded("gardenio/controller/cmd/gitlab-prov-probe/main.go", self.excl))

    def test_keeps_cmd_prod_binary(self):
        # Control: the "-probe" suffix is load-bearing — a real prod
        # binary under cmd/ must NOT be exempted. An over-broad
        # "**/cmd/**" would silently drop the whole controller.
        self.assertFalse(_mod.is_excluded("gardenio/controller/cmd/gb/jira.go", self.excl))
        self.assertFalse(_mod.is_excluded("gardenio/controller/cmd/slack-bridge/main.go", self.excl))

    def test_keeps_real_source(self):
        self.assertFalse(_mod.is_excluded("gardenio/controller/internal/podmanager/pods.go", self.excl))

    def test_keeps_kbeads_source(self):
        self.assertFalse(_mod.is_excluded("kbeads/internal/server/server.go", self.excl))

    def test_keeps_coop_source(self):
        self.assertFalse(_mod.is_excluded("coop/crates/cli/src/main.rs", self.excl))


class TestAllowlist(unittest.TestCase):
    def test_allows_components(self):
        for p in ("gardenio/x.go", "kbeads/y.go", "coop/z.rs"):
            self.assertTrue(_mod.is_allowed(p, _mod.KNOWN_PREFIXES))

    def test_rejects_absolute(self):
        self.assertFalse(_mod.is_allowed("/usr/lib/foo.go", _mod.KNOWN_PREFIXES))

    def test_rejects_unknown_prefix(self):
        self.assertFalse(_mod.is_allowed("vendor/foo.go", _mod.KNOWN_PREFIXES))


class TestStreamNormalization(unittest.TestCase):
    def test_drops_excluded_record(self):
        src = make_record("gardenio/controller/x.go") + make_record("kbeads/gen/q.pb.go")
        out = run_normalize(src)
        self.assertIn("SF:gardenio/controller/x.go", out)
        self.assertNotIn("kbeads/gen/q.pb.go", out)
        # Exactly one end_of_record remains.
        self.assertEqual(out.count("end_of_record"), 1)

    def test_rewrites_module_path(self):
        src = make_record("gardenio/kbeads/internal/server/server.go")
        out = run_normalize(src)
        self.assertIn("SF:kbeads/internal/server/server.go", out)
        self.assertNotIn("gardenio/kbeads/", out)

    def test_workspace_root_strip(self):
        src = make_record("/home/agent/bot/gardenio/work/coop/crates/cli/src/main.rs")
        out = run_normalize(src, workspace_root="/home/agent/bot/gardenio/work")
        self.assertIn("SF:coop/crates/cli/src/main.rs", out)

    def test_preserves_da_records_in_kept_file(self):
        src = make_record("gardenio/controller/foo.go", lf=4, lh=3,
                          da_lines=[(1, 5), (2, 5), (3, 0), (4, 1)])
        out = run_normalize(src)
        self.assertIn("DA:1,5", out)
        self.assertIn("DA:4,1", out)
        self.assertIn("LF:4", out)
        self.assertIn("LH:3", out)

    def test_deterministic_output(self):
        src = (
            make_record("gardenio/controller/a.go") +
            make_record("kbeads/gen/q.pb.go") +
            make_record("kbeads/internal/x.go") +
            make_record("coop/crates/cli/src/main.rs")
        )
        first = run_normalize(src)
        second = run_normalize(src)
        self.assertEqual(first, second)

    def test_handles_collision_safe_paths(self):
        # Same canonical path arriving via two upstream emitters must
        # collapse to the same SF: line so that lcov -a sums them.
        src = (
            make_record("gardenio/kbeads/internal/x.go") +
            make_record("/home/agent/bot/gardenio/work/kbeads/internal/x.go")
        )
        out = run_normalize(src, workspace_root="/home/agent/bot/gardenio/work")
        self.assertEqual(out.count("SF:kbeads/internal/x.go"), 2)

    def test_unrecognized_prefix_dropped_in_strict(self):
        src = make_record("/usr/lib/golang/runtime.go")
        out = run_normalize(src, strict=True)
        self.assertEqual(out.strip(), "")

    def test_unrecognized_prefix_kept_when_not_strict(self):
        src = make_record("/usr/lib/golang/runtime.go")
        out = run_normalize(src, strict=False)
        self.assertIn("end_of_record", out)


class TestCLI(unittest.TestCase):
    def test_cli_round_trip(self):
        src = (
            make_record("gardenio/kbeads/internal/server/server.go") +
            make_record("kbeads/gen/queries.pb.go") +
            make_record("gardenio/controller/internal/podmanager/pods.go")
        )
        proc = subprocess.run(
            [sys.executable, NORMALIZER_PATH, "--workspace-root", "/tmp/repo"],
            input=src, capture_output=True, text=True, check=True,
        )
        self.assertIn("SF:kbeads/internal/server/server.go", proc.stdout)
        self.assertIn("SF:gardenio/controller/internal/podmanager/pods.go", proc.stdout)
        self.assertNotIn("kbeads/gen/queries.pb.go", proc.stdout)


if __name__ == "__main__":
    unittest.main(verbosity=2)
