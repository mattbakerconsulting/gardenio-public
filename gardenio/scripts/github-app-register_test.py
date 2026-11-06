#!/usr/bin/env python3
"""Tests for gardenio/scripts/github-app-register.py.

Hermetic: no network, no aws, no GitHub. The RSA key is generated on the fly
with openssl and the JWT signature is verified with openssl, so the signing path
is tested for real rather than asserted about.

The tests that matter most are the two invariants a reviewer cannot eyeball:
  * the agents App never acquires `administration` (D4 — that grant is the whole
    reason there are two Apps), and
  * the PEM, webhook secret and client secret are written as SecureString while
    the non-secret fields are not, so a future edit cannot quietly downgrade one.

Run: python3 gardenio/scripts/github-app-register_test.py
"""
from __future__ import annotations

import argparse
import base64
import html
import importlib.util
import io
import json
import os
import re
import subprocess
import tempfile
import unittest
import urllib.parse
from unittest import mock

_HERE = os.path.dirname(os.path.abspath(__file__))
_spec = importlib.util.spec_from_file_location(
    "ghapp", os.path.join(_HERE, "github-app-register.py")
)
ghapp = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(ghapp)


def _b64u_decode(segment: str) -> bytes:
    return base64.urlsafe_b64decode(segment + "=" * (-len(segment) % 4))


