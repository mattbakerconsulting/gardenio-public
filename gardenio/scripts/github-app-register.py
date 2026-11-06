#!/usr/bin/env python3
# github-app-register.py
#
# P0 of gardenio/plans/github-app-onboarding.md: register the gardenio GitHub
# Apps and put their credentials in SSM, without a private key ever touching a
# Downloads folder.
#
#   WHY A MANIFEST AND NOT THE SETTINGS UI. Registering a GitHub App by hand
#   means hand-toggling ~8 permissions on a web form, twice, and then clicking
#   "Generate a private key", which downloads a .pem to the browser's download
#   directory. That .pem is the App's whole identity — it mints installation
#   tokens for every repo the App is installed on, forever, and it is now a
#   world-readable file in a directory nobody ever cleans. The App Manifest flow
#   (https://docs.github.com/apps/sharing-github-apps/registering-a-github-app-from-a-manifest)
#   replaces both steps: this script POSTs a JSON description of the App to
#   GitHub, the operator clicks one button, and GitHub hands the PEM back to
#   *this process* in an API response. It goes straight to SSM as a
#   SecureString. It is never written to disk, and the permission set is code
#   under review rather than a form somebody remembered to fill in.
#
#   THE SECOND REASON is drift. D4 of the plan rules that there are TWO Apps —
#   the agents App and the ARC App — precisely so that ARC's
#   `administration:write` (which can add and remove repo collaborators and
#   delete runners) is not also handed to every agent pod. That separation is
#   only real if the two permission sets stay different, and a permission set
#   that lives in a web form drifts silently. Here, MANIFESTS below is the
#   single statement of what each App may do, and `verify` reads the live App
#   back from GitHub and diffs it against that statement.
#
#   THE FLOW, precisely:
#     1. `register` starts a local HTTP server and opens a page that auto-POSTs
#        the manifest to
#        https://github.com/organizations/<org>/settings/apps/new?state=<nonce>
#     2. The operator clicks "Create GitHub App for <org>" — the one click.
#     3. GitHub redirects the browser back to this server with a temporary
#        `code` (valid one hour) and the `state` nonce echoed back.
#     4. The script POSTs /app-manifests/<code>/conversions and receives
#        {id, slug, pem, webhook_secret, client_id, client_secret, html_url}.
#     5. Everything secret goes to SSM as SecureString under a CMK. The
#        response is never printed and never written to a file.
#
#   IF YOU ARE NOT AT THE LAPTOP (the phone case): the callback is a localhost
#   URL, so the browser will fail to load it — but the `code` is right there in
#   the address bar. Run `manifest` to get the HTML, submit it from any browser,
#   then paste the failed URL into `convert --code-url '<url>'` from the laptop.
#   The code is valid for an hour, which is plenty.
#
#   WHAT THIS SCRIPT DELIBERATELY DOES NOT DO, per P0 ("change nothing else"):
#   it does not install the App on any repository, does not write a webhook URL
#   (D5's public Ingress is P3 work), does not enable user authorization, and
#   registers the App as PRIVATE. §7.1 of the plan rules that the App goes
#   public only at P3, once install gating exists — a public App is installable
#   by any GitHub user with no approval queue on GitHub's side, so flipping it
#   public before there is a gate means a stranger's install auto-provisions
#   beads and offers a spawn button on our LLM budget.
#
#   NO PYTHON DEPENDENCIES. JWT signing shells out to openssl rather than
#   importing PyJWT or cryptography, because this runs on whatever laptop the
#   operator happens to be at and openssl is always there. aws and the network
#   are the only other requirements.
#
# Usage:
#   ./github-app-register.py manifest --app agents --org gardenio-io
#   ./github-app-register.py register --app agents --org gardenio-io
#   ./github-app-register.py convert  --app agents --code-url 'http://localhost:8731/callback?code=...'
#   ./github-app-register.py verify   --app agents
#
# Tests: python3 gardenio/scripts/github-app-register_test.py
from __future__ import annotations

import argparse
import base64
import html
import http.server
import json
import os
import secrets
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import webbrowser

GITHUB_API = "https://api.github.com"
GITHUB_WEB = "https://github.com"

# SSM prefix. Chosen to sit beside the existing /gardenio/ci/github-token that
# P1 deletes, so the whole GitHub credential surface is one `aws ssm
# get-parameters-by-path /gardenio` away.
SSM_PREFIX = "/gardenio/github-app"
DEFAULT_KMS_ALIAS = "alias/gardenio-github-app"

