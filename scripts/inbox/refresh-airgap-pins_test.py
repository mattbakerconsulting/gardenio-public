#!/usr/bin/env python3
"""Unit tests for refresh-airgap-pins.py (ka-0000004971).

Covers the load-bearing invariants of the air-gap first-party digest-pin
refresh/check tool:

  * find_pins attributes every `digest:` line to its correct parent key-path,
    across the two-space nested layout of the real values-inbox.yaml;
  * --check exits 0 when every pin matches the registry digest for the chart
    appVersion, and exits 1 (listing the drifted pins) when any has rotted —
    the exact ka-0000000443 silent-drift failure this guard catches;
  * --write rewrites ONLY the drifted digest values, byte-for-byte preserving
    every comment, blank line, ordering, and quoting style in the file;
  * an unmapped pin or a missing first-party block fails loud (exit 2);
  * appVersion is read from Chart.yaml (quoted or bare).

crane is mocked as a tiny script on PATH (matching the sibling script tests
under scripts/), so no registry / network is touched.
"""
import os
import subprocess
import sys
import tempfile
import textwrap
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
TOOL = os.path.join(HERE, "refresh-airgap-pins.py")

# The 11 first-party components, in the file's order, with distinct fake digests.
COMPONENTS = [
    ("agents.image", "controller"),
    ("agents.agentImage", "agent"),
    ("coopmux.image", "coopmux"),
    ("beads.image", "kbeads"),
    ("beadsViewer.image", "beads-viewer"),
    ("slackBridge.image", "slack-bridge"),
    ("jiraBridge.image", "jira-bridge"),
    ("gitlabBridge.image", "gitlab-bridge"),
    ("triggerEngine.image", "trigger-engine"),
    ("edgeAuth.image", "edge-auth"),
]


def _fake_digest(component):
    """A deterministic, valid-looking sha256 for a component (the 'registry' truth)."""
    h = "".join("%02x" % ((ord(c) * 7 + 3) % 256) for c in component)
    return "sha256:" + (h * 4)[:64]


class RefreshPinsTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()

    def tearDown(self):
        import shutil

        shutil.rmtree(self.tmp, ignore_errors=True)

    # ── fixtures ────────────────────────────────────────────────────────────
    def _write_crane(self, mapping):
        """Write a fake `crane` on PATH that returns mapping[component] for
        `crane digest <registry>/<component>:<tag>`; exit 1 for unknown refs."""
        crane_dir = os.path.join(self.tmp, "bin")
        os.makedirs(crane_dir, exist_ok=True)
        crane = os.path.join(crane_dir, "crane")
        # mapping: component -> digest
        cases = "\n".join(
            '    */%s:*) echo "%s" ;;' % (comp, dig) for comp, dig in mapping.items()
        )
        with open(crane, "w") as fh:
            fh.write(
                textwrap.dedent(
                    """\
                    #!/usr/bin/env bash
                    # fake crane: only `crane digest <ref>` is implemented
                    if [ "$1" != "digest" ]; then echo "unsupported: $*" >&2; exit 2; fi
                    ref="$2"
                    case "$ref" in
                    %s
                        *) echo "no such tag: $ref" >&2; exit 1 ;;
                    esac
                    """
                )
                % cases
            )
        os.chmod(crane, 0o755)
        return crane_dir

    def _write_chart(self, app_version='"2026.238.7"'):
        chart = os.path.join(self.tmp, "Chart.yaml")
        with open(chart, "w") as fh:
            fh.write("apiVersion: v2\nname: gardenio\nversion: 2026.238.7\nappVersion: %s\n" % app_version)
        return chart

    def _write_values(self, digests):
        """Write a values-inbox-shaped file with the given {path: digest} pins,
        interleaved with comments/blank lines to prove preservation."""
        # digests keyed by path
        def d(path):
            return digests[path]

        content = textwrap.dedent(
            """\
            # header comment (must survive)
            global:
              airgap: true

            # controller + agent pins live under agents: (not global:)
            agents:
              agentPullPolicy: "IfNotPresent"
              image:
                digest: "%s"
              agentImage:
                digest: "%s"

            # secrets axis
            coopmux:
              credentialKey:
                secretName: "coop-credential-key"
              image:
                digest: "%s"

            # ── air-gap first-party pins ──
            beads:
              image:
                digest: "%s"

            beadsViewer:
              image:
                digest: "%s"

            slackBridge:
              image:
                digest: "%s"

            jiraBridge:
              image:
                digest: "%s"

            gitlabBridge:
              image:
                digest: "%s"

            triggerEngine:
              image:
                digest: "%s"

            edgeAuth:
              image:
                digest: "%s"

            # trailing comment (must survive)
            """
        ) % tuple(d(path) for path, _ in COMPONENTS)
        values = os.path.join(self.tmp, "values-inbox.yaml")
        with open(values, "w") as fh:
            fh.write(content)
        return values

    def _run(self, *args, crane_dir=None):
        env = dict(os.environ)
        if crane_dir:
            env["PATH"] = crane_dir + os.pathsep + env["PATH"]
        return subprocess.run(
            [sys.executable, TOOL, *args],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            env=env,
        )

    # ── tests ───────────────────────────────────────────────────────────────
    def test_find_pins_paths(self):
        """find_pins attributes each digest to its correct parent path."""
        sys.path.insert(0, HERE)
        import importlib.util

        spec = importlib.util.spec_from_file_location("rap", TOOL)
        rap = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(rap)
        truth = {path: _fake_digest(comp) for path, comp in COMPONENTS}
        values = self._write_values(truth)
        with open(values) as fh:
            lines = fh.readlines()
        pins = rap.find_pins(lines)
        got = {p["path"]: p["value"] for p in pins}
        self.assertEqual(got, truth)
        # coopmux.credentialKey.secretName must NOT be mistaken for a pin
        self.assertNotIn("coopmux.credentialKey", got)

    def test_check_ok_when_matching(self):
        truth = {path: _fake_digest(comp) for path, comp in COMPONENTS}
        values = self._write_values(truth)
        chart = self._write_chart()
        crane_dir = self._write_crane({comp: _fake_digest(comp) for _, comp in COMPONENTS})
        r = self._run("--check", "--values", values, "--chart", chart, crane_dir=crane_dir)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("OK", r.stdout)

    def test_check_fails_on_drift(self):
        # pin controller/kbeads at a stale value; registry has the fresh one
        truth = {path: _fake_digest(comp) for path, comp in COMPONENTS}
        truth["agents.image"] = "sha256:" + "0" * 64
        truth["beads.image"] = "sha256:" + "0" * 64
        values = self._write_values(truth)
        chart = self._write_chart()
        crane_dir = self._write_crane({comp: _fake_digest(comp) for _, comp in COMPONENTS})
        r = self._run("--check", "--values", values, "--chart", chart, crane_dir=crane_dir)
        self.assertEqual(r.returncode, 1, r.stdout + r.stderr)
        self.assertIn("DRIFT", r.stderr)
        self.assertIn("controller", r.stderr)
        self.assertIn("kbeads", r.stderr)

    def test_default_mode_is_check(self):
        truth = {path: _fake_digest(comp) for path, comp in COMPONENTS}
        truth["edgeAuth.image"] = "sha256:" + "1" * 64
        values = self._write_values(truth)
        chart = self._write_chart()
        crane_dir = self._write_crane({comp: _fake_digest(comp) for _, comp in COMPONENTS})
        r = self._run("--values", values, "--chart", chart, crane_dir=crane_dir)
        self.assertEqual(r.returncode, 1, "default mode must behave like --check")

    def test_write_only_touches_drifted_values(self):
        truth = {path: _fake_digest(comp) for path, comp in COMPONENTS}
        stale = {**truth, "agents.image": "sha256:" + "a" * 64,
                 "triggerEngine.image": "sha256:" + "b" * 64}
        values = self._write_values(stale)
        chart = self._write_chart()
        crane_dir = self._write_crane({comp: _fake_digest(comp) for _, comp in COMPONENTS})

        with open(values) as fh:
            before = fh.read()
        r = self._run("--write", "--values", values, "--chart", chart, crane_dir=crane_dir)
        self.assertEqual(r.returncode, 0, r.stderr)
        with open(values) as fh:
            after = fh.read()

        # every non-digest line is byte-identical
        b_lines, a_lines = before.splitlines(), after.splitlines()
        self.assertEqual(len(b_lines), len(a_lines))
        changed = [(b, a) for b, a in zip(b_lines, a_lines) if b != a]
        # exactly the two drifted digest lines changed
        self.assertEqual(len(changed), 2, changed)
        for b, a in changed:
            self.assertIn("digest:", b)
            self.assertIn("digest:", a)
        # comments preserved
        self.assertIn("# header comment (must survive)", after)
        self.assertIn("# trailing comment (must survive)", after)
        self.assertIn("# ── air-gap first-party pins ──", after)

        # a second --write is a no-op (now all match)
        r2 = self._run("--write", "--values", values, "--chart", chart, crane_dir=crane_dir)
        self.assertEqual(r2.returncode, 0, r2.stderr)
        self.assertIn("no change", r2.stdout)

    def test_write_then_check_roundtrips(self):
        truth = {path: _fake_digest(comp) for path, comp in COMPONENTS}
        stale = {**truth, "beads.image": "sha256:" + "c" * 64}
        values = self._write_values(stale)
        chart = self._write_chart()
        crane_dir = self._write_crane({comp: _fake_digest(comp) for _, comp in COMPONENTS})
        self.assertEqual(self._run("--write", "--values", values, "--chart", chart, crane_dir=crane_dir).returncode, 0)
        self.assertEqual(self._run("--check", "--values", values, "--chart", chart, crane_dir=crane_dir).returncode, 0)

    def test_unmapped_pin_fails_loud(self):
        # inject an extra digest under an unknown parent
        truth = {path: _fake_digest(comp) for path, comp in COMPONENTS}
        values = self._write_values(truth)
        with open(values, "a") as fh:
            fh.write("\nmysteryImage:\n  image:\n    digest: \"sha256:%s\"\n" % ("d" * 64))
        chart = self._write_chart()
        crane_dir = self._write_crane({comp: _fake_digest(comp) for _, comp in COMPONENTS})
        r = self._run("--check", "--values", values, "--chart", chart, crane_dir=crane_dir)
        self.assertEqual(r.returncode, 2, r.stdout + r.stderr)
        self.assertIn("unmapped", r.stderr)

    def test_missing_block_fails_loud(self):
        # drop the edgeAuth pin entirely — a first-party image lost its pin
        truth = {path: _fake_digest(comp) for path, comp in COMPONENTS}
        values = self._write_values(truth)
        with open(values) as fh:
            text = fh.read()
        text = text.replace(
            "edgeAuth:\n  image:\n    digest: \"%s\"\n" % truth["edgeAuth.image"], ""
        )
        with open(values, "w") as fh:
            fh.write(text)
        chart = self._write_chart()
        crane_dir = self._write_crane({comp: _fake_digest(comp) for _, comp in COMPONENTS})
        r = self._run("--check", "--values", values, "--chart", chart, crane_dir=crane_dir)
        self.assertEqual(r.returncode, 2, r.stdout + r.stderr)
        self.assertIn("edge-auth", r.stderr)

    def test_crane_failure_is_fatal(self):
        truth = {path: _fake_digest(comp) for path, comp in COMPONENTS}
        values = self._write_values(truth)
        chart = self._write_chart()
        # crane knows nothing → every ref fails
        crane_dir = self._write_crane({})
        r = self._run("--check", "--values", values, "--chart", chart, crane_dir=crane_dir)
        self.assertEqual(r.returncode, 2, r.stdout + r.stderr)
        self.assertIn("crane digest", r.stderr)

    def test_appversion_bare_and_quoted(self):
        sys.path.insert(0, HERE)
        import importlib.util

        spec = importlib.util.spec_from_file_location("rap2", TOOL)
        rap = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(rap)
        self.assertEqual(rap.read_app_version(self._write_chart('"2026.238.7"')), "2026.238.7")
        self.assertEqual(rap.read_app_version(self._write_chart("2026.238.7")), "2026.238.7")

    def test_tag_override(self):
        """--tag overrides Chart.AppVersion (bundle cutter pins a specific calver)."""
        truth = {path: _fake_digest(comp) for path, comp in COMPONENTS}
        values = self._write_values(truth)
        chart = self._write_chart()
        crane_dir = self._write_crane({comp: _fake_digest(comp) for _, comp in COMPONENTS})
        # the fake crane ignores the tag, so any override still resolves
        r = self._run("--check", "--tag", "2026.999.0", "--values", values, "--chart", chart, crane_dir=crane_dir)
        self.assertEqual(r.returncode, 0, r.stderr)


if __name__ == "__main__":
    unittest.main()