class TestManifest(unittest.TestCase):
    def test_agents_permissions_are_exactly_the_working_set(self):
        man = ghapp.build_manifest("agents", "gardenio-io", "http://localhost:8731/callback")
        self.assertEqual(
            man["default_permissions"],
            {
                "contents": "write",
                "workflows": "write",
                "pull_requests": "write",
                "issues": "write",
                "metadata": "read",
                "actions": "write",
                "checks": "read",
                "packages": "read",
                "deployments": "read",
            },
        )

    def test_agents_can_write_workflow_files(self):
        """Crews push .github/workflows (PRs #73, #65, #54, #19). GitHub rejects
        that for an App token holding only contents:write, so losing this
        permission breaks them at cutover and the failure looks like a 403 on
        push, not like a missing grant."""
        man = ghapp.build_manifest("agents", "o", "http://x/cb")
        self.assertEqual(man["default_permissions"].get("workflows"), "write")
        self.assertEqual(man["default_permissions"].get("contents"), "write")

    def test_agents_app_never_gets_administration(self):
        """D4's whole point. If this fails, the two-App split has collapsed."""
        man = ghapp.build_manifest("agents", "gardenio-io", "http://x/cb")
        self.assertNotIn("administration", man["default_permissions"])
        for name, value in man["default_permissions"].items():
            self.assertIn(value, ("read", "write"), f"{name} has a bogus level {value!r}")

    def test_arc_permissions_are_the_minimum_the_listener_uses(self):
        """D4. ARC's scale-set listener registers and deletes runners, which is
        Administration; it never calls an Actions endpoint. `actions` was in the
        original self-hosted-runner plan and is not required — this App holds the
        most dangerous grant in the file, so it holds nothing else."""
        man = ghapp.build_manifest("arc", "gardenio-io", "http://x/cb")
        self.assertEqual(
            man["default_permissions"],
            {"administration": "write", "metadata": "read"},
        )

    def test_agents_can_rerun_and_dispatch_workflows(self):
        """Crew plans call `gh run rerun --failed` and `gh workflow run`, both of
        which need Actions WRITE, not read. Ruled 2026-09-21 knowing write also
        grants cancel and deletion of runs and logs."""
        man = ghapp.build_manifest("agents", "o", "http://x/cb")
        self.assertEqual(man["default_permissions"].get("actions"), "write")

    def test_agents_app_name_is_one_we_can_actually_hold(self):
        """GitHub: "You cannot use the same name as an existing GitHub account,
        unless it is your own user or organization name." `gardenio` is an
        unrelated Organization, so D6's original name would be rejected at the
        confirmation page. `gardenio-io` is ours."""
        man = ghapp.build_manifest("agents", "joyousaversion", "http://x/cb")
        self.assertEqual(man["name"], "gardenio-io")

    def test_the_two_apps_are_distinct(self):
        agents = ghapp.build_manifest("agents", "o", "http://x/cb")
        arc = ghapp.build_manifest("arc", "o", "http://x/cb")
        self.assertNotEqual(agents["name"], arc["name"])
        self.assertNotEqual(agents["default_permissions"], arc["default_permissions"])

    def test_lands_permissions_match_the_go_set(self):
        """MANIFESTS["lands"] and githubapp.LandsTokenPermissions are one list in
        two languages: the App holds exactly what tenant tokens ask for."""
        go = open(os.path.join(os.path.dirname(__file__), "..", "controller", "internal",
                               "githubapp", "identity.go")).read()
        body = go.split("func LandsTokenPermissions()", 1)[1].split("}\n}", 1)[0]
        go_set = dict(re.findall(r'"(\w+)":\s*"(\w+)"', body))
        self.assertEqual(go_set, ghapp.MANIFESTS["lands"]["permissions"])
        self.assertEqual(ghapp.MANIFESTS["lands"]["permissions"], {
            "contents": "write", "pull_requests": "write", "issues": "write",
            "checks": "read", "metadata": "read",
        })

    def test_lands_app_never_gets_the_fleet_extras(self):
        perms = ghapp.MANIFESTS["lands"]["permissions"]
        for key in ("administration", "actions", "workflows", "packages"):
            self.assertNotIn(key, perms)
        self.assertNotEqual(ghapp.MANIFESTS["lands"]["name"], ghapp.MANIFESTS["agents"]["name"])

    def test_lands_manifest_has_the_setup_leg_and_no_webhook(self):
        man = ghapp.build_manifest("lands", "o", "http://x/cb", "https://gd.example/hook")
        self.assertIs(man["public"], True)
        self.assertIs(man["request_oauth_on_install"], True)
        self.assertIs(man["setup_on_update"], True)
        self.assertEqual(man["callback_urls"], ["https://gardenio.clonk.tv/lands/github/setup"])
        self.assertEqual(man["setup_url"], "https://gardenio.clonk.tv/lands/github/setup")
        # A webhook URL turns on the hook but subscribes to nothing: no events in v1.
        self.assertEqual(man["default_events"], [])
        self.assertEqual(ghapp.MANIFESTS["lands"]["repository_selection"], "selected")

    def test_cli_without_public_keeps_the_declared_visibility(self):
        """`manifest` without --public must print what MANIFESTS declares: the
        lands App public, the other two private."""
        for app, want in (("lands", True), ("agents", False), ("arc", False)):
            out = io.StringIO()
            with mock.patch("sys.stdout", out):
                ghapp.main(["manifest", "--app", app, "--org", "o"])
            self.assertIs(json.loads(out.getvalue())["public"], want, app)
        out = io.StringIO()
        with mock.patch("sys.stdout", out):
            ghapp.main(["manifest", "--app", "agents", "--org", "o", "--public"])
        self.assertIs(json.loads(out.getvalue())["public"], True)

    def test_only_lands_has_a_user_authorization_leg(self):
        for app in ("agents", "arc"):
            man = ghapp.build_manifest(app, "o", "http://x/cb")
            self.assertIs(man["request_oauth_on_install"], False)
            self.assertNotIn("callback_urls", man)
            self.assertNotIn("setup_url", man)

    def test_declared_visibility_is_the_default(self):
        """MANIFESTS is the single statement of what each App IS, and `verify`
        diffs the live App against it — so visibility has to live there too, not
        only in a CLI flag verify never sees."""
        for app in ("agents", "arc"):
            self.assertIs(ghapp.MANIFESTS[app]["public"], False)
            self.assertIs(ghapp.build_manifest(app, "o", "http://x/cb")["public"], False)

    def test_public_is_opt_in_and_changes_nothing_else(self):
        """`public` is the one P3 property that may have to move early, because a
        private App cannot install on an account other than its owner's. Turning
        it on must not quietly switch on the webhook or the user-auth leg."""
        man = ghapp.build_manifest("agents", "o", "http://x/cb", public=True)
        self.assertIs(man["public"], True)
        self.assertIs(man["request_oauth_on_install"], False)
        self.assertEqual(man["default_events"], [])
        self.assertNotIn("hook_attributes", man)

    def test_p0_registers_the_app_inert(self):
        """Private, no webhook, no user-auth leg, no events — §7.1 and P0."""
        man = ghapp.build_manifest("agents", "gardenio-io", "http://localhost:8731/callback")
        self.assertIs(man["public"], False)
        self.assertIs(man["request_oauth_on_install"], False)
        self.assertEqual(man["default_events"], [])
        self.assertNotIn("hook_attributes", man)
        self.assertEqual(man["redirect_url"], "http://localhost:8731/callback")

    def test_webhook_url_is_opt_in_for_p3(self):
        man = ghapp.build_manifest("agents", "o", "http://x/cb", "https://gd.example/hook")
        self.assertEqual(man["hook_attributes"], {"url": "https://gd.example/hook", "active": True})

    def test_webhook_subscribes_to_the_bridge_events_and_only_then(self):
        """default_events is empty without a webhook URL (P0 stays inert) and is
        the github-bridge's set with one. Every subscribed event must be backed by
        a permission the App holds, or GitHub rejects the manifest."""
        needs = {
            "pull_request": "pull_requests",
            "pull_request_review": "pull_requests",
            "pull_request_review_comment": "pull_requests",
            "pull_request_review_thread": "pull_requests",
            "issue_comment": "issues",
            "check_suite": "checks",
            "workflow_run": "actions",
            "deployment_status": "deployments",
        }
        man = ghapp.build_manifest("agents", "o", "http://x/cb", "https://gd.example/hook")
        self.assertEqual(sorted(man["default_events"]), sorted(needs))
        for ev in man["default_events"]:
            self.assertIn(needs[ev], man["default_permissions"], ev)
        self.assertEqual(ghapp.build_manifest("agents", "o", "http://x/cb")["default_events"], [])
        # The ARC App is infrastructure, never a webhook consumer.
        arc = ghapp.build_manifest("arc", "o", "http://x/cb", "https://gd.example/hook")
        self.assertEqual(arc["default_events"], [])

    def test_unknown_app_is_rejected(self):
        with self.assertRaises(KeyError):
            ghapp.build_manifest("nope", "o", "http://x/cb")