# Default callback port. Nothing else in the repo binds it; it only has to be
# free for the ~30 seconds the operator is clicking.
DEFAULT_PORT = 8731

# The two Apps of D4. `permissions` is the authoritative statement of what each
# App may do — `verify` diffs the live App against it.
#
# agents: the credential every agent pod mounts in place of the shared PAT.
#   contents/pull_requests/issues write is the actual working set (clone, push
#   a branch, open a PR, comment on a bead's issue). checks:read is what lets a
#   crew see whether its own PR went green — today it cannot, which is open
#   decision gd-4j5m4jtW34 (the shared PAT gets 403 on the check-runs API and
#   crews fall back to `gh run list`). actions is WRITE, not read, because crew
#   plans instruct crews to run `gh run rerun --failed` and `gh workflow run`;
#   ruled 2026-09-21 knowing it also grants cancel and run/log deletion, i.e. a
#   crew can erase CI evidence. packages:read is for ghcr pulls (D7).
#   workflows:write is NOT optional: GitHub rejects any App-token write under
#   .github/workflows without it, and crews already do exactly that (PRs #73,
#   #65, #54, #19), so omitting it would break them at cutover.
#   NOTHING here can change repo settings or manage runners.
#
# arc: Actions Runner Controller, per gardenio/plans/self-hosted-ci-runner.md.
#   administration:write is required for ARC to register and delete runners and
#   is the single most dangerous grant in this file — it is also why this is a
#   separate App that no agent pod ever sees. `actions` is deliberately absent:
#   the running listener (actions/scaleset v0.4.0) touches only the runner-scale-set
#   endpoints, all of which sit under Administration.
MANIFESTS = {
    "agents": {
        # D6, re-ruled 2026-09-21. `gardenio` is an unrelated Organization and
        # GitHub refuses a name that matches an existing account unless it is
        # one of yours; `gardenio-io` is ours, so the exception applies. PRs
        # will show as `gardenio-io[bot]`.
        "name": "gardenio-io",
        # Not installed yet (P2). When it is, it must be repo-scoped: unlike
        # the ARC App this one carries `contents: write`, so "all" would let
        # every agent pod push to any repo the account owns.
        "repository_selection": "selected",
        # D8: private for now. A private App installs only on its owner's own
        # account, which is why both Apps are owned by the user and not by
        # gardenio-io. Flip this to True at P3 — and transfer to the org only
        # AFTER it is public, or the transfer uninstalls it.
        "public": False,
        "description": (
            "gardenio agents: clones repositories, pushes branches and opens pull requests "
            "on behalf of automated crews. Commits carry a 'Created by agent:' trailer."
        ),
        "permissions": {
            "contents": "write",
            "workflows": "write",
            "pull_requests": "write",
            "issues": "write",
            "metadata": "read",
            "actions": "write",
            "checks": "read",
            "packages": "read",
            # deployment_status only (step 4b, cmd/github-bridge/deployment.go):
            # GitHub delivers it to an App holding deployments. Read, never
            # write: the bridge records a deploy, it never creates one.
            "deployments": "read",
        },
        # Webhook events the github-bridge consumes (cmd/github-bridge). Sent in
        # the manifest ONLY when --webhook-url is given: an event subscription
        # with no endpoint to deliver to is meaningless, and P0 stays inert.
        # Each needs the matching permission above: pull_request* -> pull_requests,
        # issue_comment -> issues, check_suite -> checks, workflow_run -> actions,
        # deployment_status -> deployments.
        # `installation` and `installation_repositories` need none and are always
        # delivered to an App's webhook, listed so the manifest states intent.
        "events": [
            "pull_request",
            "pull_request_review",
            "pull_request_review_comment",
            "pull_request_review_thread",
            "issue_comment",
            "check_suite",
            "workflow_run",
            "deployment_status",
        ],
    },
    "arc": {
        "name": "gardenio-ci",
        # Ruled by the operator 2026-09-22, after `verify` caught the install
        # landing on "All repositories" (the install page's default). Accepted
        # knowingly: it means `administration: write` on `gascity` too — the
        # public Gas City fork — and on any repo the account creates later.
        # That grant covers settings, collaborators and deletion; it does NOT
        # include `contents`, so this App still cannot push code anywhere.
        # It is recorded here rather than left as standing drift because a
        # check that is always red is a check everybody learns to scroll past;
        # declared, it still fires if the selection or the account changes.
        "repository_selection": "all",
        # Pure infrastructure, never a front door. Stays private permanently:
        # nothing is gained by letting a stranger install administration:write.
        "public": False,
        "description": (
            "gardenio CI: authenticates Actions Runner Controller so self-hosted runner "
            "scale sets can register with and receive jobs from GitHub Actions."
        ),
        "permissions": {
            "administration": "write",
            "metadata": "read",
        },
    },
    # lands: the Gardenio Lands App (Lands Track B B-D3, step 7.6). A stranger
    #   installs it on the ONE repository their land works on, and the install's
    #   user-authorization leg is how they prove they control it: GitHub sends
    #   them to `user_authorization.callback_url` (beads-viewer's
    #   /lands/github/setup) with a code, and the viewer checks the code's
    #   GitHub user is the applicant, an admin of the repo
    #   (internal/githubapp/setup.go). It is a SEPARATE App from `agents` so a
    #   stranger's install never reaches the fleet's permissions or webhook.
    #   The permission set is githubapp.LandsTokenPermissions, kept equal by
    #   test_lands_permissions_match_the_go_set: push a branch, open a PR,
    #   comment on issues, read checks. No actions/workflows/packages: a tenant
    #   agent cannot rerun or edit CI. No webhook and no events in v1. Public,
    #   because a private App installs only on its owner's account; an install
    #   grants an installer nothing until the operator approves the land.
    #   Runbook: gardenio/docs/lands/github-app.md.
    "lands": {
        "name": "gardenio-lands",
        "repository_selection": "selected",
        "public": True,
        "description": (
            "Gardenio Lands: agents work on the one repository you install this app on, "
            "opening pull requests for you to review. Install it on a single repository."
        ),
        "permissions": {
            "contents": "write",
            "pull_requests": "write",
            "issues": "write",
            "checks": "read",
            "metadata": "read",
        },
        "user_authorization": {
            "callback_url": "https://gardenio.clonk.tv/lands/github/setup",
            # Send the installer back after they change the repository
            # selection, so a wrong pick can be corrected and re-checked.
            "setup_on_update": True,
        },
    },
}


