#!/usr/bin/env python3
"""Tests for scripts/check-gardener-prompt-drift.py (ka-0000000874).

Hermetic: spins up a throwaway HTTP server that answers GET /v1/beads/<id>
with a canned .fields.prompt, and builds a temp repo tree with a manifest +
.md files. No live daemon required. Run: python3 scripts/check-gardener-prompt-drift_test.py
"""
from __future__ import annotations

import json
import os
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import importlib.util

_spec = importlib.util.spec_from_file_location(
    "drift", os.path.join(os.path.dirname(os.path.abspath(__file__)), "check-gardener-prompt-drift.py")
)
drift = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(drift)


class _Handler(BaseHTTPRequestHandler):
    # class attribute set per-test: {bead_id: prompt_string}
    prompts: dict = {}

    def log_message(self, *args):  # silence
        pass

    def do_GET(self):
        # /v1/beads/<id>
        parts = self.path.strip("/").split("/")
        if len(parts) == 3 and parts[0] == "v1" and parts[1] == "beads":
            bead = parts[2]
            if bead in self.prompts:
                body = json.dumps({"id": bead, "fields": {"prompt": self.prompts[bead]}}).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(body)
                return
        self.send_response(404)
        self.end_headers()


class DriftCheckTest(unittest.TestCase):
    def _serve(self, prompts: dict):
        _Handler.prompts = prompts
        srv = HTTPServer(("127.0.0.1", 0), _Handler)
        t = threading.Thread(target=srv.serve_forever, daemon=True)
        t.start()
        self.addCleanup(srv.server_close)
        self.addCleanup(srv.shutdown)
        return f"http://127.0.0.1:{srv.server_address[1]}"

    def _repo(self, md: dict, runbooks: dict | None = None):
        """Build a temp repo with a manifest.

        md: {name: body} — inline-copy gardeners, one gardener-prompts/<name>.md each.
        runbooks: {name: (runbook_rel, write_file_bool)} — runbook-delegating
            gardeners. Each gets a manifest row with a "runbook" key; the runbook
            file is created under repo_root/<runbook_rel> unless write_file_bool
            is False (used to exercise the missing-runbook ERROR path)."""
        root = tempfile.mkdtemp()
        pdir = os.path.join(root, drift.PROMPTS_DIR_REL)
        os.makedirs(pdir)
        gardeners = []
        for name, body in md.items():
            with open(os.path.join(pdir, f"{name}.md"), "w", encoding="utf-8") as f:
                f.write(body)
            gardeners.append({"name": name, "bead": f"gd-{name}"})
        for name, (runbook_rel, write_file) in (runbooks or {}).items():
            if write_file:
                rb_path = os.path.join(root, runbook_rel)
                os.makedirs(os.path.dirname(rb_path), exist_ok=True)
                with open(rb_path, "w", encoding="utf-8") as f:
                    f.write("# runbook body\n")
            gardeners.append({"name": name, "bead": f"gd-{name}", "runbook": runbook_rel})
        with open(os.path.join(root, drift.MANIFEST_REL), "w", encoding="utf-8") as f:
            json.dump({"gardeners": gardeners}, f)
        return root

    def test_in_sync_passes(self):
        root = self._repo({"alpha": "hello\n"})
        url = self._serve({"gd-alpha": "hello\n"})
        code, rows = drift.check(root, url, None)
        self.assertEqual(code, 0)
        self.assertEqual(rows[0]["verdict"], "IN-SYNC")

    def test_trailing_newline_is_not_drift(self):
        # live has no trailing newline, repo has one -> normalized equal.
        root = self._repo({"alpha": "hello\n"})
        url = self._serve({"gd-alpha": "hello"})
        code, _ = drift.check(root, url, None)
        self.assertEqual(code, 0)

    def test_drift_fails_with_code_2(self):
        root = self._repo({"alpha": "new body\n"})
        url = self._serve({"gd-alpha": "OLD stale body\n"})
        code, rows = drift.check(root, url, None)
        self.assertEqual(code, 2)
        self.assertEqual(rows[0]["verdict"], "DRIFT")

    def test_pointer_stub_is_in_sync_regardless_of_md(self):
        # live is a short pointer that names its own .md; .md body differs wildly.
        stub = "You are gardener-alpha. Read and follow gardener-prompts/alpha.md from your working tree."
        root = self._repo({"alpha": "a totally different long runbook body\n" * 20})
        url = self._serve({"gd-alpha": stub})
        code, rows = drift.check(root, url, None)
        self.assertEqual(code, 0)
        self.assertEqual(rows[0]["verdict"], "POINTER")

    def test_legacy_captain_pointer_stub_is_pointer(self):
        # A live stub written before the rename still names captain-prompts/<name>.md.
        stub = "You are captain-alpha. Read and follow captain-prompts/alpha.md from your working tree."
        root = self._repo({"alpha": "a totally different long runbook body\n" * 20})
        url = self._serve({"gd-alpha": stub})
        code, rows = drift.check(root, url, None)
        self.assertEqual(code, 0)
        self.assertEqual(rows[0]["verdict"], "POINTER")

    def test_pre_rename_inline_body_is_not_drift(self):
        # Live inline copy predates the rename ("captain"); the repo .md was
        # content-renamed. Same text modulo the rename table -> IN-SYNC.
        url = self._serve({"gd-alpha": "Each Captain runs captains-camp; see captain-epic:X.\n"})
        root = self._repo({"alpha": "Each Gardener runs gardeners-camp; see gardener-epic:X.\n"})
        code, rows = drift.check(root, url, None)
        self.assertEqual(code, 0)
        self.assertEqual(rows[0]["verdict"], "IN-SYNC")
        self.assertIn("predates", rows[0]["detail"])

    def test_runbook_delegating_gardener_is_in_sync(self):
        # A runbook-delegating gardener (the intake-groomer shape): manifest row
        # carries a "runbook" key, no gardener-prompts/<name>.md; the live prompt
        # names the runbook and reads it at cycle start. Verdict RUNBOOK, code 0
        # regardless of prompt length (real groomer prompt is ~1.8KB > the
        # 1500B pointer cap, so this shape is NOT covered by _is_pointer_stub).
        rb = "gardenio/docs/runbooks/intake-groomer.md"
        prompt = (
            "You are the gardener-intake groomer. Your full operating contract is "
            "the versioned runbook gardenio/docs/runbooks/intake-groomer.md. "
            + ("Read it at cycle start. " * 40)
        )
        root = self._repo({}, runbooks={"intake-groomer": (rb, True)})
        url = self._serve({"gd-intake-groomer": prompt})
        code, rows = drift.check(root, url, None)
        self.assertEqual(code, 0)
        self.assertEqual(rows[0]["verdict"], "RUNBOOK")

    def test_runbook_delegating_matches_on_basename(self):
        # The prompt may reference just the runbook basename; still verifies.
        rb = "gardenio/docs/runbooks/intake-groomer.md"
        prompt = "Groomer. Read intake-groomer.md from origin/main at cycle start."
        root = self._repo({}, runbooks={"intake-groomer": (rb, True)})
        url = self._serve({"gd-intake-groomer": prompt})
        code, rows = drift.check(root, url, None)
        self.assertEqual(code, 0)
        self.assertEqual(rows[0]["verdict"], "RUNBOOK")

    def test_runbook_delegation_broken_is_drift(self):
        # Live prompt no longer references its declared runbook -> DRIFT, code 2.
        rb = "gardenio/docs/runbooks/intake-groomer.md"
        prompt = "You are some gardener doing something unrelated entirely."
        root = self._repo({}, runbooks={"intake-groomer": (rb, True)})
        url = self._serve({"gd-intake-groomer": prompt})
        code, rows = drift.check(root, url, None)
        self.assertEqual(code, 2)
        self.assertEqual(rows[0]["verdict"], "DRIFT")

    def test_missing_declared_runbook_fails_loud(self):
        # Manifest names a runbook that does not exist in the repo -> ERROR, code 1.
        rb = "gardenio/docs/runbooks/intake-groomer.md"
        prompt = "Groomer. Read gardenio/docs/runbooks/intake-groomer.md."
        root = self._repo({}, runbooks={"intake-groomer": (rb, False)})
        url = self._serve({"gd-intake-groomer": prompt})
        code, rows = drift.check(root, url, None)
        self.assertEqual(code, 1)
        self.assertEqual(rows[0]["verdict"], "ERROR")

    def test_long_prompt_naming_md_is_not_a_pointer(self):
        # A full runbook that merely mentions its own filename is NOT a stub
        # (too long) — must still be diffed as content.
        body = "gardener-prompts/alpha.md\n" + ("x" * 4000)
        root = self._repo({"alpha": "repo body\n"})
        url = self._serve({"gd-alpha": body})
        code, rows = drift.check(root, url, None)
        self.assertEqual(code, 2)
        self.assertEqual(rows[0]["verdict"], "DRIFT")

    def test_unreachable_daemon_fails_loud_code_1(self):
        root = self._repo({"alpha": "hello\n"})
        # point at a closed port
        code, rows = drift.check(root, "http://127.0.0.1:1", None)
        self.assertEqual(code, 1)
        self.assertEqual(rows[0]["verdict"], "ERROR")

    def test_missing_md_fails_loud_code_1(self):
        root = self._repo({"alpha": "hello\n"})
        # manifest references a bead whose .md we delete
        os.remove(os.path.join(root, drift.PROMPTS_DIR_REL, "alpha.md"))
        url = self._serve({"gd-alpha": "hello\n"})
        code, rows = drift.check(root, url, None)
        self.assertEqual(code, 1)
        self.assertEqual(rows[0]["verdict"], "ERROR")

    def test_missing_prompt_field_fails_loud(self):
        root = self._repo({"alpha": "hello\n"})

        class NoPrompt(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_GET(self):
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(json.dumps({"id": "gd-alpha", "fields": {}}).encode())

        srv = HTTPServer(("127.0.0.1", 0), NoPrompt)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        self.addCleanup(srv.server_close)
        self.addCleanup(srv.shutdown)
        url = f"http://127.0.0.1:{srv.server_address[1]}"
        code, rows = drift.check(root, url, None)
        self.assertEqual(code, 1)
        self.assertEqual(rows[0]["verdict"], "ERROR")

    def test_http_error_fails_loud(self):
        # bead present in manifest but the daemon 404s it -> ERROR, code 1.
        root = self._repo({"alpha": "hello\n"})
        url = self._serve({})  # empty map -> handler returns 404 for gd-alpha
        code, rows = drift.check(root, url, None)
        self.assertEqual(code, 1)
        self.assertEqual(rows[0]["verdict"], "ERROR")

    def test_non_json_response_fails_loud(self):
        root = self._repo({"alpha": "hello\n"})

        class NotJson(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_GET(self):
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(b"this is not json <html>")

        srv = HTTPServer(("127.0.0.1", 0), NotJson)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        self.addCleanup(srv.server_close)
        self.addCleanup(srv.shutdown)
        url = f"http://127.0.0.1:{srv.server_address[1]}"
        code, rows = drift.check(root, url, None)
        self.assertEqual(code, 1)
        self.assertEqual(rows[0]["verdict"], "ERROR")

    def test_envelope_shape_is_tolerated(self):
        root = self._repo({"alpha": "hello\n"})

        class Envelope(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_GET(self):
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(json.dumps({"bead": {"id": "gd-alpha", "fields": {"prompt": "hello\n"}}}).encode())

        srv = HTTPServer(("127.0.0.1", 0), Envelope)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        self.addCleanup(srv.server_close)
        self.addCleanup(srv.shutdown)
        url = f"http://127.0.0.1:{srv.server_address[1]}"
        code, _ = drift.check(root, url, None)
        self.assertEqual(code, 0)

    def test_real_manifest_matches_prompt_files(self):
        """The committed manifest must reference a .md that exists for every row
        (guards against a manifest row with no backing file, and vice-versa)."""
        here = os.path.dirname(os.path.abspath(__file__))
        repo_root = os.path.dirname(here)
        manifest_path = os.path.join(repo_root, drift.MANIFEST_REL)
        if not os.path.exists(manifest_path):
            self.skipTest("run from repo root")
        gardeners = drift.load_manifest(repo_root)
        # inline-copy gardeners own a gardener-prompts/<name>.md; runbook-delegating
        # gardeners instead own a "runbook" key and NO <name>.md (ka-0000002202).
        names = {c["name"] for c in gardeners if "runbook" not in c}
        for c in gardeners:
            self.assertTrue(str(c["bead"]).startswith(("gd-", "ka-")), f"bad bead id {c['bead']}")
            if "runbook" in c:
                rb = os.path.join(repo_root, c["runbook"])
                self.assertTrue(
                    os.path.isfile(rb),
                    f"manifest row {c['name']} declares runbook {c['runbook']} but it is missing",
                )
                md = os.path.join(repo_root, drift.PROMPTS_DIR_REL, f"{c['name']}.md")
                self.assertFalse(
                    os.path.exists(md),
                    f"runbook-delegating {c['name']} must NOT also have an inline {md}",
                )
                continue
            md = os.path.join(repo_root, drift.PROMPTS_DIR_REL, f"{c['name']}.md")
            self.assertTrue(os.path.exists(md), f"manifest names {c['name']} but {md} missing")
        # every non-README .md that is a gardener runbook should be in the manifest,
        # EXCEPT:
        #   - pending gardeners with no live bead yet, and
        #   - retired redirect stubs whose live lane is out of drift-alarm scope
        #     by design (runbook-delegation, not gardener-prompts/<name>.md).
        pending = {"placement", "canonical-prod-graduation"}
        # Retired predecessors: a .md whose live gardener has closed/re-authored
        # without a version-controlled successor that reads gardener-prompts/<name>.md.
        # These files are kept for history + link stability but are OUT of drift-alarm
        # scope by design — a manifest row pinning their closed bead would only emit a
        # PHANTOM verdict for a lane no gardener runs (ka-0000003148 / design
        # ka-0000003223 / ka-0000000874). Each is documented in README's
        # Retired-predecessors table and MUST NOT be re-added as an inline-copy row:
        #   - intake:                 live lane is the groomer ka-0000003841, covered by
        #                             a runbook-DELEGATING manifest row (name
        #                             "intake-groomer") — ka-0000002202, design
        #                             ka-0000003223 §3.
        #   - delivery:               live lane is gardener-delivery-v2 ka-0000007986
        #                             (inline prompt, does not read delivery.md).
        #   - release:                live lane is gardener-release-owner v4
        #                             ka-0000003719 (runbook doc ka-0000000327);
        #                             version-controlling it is ka-0000001820 /
        #                             operator decision ka-0000000800.
        #   - customer-poc-aws/reaper/reviver/wake-intent-reconciler:
        #                             closed+disabled, no live successor reads the .md.
        retired = {
            "intake",
            "delivery",
            "release",
            "customer-poc-aws",
            "reaper",
            "reviver",
            "wake-intent-reconciler",
        }
        pdir = os.path.join(repo_root, drift.PROMPTS_DIR_REL)
        md_names = {
            f[:-3]
            for f in os.listdir(pdir)
            if f.endswith(".md") and f != "README.md"
        }
        unmapped = md_names - names - pending - retired
        self.assertEqual(unmapped, set(), f"prompt .md not in manifest and not pending: {unmapped}")
        # A retired stub must NOT be silently re-mapped back into the manifest
        # (an inline-copy row for a retired lane pins a closed bead -> phantom DRIFT).
        self.assertEqual(
            names & retired, set(), f"retired stub(s) re-added to manifest: {names & retired}"
        )

    def test_retired_lane_row_would_report_phantom(self):
        """Regression for the phantom-DRIFT class (ka-0000000874): the whole reason
        retired lanes are dropped from the manifest is that their closed bead's frozen
        prompt no longer tracks the repo .md, so keeping the row makes the alarm red a
        lane no gardener runs. This asserts the mechanism: an inline-copy row whose live
        (here: closed-bead-frozen) prompt differs from the repo .md is reported DRIFT —
        which, for a retired lane, is a false alarm. The fix is row REMOVAL (asserted by
        test_real_manifest_matches_prompt_files), not tuning the diff."""
        root = self._repo({"retired": "the repo .md has moved on\n"})
        # simulate the closed bead's stale frozen prompt
        url = self._serve({"gd-retired": "frozen predecessor prompt from months ago\n"})
        code, rows = drift.check(root, url, None)
        self.assertEqual(code, 2)
        self.assertEqual(rows[0]["verdict"], "DRIFT")


if __name__ == "__main__":
    unittest.main(verbosity=2)