class TestForm(unittest.TestCase):
    def test_form_posts_to_the_org_endpoint_with_state(self):
        man = ghapp.build_manifest("agents", "gardenio-io", "http://x/cb")
        page = ghapp.manifest_form_html(man, "gardenio-io", "st4te")
        self.assertIn("/organizations/gardenio-io/settings/apps/new?state=st4te", html.unescape(page))
        self.assertIn('method="post"', page)

    def test_manifest_survives_html_escaping(self):
        """The manifest rides in a hidden input; a mangled one fails at GitHub, not here."""
        man = ghapp.build_manifest("arc", "gardenio-io", "http://x/cb")
        page = ghapp.manifest_form_html(man, "gardenio-io", "s")
        marker = 'name="manifest" value="'
        start = page.index(marker) + len(marker)
        payload = page[start : page.index('"', start)]
        self.assertEqual(json.loads(html.unescape(payload)), man)

    def test_org_with_odd_characters_is_url_quoted(self):
        man = ghapp.build_manifest("arc", "a b", "http://x/cb")
        page = ghapp.manifest_form_html(man, "a b", "s")
        self.assertIn("/organizations/a%20b/settings", html.unescape(page))

    def test_user_owner_posts_to_the_personal_form(self):
        """A private App installs only on its owner's own account, and the CI
        repos are owned by a user. The personal form is unscoped — there is no
        /users/<login>/settings/apps/new — so it must not carry the org path."""
        man = ghapp.build_manifest("agents", "joyousaversion", "http://x/cb")
        page = html.unescape(ghapp.manifest_form_html(man, "joyousaversion", "s", owner_type="user"))
        self.assertIn("https://github.com/settings/apps/new?state=s", page)
        self.assertNotIn("/organizations/", page)