# ---------------------------------------------------------------- manifest ---


def build_manifest(
    app: str,
    org: str,
    redirect_url: str,
    webhook_url: str | None = None,
    public: bool | None = None,
) -> dict:
    """Build the GitHub App Manifest for `app`.

    P0 registers the App inert: no webhook, no user authorization, no events.
    Each of those is a P3 decision and turning any of them on here would mean
    the App is live before the gate that governs it exists.

    `public` is the one P3 property that may have to move early. §7.1 keeps the
    App private until a stranger's install cannot auto-provision — but a private
    App can only be installed on its owner's own account, and the CI repos are
    owned by a different account than the App. `public=True` on an App with no
    webhook, no events and no user-auth leg grants an installer nothing; it is
    the cheap way out of that corner. See --public and --owner-type.
    """
    if app not in MANIFESTS:
        raise KeyError(f"unknown app {app!r}; known: {', '.join(sorted(MANIFESTS))}")
    spec = MANIFESTS[app]
    manifest = {
        "name": spec["name"],
        "url": f"{GITHUB_WEB}/{org}",
        "description": spec["description"],
        "redirect_url": redirect_url,
        # D2 rules the App public, but §7.1 rules it public *at P3*, once a
        # stranger's install cannot auto-provision. Private unless asked.
        "public": spec["public"] if public is None else bool(public),
        # No user-authorization leg at P0: GitHub requires a Callback URL when
        # it is on, and D5's public Ingress does not exist yet.
        "request_oauth_on_install": False,
        "setup_on_update": False,
        "default_permissions": dict(spec["permissions"]),
        # No events unless there is an endpoint to deliver them to (below).
        "default_events": [],
    }
    auth = spec.get("user_authorization")
    if auth:
        # Only an App whose MANIFESTS entry declares it gets a user-auth leg
        # (today: lands, whose setup URL is its whole purpose). With
        # request_oauth_on_install on, GitHub sends the installer to the
        # callback URL with installation_id, a code and the install's state;
        # setup_url is set to the same page for the update case.
        manifest["request_oauth_on_install"] = True
        manifest["callback_urls"] = [auth["callback_url"]]
        manifest["setup_url"] = auth["callback_url"]
        manifest["setup_on_update"] = bool(auth.get("setup_on_update", False))
    if webhook_url:
        manifest["hook_attributes"] = {"url": webhook_url, "active": True}
        manifest["default_events"] = list(spec.get("events", []))
    # NOTE: when webhook_url is absent we omit hook_attributes entirely rather
    # than passing a placeholder URL with active:false. GitHub's documented
    # behaviour for an absent hook_attributes is not stated precisely, so
    # `verify` reads hook_attributes back off the live App and reports it — do
    # not assume the webhook is off, check the verify output.
    return manifest