class TestInstallScope(unittest.TestCase):
    """An install can be right in every visible way and still be wrong in the
    one way that matters: scoped to All repositories."""

    def _verify(self, installs):
        ns = argparse.Namespace(app="arc", profile="p", region="r")
        with mock.patch.object(ghapp, "ssm_get", side_effect=lambda a, f, *_: {
            "app-id": "1", "private-key": "pem",
        }[f]), mock.patch.object(ghapp, "mint_jwt", return_value="jwt"), \
                mock.patch.object(ghapp, "is_publicly_visible", return_value=False), \
                mock.patch.object(ghapp, "_api", side_effect=lambda m, path, **kw: (
                    installs if path == "/app/installations" else {
                        "slug": "gardenio-ci", "id": 1, "name": "gardenio-ci",
                        "owner": {"login": "o"}, "hook_attributes": {}, "events": [],
                        "permissions": dict(ghapp.MANIFESTS["arc"]["permissions"]),
                    })), mock.patch("sys.stdout", io.StringIO()) as out:
            return ghapp.cmd_verify(ns), out.getvalue()

    def test_install_matching_the_declared_selection_passes(self):
        want = ghapp.MANIFESTS["arc"]["repository_selection"]
        rc, _ = self._verify([{"id": 9, "account": {"login": "o"},
                               "repository_selection": want}])
        self.assertEqual(rc, 0)

    def test_selection_other_than_the_declared_one_is_drift(self):
        want = ghapp.MANIFESTS["arc"]["repository_selection"]
        other = "selected" if want == "all" else "all"
        rc, out = self._verify([{"id": 9, "account": {"login": "o"},
                                 "repository_selection": other}])
        self.assertEqual(rc, 1, "a selection MANIFESTS did not declare must fail verify")
        self.assertIn("repository_selection", out)
        self.assertIn("Drift against MANIFESTS", out)

    def test_the_agents_app_must_stay_repo_scoped(self):
        """It carries contents:write. 'all' would let every agent pod push to
        anything the account owns, which is the grant the whole plan removes."""
        self.assertEqual(ghapp.MANIFESTS["agents"]["repository_selection"], "selected")

    def test_no_install_is_not_drift(self):
        """P0 leaves 0 installations on purpose; verify has to stay green there."""
        rc, _ = self._verify([])
        self.assertEqual(rc, 0)

    def _install_url(self, **kw):
        ns = argparse.Namespace(app="arc", profile="p", region="r", target_id="42",
                                repo=["o/one", "o/two"], repo_id=[])
        for k, v in kw.items():
            setattr(ns, k, v)
        with mock.patch.object(ghapp, "ssm_get", return_value="gardenio-ci"), \
                mock.patch("sys.stdout", io.StringIO()) as out:
            self.assertEqual(ghapp.cmd_install_url(ns), 0)
        return out.getvalue()

    def test_install_url_names_the_repos_and_the_wrong_radio_button(self):
        text = self._install_url()
        self.assertIn("https://github.com/apps/gardenio-ci/installations/new", text)
        self.assertIn("suggested_target_id=42", text)
        self.assertIn("o/one", text)
        self.assertIn("o/two", text)
        self.assertIn("NOT 'All repositories'", text)

    def test_repo_ids_preselect_rather_than_asking_for_the_right_click(self):
        """Prose under the link did not stop the first install landing on
        every repo the account owns. The page has to open already narrowed."""
        text = self._install_url(repo_id=["11", "22"])
        url = text.splitlines()[0]
        self.assertIn("/installations/new/permissions?", url)
        parsed = urllib.parse.parse_qs(urllib.parse.urlparse(url).query)
        self.assertEqual(parsed["repository_ids[]"], ["11", "22"])
        self.assertEqual(parsed["suggested_target_id"], ["42"])


class TestTimeoutIsRecoverable(unittest.TestCase):
    """A timeout after the operator clicked Create leaves a real App behind and a
    code that only lives in their address bar. Exiting with a bare "timed out"
    invites a re-run, which GitHub refuses as a duplicate name and which loses
    the code for good."""

    def test_timeout_message_hands_back_the_convert_command(self):
        ghapp._Handler.result = {}
        # Don't let a CI runner try to launch a browser.
        opened, ghapp.webbrowser.open = ghapp.webbrowser.open, lambda _url: True
        try:
            with self.assertRaises(SystemExit) as caught:
                ghapp.await_code("<html></html>", "s", 0, timeout=0, app="agents")
        finally:
            ghapp.webbrowser.open = opened
        message = str(caught.exception)
        self.assertIn("convert --app agents --code-url", message)
        self.assertIn("/callback?", message)
        self.assertIn("one hour", message)


class TestCodeExtraction(unittest.TestCase):
    def test_from_callback_url(self):
        url = "http://localhost:8731/callback?code=abc123&state=xyz"
        self.assertEqual(ghapp.code_from_url(url), "abc123")

    def test_bare_code_passes_through(self):
        self.assertEqual(ghapp.code_from_url("  abc123 "), "abc123")

    def test_url_without_code_is_an_error(self):
        with self.assertRaises(SystemExit):
            ghapp.code_from_url("http://localhost:8731/callback?state=xyz")