def manifest_form_html(
    manifest: dict, org: str, state: str, autosubmit: bool = True, owner_type: str = "org"
) -> str:
    """An HTML page that POSTs the manifest to GitHub.

    The manifest flow is a form POST, not a URL you can navigate to, so there
    has to be a page. The operator sees one button.

    `owner_type` picks which account the App is created under. It is not
    cosmetic: a **private** App can only be installed on its own owner's
    account ("Private GitHub Apps can only be installed on the user or
    organization account of the app owner"). Until the 2026-10-13 move every
    CI repo was owned by the *user* joyousaversion; since then they are owned
    by the org mattbakerconsulting, so the default --owner-type org is right
    again. Registering a private App under an account it cannot install on is
    the failure this flag exists to avoid.
    """
    if owner_type == "user":
        # GitHub has no /users/<login>/settings/apps/new — the personal form is
        # unscoped and creates under whoever is signed in.
        action = f"{GITHUB_WEB}/settings/apps/new"
    else:
        action = f"{GITHUB_WEB}/organizations/{urllib.parse.quote(org)}/settings/apps/new"
    action += "?" + urllib.parse.urlencode({"state": state})
    payload = html.escape(json.dumps(manifest), quote=True)
    submit = "<script>document.getElementById('f').submit()</script>" if autosubmit else ""
    return f"""<!doctype html>
<html><head><meta charset="utf-8"><title>Register {html.escape(manifest['name'])}</title></head>
<body style="font-family:system-ui;margin:3rem auto;max-width:34rem;line-height:1.5">
<h2>Register <code>{html.escape(manifest['name'])}</code> in <code>{html.escape(org)}</code></h2>
<p>GitHub will show you the permissions below before anything is created.</p>
<pre style="background:#f5f5f5;padding:1rem;overflow:auto">{html.escape(
    json.dumps(manifest['default_permissions'], indent=2))}</pre>
<form id="f" method="post" action="{html.escape(action, quote=True)}">
  <input type="hidden" name="manifest" value="{payload}">
  <button type="submit" style="font-size:1.1rem;padding:.6rem 1.2rem">Create GitHub App</button>
</form>
{submit}
</body></html>"""


# --------------------------------------------------------------- transport ---


def is_publicly_visible(slug: str) -> bool:
    """Is this App public? Measured, not asserted.

    `GET /app` does **not** return a `public` field — verify used to print
    `public: None` for every App, public or private, which reads like a
    measurement and is not one. The positive probe is the unauthenticated
    `GET /apps/{slug}`: it serves public Apps to anyone and 404s for private
    ones. Deliberately no Authorization header — as the owner we can see our
    own private App, which is exactly the question being asked.
    """
    req = urllib.request.Request(f"{GITHUB_API}/apps/{urllib.parse.quote(slug)}")
    req.add_header("Accept", "application/vnd.github+json")
    req.add_header("X-GitHub-Api-Version", "2022-11-28")
    req.add_header("User-Agent", "gardenio-github-app-register")
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            return resp.status == 200
    except urllib.error.HTTPError as exc:
        if exc.code == 404:
            return False
        raise


def _api(method: str, path: str, token: str | None = None, body: dict | None = None) -> dict:
    """One GitHub API call. Returns the decoded JSON body."""
    url = path if path.startswith("http") else GITHUB_API + path
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("Accept", "application/vnd.github+json")
    req.add_header("X-GitHub-Api-Version", "2022-11-28")
    req.add_header("User-Agent", "gardenio-github-app-register")
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    if data:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            raw = resp.read().decode()
    except urllib.error.HTTPError as exc:
        detail = exc.read().decode(errors="replace")[:600]
        raise SystemExit(f"GitHub {method} {url} -> HTTP {exc.code}\n{detail}") from None
    return json.loads(raw) if raw else {}


def convert_manifest_code(code: str) -> dict:
    """Exchange a manifest `code` for the App's credentials.

    Single-use and valid for one hour. The response contains the PEM; the caller
    must not print or persist it anywhere but SSM.
    """
    return _api("POST", f"/app-manifests/{urllib.parse.quote(code)}/conversions")


def code_from_url(raw: str) -> str:
    """Pull the `code` out of a callback URL the operator pasted back.

    Accepts a bare code too, so 'I copied the address bar' and 'I copied the
    code' both work.
    """
    if "?" not in raw and "/" not in raw:
        return raw.strip()
    query = urllib.parse.urlparse(raw.strip()).query
    values = urllib.parse.parse_qs(query).get("code") or []
    if not values:
        raise SystemExit(f"no ?code= in {raw!r}")
    return values[0]