class TestJWT(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.TemporaryDirectory()
        cls.key = os.path.join(cls.tmp.name, "k.pem")
        cls.pub = os.path.join(cls.tmp.name, "k.pub")
        subprocess.run(
            ["openssl", "genpkey", "-algorithm", "RSA", "-pkeyopt", "rsa_keygen_bits:2048",
             "-out", cls.key],
            check=True, capture_output=True,
        )
        subprocess.run(["openssl", "rsa", "-in", cls.key, "-pubout", "-out", cls.pub],
                       check=True, capture_output=True)
        with open(cls.key) as handle:
            cls.pem = handle.read()

    @classmethod
    def tearDownClass(cls):
        cls.tmp.cleanup()

    def test_signature_verifies_against_the_public_key(self):
        token = ghapp.mint_jwt(self.pem, "12345", now=1_700_000_000)
        header, claims, signature = token.split(".")
        with tempfile.TemporaryDirectory() as tmp:
            sig = os.path.join(tmp, "sig")
            with open(sig, "wb") as handle:
                handle.write(_b64u_decode(signature))
            proc = subprocess.run(
                ["openssl", "dgst", "-sha256", "-verify", self.pub, "-signature", sig],
                input=f"{header}.{claims}".encode(), capture_output=True,
            )
        self.assertEqual(proc.returncode, 0, proc.stderr.decode(errors="replace"))

    def test_claims_are_what_github_requires(self):
        token = ghapp.mint_jwt(self.pem, "12345", now=1_700_000_000)
        header, claims, _ = token.split(".")
        self.assertEqual(json.loads(_b64u_decode(header)), {"alg": "RS256", "typ": "JWT"})
        payload = json.loads(_b64u_decode(claims))
        self.assertEqual(payload["iss"], "12345")
        # Backdated against laptop clock drift, and inside GitHub's 10-minute cap.
        self.assertLess(payload["iat"], 1_700_000_000)
        self.assertLessEqual(payload["exp"] - payload["iat"], 600)

    def test_app_id_is_stringified(self):
        """SSM hands back a string, but an int caller must not produce `iss: 123`."""
        token = ghapp.mint_jwt(self.pem, 12345, now=1_700_000_000)
        payload = json.loads(_b64u_decode(token.split(".")[1]))
        self.assertEqual(payload["iss"], "12345")

    def test_no_key_material_is_left_behind(self):
        before = set(os.listdir(tempfile.gettempdir()))
        ghapp.mint_jwt(self.pem, "1", now=1_700_000_000)
        leaked = [n for n in set(os.listdir(tempfile.gettempdir())) - before
                  if n.startswith("gh-app-")]
        self.assertEqual(leaked, [])


class TestSSM(unittest.TestCase):
    def test_parameter_paths(self):
        self.assertEqual(ghapp.ssm_path("agents", "private-key"),
                         "/gardenio/github-app/agents/private-key")

    def test_secrets_are_securestring_and_public_fields_are_not(self):
        calls = []

        def fake_put(app, field, value, *, secret, kms, profile, region):
            calls.append({"field": field, "value": value, "secret": secret, "kms": kms})
            return ghapp.ssm_path(app, field)

        original, ghapp.ssm_put = ghapp.ssm_put, fake_put
        try:
            conv = {
                "id": 42, "slug": "gardenio", "client_id": "Iv1.abc",
                "html_url": "https://github.com/apps/gardenio",
                "pem": "-----BEGIN RSA PRIVATE KEY-----\nx\n-----END RSA PRIVATE KEY-----", # gitleaks:allow: fake PEM
                "webhook_secret": "whsec", "client_secret": "csec",
            }
            names = ghapp.store_conversion("agents", conv, kms="alias/k", profile="p", region="r")
        finally:
            ghapp.ssm_put = original

        by_field = {c["field"]: c for c in calls}
        for field in ("private-key", "webhook-secret", "client-secret"):
            self.assertTrue(by_field[field]["secret"], f"{field} must be SecureString")
            self.assertEqual(by_field[field]["kms"], "alias/k")
        for field in ("app-id", "slug", "client-id", "html-url"):
            self.assertFalse(by_field[field]["secret"], f"{field} need not be encrypted")
        self.assertEqual(by_field["app-id"]["value"], "42", "app id must be stored as a string")
        self.assertEqual(len(names), 7)

    def test_absent_fields_are_skipped_not_stored_empty(self):
        """A manifest registered without webhooks has no webhook_secret; storing '' would
        make `verify` look configured when it is not."""
        calls = []
        original, ghapp.ssm_put = ghapp.ssm_put, (
            lambda app, field, value, **kw: calls.append(field) or ghapp.ssm_path(app, field)
        )
        try:
            ghapp.store_conversion("arc", {"id": 7, "pem": "k", "webhook_secret": ""},
                                   kms="alias/k", profile="p", region="r")
        finally:
            ghapp.ssm_put = original
        self.assertEqual(sorted(calls), ["app-id", "private-key"])


class TestCallbackServer(unittest.TestCase):
    """The state check is CSRF protection on a flow that mints an App identity."""

    def _handle(self, path, state):
        ghapp._Handler.state = state
        ghapp._Handler.result = {}
        parsed = urllib.parse.urlparse(path)
        params = urllib.parse.parse_qs(parsed.query)
        got = (params.get("state") or [""])[0]
        code = (params.get("code") or [""])[0]
        # Mirrors do_GET's decision without standing up a socket.
        import secrets as _s
        if not _s.compare_digest(got, state):
            return {"error": "state mismatch"}
        if not code:
            return {"error": "no code"}
        return {"code": code}

    def test_matching_state_accepts(self):
        self.assertEqual(self._handle("/callback?code=c&state=s", "s"), {"code": "c"})

    def test_mismatched_state_rejects(self):
        self.assertIn("error", self._handle("/callback?code=c&state=evil", "s"))

    def test_missing_code_rejects(self):
        self.assertIn("error", self._handle("/callback?state=s", "s"))


if __name__ == "__main__":
    unittest.main(verbosity=2)