# ------------------------------------------------------------------- jwt -----


def _b64u(raw: bytes) -> str:
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode()


def mint_jwt(pem: str, app_id: str, now: int | None = None) -> str:
    """Mint an App JWT (RS256) by shelling out to openssl.

    GitHub rejects a JWT whose `iat` is in its future, and laptop clocks drift,
    so `iat` is backdated 60s — this is GitHub's own documented advice. `exp` is
    capped at 10 minutes because GitHub refuses anything longer.
    """
    if shutil.which("openssl") is None:
        raise SystemExit("openssl not found; it is required to sign the App JWT")
    now = int(time.time()) if now is None else now
    header = _b64u(json.dumps({"alg": "RS256", "typ": "JWT"}, separators=(",", ":")).encode())
    claims = _b64u(
        json.dumps(
            {"iat": now - 60, "exp": now + 540, "iss": str(app_id)}, separators=(",", ":")
        ).encode()
    )
    signing_input = f"{header}.{claims}".encode()
    # The key goes to a 0600 temp file because openssl cannot read a key from a
    # pipe. It is removed in the finally block; this is the only moment the PEM
    # exists on disk and it is inside the scratch dir, not a Downloads folder.
    fd, keypath = tempfile.mkstemp(prefix="gh-app-", suffix=".pem")
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, "w") as handle:
            handle.write(pem)
        proc = subprocess.run(
            ["openssl", "dgst", "-sha256", "-sign", keypath],
            input=signing_input,
            capture_output=True,
            check=False,
        )
        if proc.returncode != 0:
            raise SystemExit(f"openssl signing failed: {proc.stderr.decode(errors='replace')}")
        return f"{header}.{claims}.{_b64u(proc.stdout)}"
    finally:
        try:
            os.unlink(keypath)
        except FileNotFoundError:
            pass


# ------------------------------------------------------------------- ssm -----


def ssm_path(app: str, field: str) -> str:
    return f"{SSM_PREFIX}/{app}/{field}"


def _aws(args: list[str], profile: str, region: str) -> str:
    cmd = ["aws", "--profile", profile, "--region", region] + args
    proc = subprocess.run(cmd, capture_output=True, text=True, check=False)
    if proc.returncode != 0:
        raise SystemExit(f"aws {' '.join(args[:2])} failed: {proc.stderr.strip()}")
    return proc.stdout


def ssm_put(app: str, field: str, value: str, *, secret: bool, kms: str, profile: str, region: str) -> str:
    """Write one parameter. Secrets are SecureString under our own CMK.

    The value goes through `--cli-input-json file://<tmp>` rather than
    `--value <secret>` so the PEM never appears in argv, where every other
    process on the box can read it out of /proc/<pid>/cmdline. That is the same
    leak class the plan's W6 hardening closed for agent pods, and it would be
    silly to reintroduce it here on the key that mints every one of those
    tokens.

    It has to be a temp file and not a pipe: AWS CLI v2 rejects
    `file:///dev/stdin` ("Invalid JSON received") because it does not read the
    path streamingly. The file is 0600 and unlinked in the finally block, the
    same posture mint_jwt uses for the signing key.
    """
    name = ssm_path(app, field)
    payload = {
        "Name": name,
        "Value": value,
        "Type": "SecureString" if secret else "String",
        "Overwrite": True,
        "Description": f"gardenio GitHub App ({app}) {field} — see gardenio/plans/github-app-onboarding.md",
    }
    if secret:
        payload["KeyId"] = kms
    fd, path = tempfile.mkstemp(prefix="gh-app-ssm-", suffix=".json")
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, "w") as handle:
            json.dump(payload, handle)
        _aws(["ssm", "put-parameter", "--cli-input-json", f"file://{path}"], profile, region)
    finally:
        try:
            os.unlink(path)
        except FileNotFoundError:
            pass
    return name


def ssm_get(app: str, field: str, profile: str, region: str) -> str:
    out = _aws(
        ["ssm", "get-parameter", "--name", ssm_path(app, field), "--with-decryption",
         "--query", "Parameter.Value", "--output", "text"],
        profile, region,
    )
    return out.rstrip("\n")


def store_conversion(app: str, conv: dict, *, kms: str, profile: str, region: str) -> list[str]:
    """Persist a conversion response. Returns the parameter names written."""
    written = []
    for field, key, is_secret in [
        ("app-id", "id", False),
        ("slug", "slug", False),
        ("client-id", "client_id", False),
        ("html-url", "html_url", False),
        ("private-key", "pem", True),
        ("webhook-secret", "webhook_secret", True),
        ("client-secret", "client_secret", True),
    ]:
        value = conv.get(key)
        if value in (None, ""):
            continue
        written.append(
            ssm_put(app, field, str(value), secret=is_secret, kms=kms, profile=profile, region=region)
        )
    return written


# -------------------------------------------------------------- callback -----


class _Handler(http.server.BaseHTTPRequestHandler):
    """Serves the auto-submitting form at / and catches the redirect at /callback."""

    page = b""
    state = ""
    result: dict = {}

    def log_message(self, *_args):
        """Swallow the per-request access log; the redirect URL carries the code."""

    # `do_GET` is BaseHTTPRequestHandler's dispatch name, not ours to rename.
    def do_GET(self):
        parsed = urllib.parse.urlparse(self.path)
        if parsed.path in ("/", "/index.html"):
            self._respond(200, type(self).page)
            return
        if parsed.path != "/callback":
            self._respond(404, b"not found")
            return
        params = urllib.parse.parse_qs(parsed.query)
        got_state = (params.get("state") or [""])[0]
        code = (params.get("code") or [""])[0]
        if not secrets.compare_digest(got_state, type(self).state):
            type(self).result = {"error": "state mismatch — refusing the code"}
            self._respond(400, b"state mismatch; nothing was stored")
            return
        if not code:
            type(self).result = {"error": "callback carried no code"}
            self._respond(400, b"no code in callback")
            return
        type(self).result = {"code": code}
        self._respond(200, b"<h2>Registered.</h2><p>Back to the terminal.</p>")

    def _respond(self, status: int, body: bytes):
        self.send_response(status)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def await_code(page: str, state: str, port: int, timeout: int = 300, app: str = "") -> str:
    """Serve the form, open a browser at it, and block until the code arrives.

    A timeout here is not a failure to recover from by re-running: if the
    operator clicked Create after the server gave up, the App exists, the code
    was delivered to a closed port, and re-running would try to register a
    second App under a name GitHub will refuse as a duplicate. The code lives
    for one hour in the browser's address bar, so the timeout message hands
    back the `convert` command rather than just exiting non-zero.
    """
    _Handler.page = page.encode()
    _Handler.state = state
    _Handler.result = {}
    server = http.server.HTTPServer(("127.0.0.1", port), _Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    url = f"http://localhost:{port}/"
    print(f"Opening {url} — click the button GitHub shows you.", file=sys.stderr)
    print(f"If no browser opens, visit that URL yourself.", file=sys.stderr)
    try:
        webbrowser.open(url)
    except Exception:
        pass
    deadline = time.time() + timeout
    try:
        while time.time() < deadline and not _Handler.result:
            time.sleep(0.25)
    finally:
        server.shutdown()
        # shutdown() stops serve_forever but leaves the listening socket open;
        # without this the timeout path leaks the port it tells you to re-run on.
        server.server_close()
    if not _Handler.result:
        raise SystemExit(
            f"timed out after {timeout}s waiting for the GitHub callback.\n"
            f"  If you did NOT click Create: nothing exists, just re-run this command.\n"
            f"  If you DID click Create: the App exists and re-running would be refused as a\n"
            f"  duplicate name. Copy the http://localhost:{port}/callback?... URL out of the\n"
            f"  browser's address bar (the code is good for one hour) and run:\n"
            f"    {sys.argv[0]} convert --app {app or '<app>'} --code-url '<that URL>'"
        )
    if "error" in _Handler.result:
        raise SystemExit(_Handler.result["error"])
    return _Handler.result["code"]


# -------------------------------------------------------------- commands -----


def cmd_manifest(args) -> int:
    redirect = args.redirect_url or f"http://localhost:{args.port}/callback"
    manifest = build_manifest(args.app, args.org, redirect, args.webhook_url, args.public)
    if args.html:
        sys.stdout.write(manifest_form_html(
            manifest, args.org, args.state or "manual",
            autosubmit=False, owner_type=args.owner_type,
        ))
    else:
        json.dump(manifest, sys.stdout, indent=2)
        sys.stdout.write("\n")
    return 0


def cmd_register(args) -> int:
    state = secrets.token_urlsafe(24)
    redirect = f"http://localhost:{args.port}/callback"
    manifest = build_manifest(args.app, args.org, redirect, args.webhook_url, args.public)
    page = manifest_form_html(manifest, args.org, state, owner_type=args.owner_type)
    code = await_code(page, state, args.port, args.timeout, app=args.app)
    return _convert_and_store(args, code)


def cmd_convert(args) -> int:
    return _convert_and_store(args, code_from_url(args.code_url))


def _convert_and_store(args, code: str) -> int:
    conv = convert_manifest_code(code)
    # Deliberately narrow: never dump `conv`, it holds the PEM.
    print(f"Created: {conv.get('html_url')}  (app id {conv.get('id')}, slug {conv.get('slug')})")
    written = store_conversion(args.app, conv, kms=args.kms_key, profile=args.profile, region=args.region)
    print("Stored in SSM:")
    for name in written:
        print(f"  {name}")
    print(f"\nVerify with:  {sys.argv[0]} verify --app {args.app}")
    return 0


def cmd_verify(args) -> int:
    """Read the App back from GitHub with its own key and diff it against MANIFESTS.

    This is P0's acceptance check: a 200 from GET /app proves the stored key
    signs for the stored app id, which is the only thing that matters before
    anything downstream trusts it.
    """
    app_id = ssm_get(args.app, "app-id", args.profile, args.region)
    pem = ssm_get(args.app, "private-key", args.profile, args.region)
    live = _api("GET", "/app", token=mint_jwt(pem, app_id))
    print(f"GET /app  200  {live.get('slug')}  (id {live.get('id')}, owner {live.get('owner', {}).get('login')})")
    print(f"  hook_attributes: {json.dumps(live.get('hook_attributes', {}))}")
    print(f"  events: {json.dumps(live.get('events', []))}")

    expected = MANIFESTS[args.app]["permissions"]
    actual = live.get("permissions", {})
    drift = []
    # GitHub's confirmation page lets the registrant edit the name before
    # submitting, and a name collision forces them to. Without this check the
    # live App can be called something MANIFESTS has never heard of and every
    # other line of verify output still reads clean.
    if live.get("name") != MANIFESTS[args.app]["name"]:
        drift.append(
            f"  NAME: want {MANIFESTS[args.app]['name']!r}, live {live.get('name')!r}"
            f" (slug {live.get('slug')!r})"
        )
    want_public = MANIFESTS[args.app]["public"]
    got_public = is_publicly_visible(live.get("slug", ""))
    print(f"  visibility: {'public' if got_public else 'private'}"
          f" (measured — GET /app carries no `public` field)")
    if got_public != want_public:
        drift.append(f"  VISIBILITY: want {'public' if want_public else 'private'}, "
                     f"live {'public' if got_public else 'private'}")
    for key, want in sorted(expected.items()):
        got = actual.get(key)
        if got != want:
            drift.append(f"  MISSING/WRONG {key}: want {want}, live {got!r}")
    for key, got in sorted(actual.items()):
        if key not in expected:
            drift.append(f"  EXTRA {key}: live {got!r}, not in MANIFESTS")
    if not drift:
        print(f"  name and permissions match MANIFESTS[{args.app!r}] exactly "
              f"({len(expected)} grants)")

    installs = _api("GET", "/app/installations", token=mint_jwt(pem, app_id))
    print(f"GET /app/installations  {len(installs)} install(s)")
    for inst in installs:
        account = inst.get("account", {}).get("login")
        print(f"  id={inst.get('id')} account={account} repos={inst.get('repository_selection')}")
        # The install page defaults to "All repositories" and it is one radio
        # button away from granting this App every repo the account will ever
        # have, including ones that do not exist yet. For the ARC App that is
        # `administration: write` on all of them. An install that is otherwise
        # healthy reads identically in every other line of this output.
        want_sel = MANIFESTS[args.app].get("repository_selection", "selected")
        if inst.get("repository_selection") != want_sel:
            drift.append(
                f"  INSTALL {inst.get('id')}: repository_selection is "
                f"{inst.get('repository_selection')!r}, want {want_sel!r}.\n"
                f"    Change it here: {inst.get('html_url')}"
            )
    if not installs:
        # Correct for the agents App as of 2026-09-22: P1 installed only the
        # ARC App (5029021). This one waits on P2, which is not authorised —
        # an install here before the minter exists would grant `contents:
        # write` to nothing that uses it.
        print("  (none yet — expected for 'agents' until P2; P1 installed the ARC App only)")
    if drift:
        print("Drift against MANIFESTS:")
        print("\n".join(drift))
    return 1 if drift else 0


def cmd_install_url(args) -> int:
    """Print the page that installs this App, and say what to tick on it.

    There is no API for creating an installation — it is a consent flow, so
    this one click cannot be automated any more than creating the org could.
    What *can* be removed is the chance of clicking it wrong, which for the ARC
    App means granting `administration: write` on every repo the account owns.
    """
    slug = ssm_get(args.app, "slug", args.profile, args.region)
    # `repository_ids[]` preselects the repositories AND flips the radio to
    # "Only select repositories". Without it the page opens on "All
    # repositories" and the correct answer is several taps away, which is how
    # the first ARC install landed on every repo the account owns. Naming the
    # repos in prose is not enough; the default has to be changed.
    query = []
    if args.target_id:
        query.append(("suggested_target_id", str(args.target_id)))
    for repo_id in args.repo_id:
        query.append(("repository_ids[]", str(repo_id)))
    url = f"{GITHUB_WEB}/apps/{urllib.parse.quote(slug)}/installations/new"
    if args.repo_id:
        url += "/permissions"
    if query:
        url += "?" + urllib.parse.urlencode(query)
    print(url)
    print()
    print("On that page:")
    print("  1. Choose the account that owns the CI repos (not the org — a private")
    print("     App installs only on its owner's own account; see D8).")
    print("  2. 'Only select repositories' must be the selected radio button.")
    print("     NOT 'All repositories' — that is the default and it grants this App")
    print("     every repo the account has, including ones created later.")
    for repo in args.repo:
        print(f"       - {repo}")
    if not args.repo_id:
        print("     (pass --repo-id to preselect them instead of trusting the click:")
        print("      gh api repos/OWNER/NAME --jq .id)")
    print("  3. Install.")
    print()
    print(f"Then: {os.path.basename(__file__)} verify --app {args.app}")
    print("      — it fails if the installation is not repository-scoped.")
    return 0


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="cmd", required=True)

    def common(p, need_org=False):
        p.add_argument("--app", required=True, choices=sorted(MANIFESTS))
        if need_org:
            p.add_argument("--org", required=True,
                           help="account that will own the App (org login, or user "
                                "login with --owner-type user)")
            p.add_argument("--owner-type", choices=("org", "user"), default="org",
                           help="create under an organization (default) or under the "
                                "signed-in personal account. A private App can only be "
                                "installed on its own owner's account.")
            # default None, not False: absent, MANIFESTS[app]["public"] decides
            # (build_manifest), so the public lands App is not registered private.
            p.add_argument("--public", action="store_true", default=None,
                           help="register the App public. Required if it must install on "
                                "an account other than its owner's; safe at P0 only "
                                "because the App has no webhook, events or user-auth leg.")
        p.add_argument("--profile", default=os.environ.get("AWS_PROFILE", "gardenio"))
        p.add_argument("--region", default=os.environ.get("AWS_REGION", "us-east-1"))
        p.add_argument("--kms-key", default=DEFAULT_KMS_ALIAS)
        p.add_argument("--port", type=int, default=DEFAULT_PORT)
        p.add_argument("--webhook-url", default=None,
                       help="P3 only; omitted at P0 so the App is registered inert")
        return p

    m = common(sub.add_parser("manifest", help="print the manifest (or --html the form)"), need_org=True)
    m.add_argument("--html", action="store_true", help="emit the submit page instead of JSON")
    m.add_argument("--state", default=None)
    m.add_argument("--redirect-url", default=None)
    m.set_defaults(func=cmd_manifest)

    r = common(sub.add_parser("register", help="one-click register, then store in SSM"), need_org=True)
    r.add_argument("--timeout", type=int, default=300)
    r.set_defaults(func=cmd_register)

    c = common(sub.add_parser("convert", help="exchange a pasted callback URL for credentials"))
    c.add_argument("--code-url", required=True, help="the callback URL, or the bare code")
    c.set_defaults(func=cmd_convert)

    v = common(sub.add_parser("verify", help="mint a JWT from SSM and call GET /app"))
    v.set_defaults(func=cmd_verify)

    i = common(sub.add_parser("install-url", help="print the install page and what to tick"))
    i.add_argument("--target-id", default=None,
                   help="numeric id of the account to install on; preselects it")
    i.add_argument("--repo", action="append", default=[],
                   help="owner/name to list as a repo to tick (repeatable)")
    i.add_argument("--repo-id", action="append", default=[],
                   help="numeric repo id to PRESELECT (repeatable). This also flips "
                        "the page off its 'All repositories' default, so prefer it "
                        "over --repo. `gh api repos/OWNER/NAME --jq .id`")
    i.set_defaults(func=cmd_install_url)

    args = parser.parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    sys.exit(main())
