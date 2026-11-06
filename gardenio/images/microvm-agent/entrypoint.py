#!/usr/bin/env python3

import json
import logging
import os
import re
import signal
import socket
import subprocess
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path


logging.basicConfig(
    level=os.environ.get("LOG_LEVEL", "INFO"),
    format="%(asctime)s %(levelname)s %(message)s",
)
LOG = logging.getLogger("gardenio-microvm")

HOOK_PREFIX = "/aws/lambda-microvms/runtime/v1"
RUNTIME_SECRET_DIR = Path("/home/agent/.runtime-secrets")
IDENTITY_PATH = RUNTIME_SECRET_DIR / "openziti/identity.json"
NATS_TOKEN_PATH = RUNTIME_SECRET_DIR / "gardenio/nats-token"
CLAUDE_CREDENTIALS_PATH = Path("/home/agent/.claude/.credentials.json")
SSH_PRIVATE_KEY_PATH = Path("/home/agent/.ssh/id_ed25519")
ZITI_BIN = "/opt/openziti/bin/ziti"
GARDENIO_ENTRYPOINT = "/opt/gardenio/bin/entrypoint.sh"
PROJECT_RE = re.compile(r"^[A-Za-z0-9._-]+$")
SERVICE_RE = re.compile(r"^[A-Za-z0-9._-]+:[0-9]{1,5}$")


class RuntimeManager:
    def __init__(self):
        self._lock = threading.Lock()
        self._ziti_process = None
        self._agent_process = None
        self._identity_secret_arn = os.environ.get("ZITI_IDENTITY_SECRET_ARN", "")
        self._agent_secret_arn = os.environ.get("GARDENIO_AGENT_SECRET_ARN", "")
        self._mode = os.environ.get("ZITI_MODE", "proxy")
        self._run_agent = os.environ.get("GARDENIO_RUN_AGENT", "false").lower() == "true"
        self._agent_env = {}

    def _load_payload(self, request_body):
        if not request_body:
            return

        body = json.loads(request_body)
        payload = body.get("runHookPayload")
        if not payload:
            return
        values = json.loads(payload) if isinstance(payload, str) else payload

        self._identity_secret_arn = values.get(
            "identitySecretArn", self._identity_secret_arn
        )
        if "agentSecretArn" in values:
            self._agent_secret_arn = values["agentSecretArn"]
        self._mode = values.get("mode", self._mode)
        if "runAgent" in values:
            self._run_agent = bool(values["runAgent"])
        elif "agentSecretArn" in values:
            self._run_agent = True

    @staticmethod
    def _get_secret(secret_arn):
        command = [
            "aws",
            "secretsmanager",
            "get-secret-value",
            "--secret-id",
            secret_arn,
            "--query",
            "SecretString",
            "--output",
            "text",
            "--no-cli-pager",
        ]
        result = subprocess.run(command, check=True, capture_output=True, text=True)
        return result.stdout.strip()

    def _write_identity(self):
        if not self._identity_secret_arn:
            raise RuntimeError("ZITI_IDENTITY_SECRET_ARN is not configured")

        parsed = json.loads(self._get_secret(self._identity_secret_arn))
        if isinstance(parsed, dict) and "identity" in parsed:
            parsed = parsed["identity"]
            if isinstance(parsed, str):
                parsed = json.loads(parsed)

        if (
            isinstance(parsed, dict)
            and parsed.get("type") == "ziti_enrollment_jwt"
            and parsed.get("jwt")
        ):
            self._enroll_identity(parsed["jwt"])
            return

        if not isinstance(parsed, dict) or "ztAPI" not in parsed:
            raise RuntimeError(
                "Secrets Manager value is not an enrolled OpenZiti identity JSON"
                " document or a ziti_enrollment_jwt document"
            )

        IDENTITY_PATH.parent.mkdir(parents=True, mode=0o700, exist_ok=True)
        IDENTITY_PATH.write_text(json.dumps(parsed), encoding="utf-8")
        IDENTITY_PATH.chmod(0o600)

    def _enroll_identity(self, jwt):
        # Option A of the ziti-access-broker contract (ka-0000005494): the
        # per-run secret carries a one-time enrollment JWT and the private key
        # is generated here, inside the VM — it never exists anywhere else.
        # The JWT file is removed after enrollment so only the enrolled
        # identity remains.
        #
        # The JWT is single-use, but the resume hook re-runs start(): reuse
        # the identity enrolled by the initial run hook instead of failing on
        # a consumed JWT.
        if IDENTITY_PATH.is_file() and IDENTITY_PATH.stat().st_size > 0:
            LOG.info("per-run OpenZiti identity already enrolled; reusing it")
            return
        IDENTITY_PATH.parent.mkdir(parents=True, mode=0o700, exist_ok=True)
        jwt_path = IDENTITY_PATH.parent / "enroll.jwt"
        jwt_path.write_text(str(jwt), encoding="utf-8")
        jwt_path.chmod(0o600)
        try:
            LOG.info("enrolling per-run OpenZiti identity from one-time JWT")
            subprocess.run(
                [
                    ZITI_BIN,
                    "edge",
                    "enroll",
                    "--jwt",
                    str(jwt_path),
                    "--out",
                    str(IDENTITY_PATH),
                ],
                check=True,
                capture_output=True,
                text=True,
            )
        except subprocess.CalledProcessError as exc:
            detail = (exc.stderr or exc.stdout or "").strip()[-500:]
            raise RuntimeError(
                f"OpenZiti enrollment from the one-time JWT failed: {detail}"
            ) from None
        finally:
            jwt_path.unlink(missing_ok=True)
        if not IDENTITY_PATH.is_file():
            raise RuntimeError("OpenZiti enrollment produced no identity file")
        IDENTITY_PATH.chmod(0o600)
        LOG.info("per-run OpenZiti identity enrolled")

    @staticmethod
    def _write_secret_file(path, value, mode):
        path.parent.mkdir(parents=True, mode=0o700, exist_ok=True)
        if isinstance(value, (dict, list)):
            content = json.dumps(value)
        else:
            content = str(value)
        path.write_text(content, encoding="utf-8")
        path.chmod(mode)

    def _load_agent_secret(self):
        self._agent_env = {}
        if not self._agent_secret_arn:
            return

        config = json.loads(self._get_secret(self._agent_secret_arn))
        if not isinstance(config, dict):
            raise RuntimeError("Gardenio agent secret must be a JSON object")

        environment = config.get("environment", {})
        if not isinstance(environment, dict):
            raise RuntimeError("Gardenio agent secret environment must be a JSON object")
        for key, value in environment.items():
            if not re.fullmatch(r"[A-Z][A-Z0-9_]*", key) or not isinstance(value, str):
                raise RuntimeError(f"invalid Gardenio environment entry {key!r}")
            self._agent_env[key] = value

        if "natsToken" in config:
            self._write_secret_file(NATS_TOKEN_PATH, config["natsToken"], 0o400)
            self._agent_env.setdefault("COOP_NATS_TOKEN_FILE", str(NATS_TOKEN_PATH))
        if "claudeCredentials" in config:
            self._write_secret_file(
                CLAUDE_CREDENTIALS_PATH, config["claudeCredentials"], 0o600
            )
        if "sshPrivateKey" in config:
            self._write_secret_file(SSH_PRIVATE_KEY_PATH, config["sshPrivateKey"], 0o600)

    @staticmethod
    def _stop_process(process, name):
        if process is None:
            return None
        if process.poll() is None:
            LOG.info("stopping %s", name)
            process.terminate()
            try:
                process.wait(timeout=20)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
        return None

    def _stop_locked(self):
        self._agent_process = self._stop_process(self._agent_process, "Gardenio agent")
        self._ziti_process = self._stop_process(self._ziti_process, "OpenZiti proxy")
        for path in (
            IDENTITY_PATH,
            NATS_TOKEN_PATH,
            CLAUDE_CREDENTIALS_PATH,
            SSH_PRIVATE_KEY_PATH,
        ):
            path.unlink(missing_ok=True)
        self._agent_env = {}

    @staticmethod
    def _proxy_services():
        raw = os.environ.get("ZITI_PROXY_SERVICES", "")
        services = [item.strip() for item in raw.split(",") if item.strip()]
        if not services:
            raise RuntimeError("ZITI_PROXY_SERVICES is empty")
        for service in services:
            if not SERVICE_RE.fullmatch(service):
                raise RuntimeError(f"invalid Ziti proxy service mapping {service!r}")
            port = int(service.rsplit(":", 1)[1])
            if not 1 <= port <= 65535:
                raise RuntimeError(f"invalid Ziti proxy port in {service!r}")
        return services

    @staticmethod
    def _wait_for_ports(services, timeout=45):
        pending = {int(service.rsplit(":", 1)[1]) for service in services}
        deadline = time.monotonic() + timeout
        while pending and time.monotonic() < deadline:
            for port in list(pending):
                try:
                    with socket.create_connection(("127.0.0.1", port), timeout=1):
                        pending.remove(port)
                except OSError:
                    pass
            if pending:
                time.sleep(1)
        if pending:
            raise RuntimeError(
                "Ziti proxy listeners did not become ready: "
                + ", ".join(str(port) for port in sorted(pending))
            )

    def _start_ziti(self):
        if self._mode == "host":
            command = [ZITI_BIN, "tunnel", "host", "--identity", str(IDENTITY_PATH)]
            services = []
        elif self._mode == "proxy":
            services = self._proxy_services()
            command = [
                ZITI_BIN,
                "tunnel",
                "proxy",
                "--identity",
                str(IDENTITY_PATH),
                *services,
            ]
        else:
            raise RuntimeError(f"unsupported ZITI_MODE {self._mode!r}")

        LOG.info("starting OpenZiti %s-mode tunnel", self._mode)
        self._ziti_process = subprocess.Popen(command)
        if services:
            self._wait_for_ports(services)

    def _prepare_agent_environment(self):
        env = os.environ.copy()
        env.update(self._agent_env)
        env.setdefault("HOME", "/home/agent")
        env.setdefault("BOAT_WORKSPACE", "/home/agent/workspace")
        env.setdefault("BEADS_HTTP_URL", "http://127.0.0.1:18080")
        env.setdefault("COOP_BROKER_URL", "http://127.0.0.1:19800")
        env.setdefault("COOP_MUX_URL", "http://127.0.0.1:19800")
        env.setdefault("COOP_HOST", "127.0.0.1")
        env.setdefault("COOP_PORT", "8181")
        env.setdefault("COOP_HEALTH_PORT", "9090")
        env.setdefault("COOP_METRICS_PORT", "9093")
        env.setdefault("POD_IP", "127.0.0.1")
        env.setdefault("DISABLE_AUTOUPDATER", "1")

        if any(item.startswith("gardenio-prod-nats:") for item in self._proxy_services()):
            env.setdefault("BEADS_NATS_URL", "nats://127.0.0.1:14222")
            env.setdefault("COOP_NATS_URL", "nats://127.0.0.1:14222")
            env.setdefault("COOP_NATS_FORWARD", "1")
        if any(
            item.startswith("gardenio-prod-litellm:")
            for item in self._proxy_services()
        ):
            env.setdefault("ANTHROPIC_BASE_URL", "http://127.0.0.1:14000")
        if any(
            item.startswith("gardenio-prod-slackbridge:")
            for item in self._proxy_services()
        ):
            env.setdefault("SLACK_BRIDGE_URL", "http://127.0.0.1:18090")
        if any(
            item.startswith("gardenio-prod-beads-viewer:")
            for item in self._proxy_services()
        ):
            env.setdefault("BEADS_VIEWER_INTERNAL_URL", "http://127.0.0.1:18095")
        return env

    @staticmethod
    def _prepare_primary_repo(env):
        git_url = env.get("BOAT_GIT_URL", "")
        project = env.get("BOAT_PROJECT", "")
        if not git_url:
            return
        if not PROJECT_RE.fullmatch(project):
            raise RuntimeError("BOAT_PROJECT must be set to a path-safe name")

        destination = Path("/home/agent/bot") / project / "work"
        if (destination / ".git").exists():
            return
        destination.parent.mkdir(parents=True, exist_ok=True)
        command = ["git", "clone", "--depth", "1"]
        branch = env.get("BOAT_GIT_BRANCH", "")
        if branch:
            command.extend(["--branch", branch])
        command.extend([git_url, str(destination)])
        LOG.info("cloning primary project repository for %s", project)
        subprocess.run(command, check=True, env=env)

    def _start_agent(self):
        if not self._run_agent:
            return
        env = self._prepare_agent_environment()
        self._prepare_primary_repo(env)
        LOG.info(
            "starting Gardenio agent role=%s project=%s name=%s",
            env.get("BOAT_ROLE", "unknown"),
            env.get("BOAT_PROJECT", "none"),
            env.get("BOAT_AGENT", "unknown"),
        )
        self._agent_process = subprocess.Popen([GARDENIO_ENTRYPOINT], env=env)

    def start(self, request_body=""):
        with self._lock:
            self._load_payload(request_body)
            self._stop_locked()
            self._write_identity()
            if self._run_agent:
                self._load_agent_secret()
            else:
                self._agent_env = {}
            try:
                self._start_ziti()
                self._start_agent()
            except Exception:
                self._stop_locked()
                raise

    def stop(self):
        with self._lock:
            self._stop_locked()

    def suspend(self):
        # The AWS /suspend hook is only a NOTIFICATION that the platform is about
        # to snapshot this MicroVM: SuspendMicrovm "suspends a running MicroVM,
        # preserving its full memory and disk state" (lambdamicrovms SDK) and
        # thaws it in place on ResumeMicrovm. It MUST NOT tear the agent down.
        #
        # THE BUG THIS FIXES (ka-0000002917): suspend used to share the terminate
        # branch and call RUNTIME.stop() -> _stop_locked(), which SIGTERM-killed
        # coop (destroying the very process the snapshot exists to preserve) and
        # unlinked every per-run secret. Both are wrong for a freeze: the
        # controller's lifecycle.Suspend deliberately leaves the bead open and
        # does NOT revoke per-run credentials or Ziti leases so the resumed VM
        # stays authenticated across standby (12h session creds cover the
        # window) — wiping the on-disk secrets here would strand the resumed VM
        # on missing material. Leave every process + secret intact and let AWS
        # capture a live, resumable snapshot; the lock mirrors start()/stop() so
        # a concurrent hook still serializes.
        with self._lock:
            LOG.info(
                "suspend hook: leaving agent + proxy running for AWS snapshot (no teardown)"
            )

    def status(self):
        with self._lock:
            ziti_running = (
                self._ziti_process is not None and self._ziti_process.poll() is None
            )
            agent_running = (
                self._agent_process is not None and self._agent_process.poll() is None
            )
            if not ziti_running:
                status = "waiting"
            elif self._run_agent and not agent_running:
                status = "degraded"
            else:
                status = "ok"
            return {
                "status": status,
                "zitiTunnelRunning": ziti_running,
                "agentRequested": self._run_agent,
                "gardenioAgentRunning": agent_running,
                "mode": self._mode,
                "identityConfigured": bool(self._identity_secret_arn),
                "agentSecretConfigured": bool(self._agent_secret_arn),
            }


RUNTIME = RuntimeManager()


class Handler(BaseHTTPRequestHandler):
    def log_message(self, message, *args):
        LOG.info("%s - %s", self.address_string(), message % args)

    def _body(self):
        length = int(self.headers.get("Content-Length", "0"))
        return self.rfile.read(length).decode("utf-8") if length else ""

    def _respond(self, status, payload):
        encoded = json.dumps(payload).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(encoded)))
        self.end_headers()
        self.wfile.write(encoded)

    def do_GET(self):
        if self.path == "/health":
            self._respond(200, RUNTIME.status())
            return
        self._respond(404, {"error": "not found"})

    def do_POST(self):
        hook = self.path.removeprefix(f"{HOOK_PREFIX}/")
        body = self._body()

        try:
            if hook == "ready":
                self._respond(200, {"status": "ready"})
            elif hook == "validate":
                versions = {}
                for name, command in {
                    "ziti": [ZITI_BIN, "version"],
                    "gb": ["gb", "--help"],
                    "kd": ["kd-bin", "--help"],
                    "coop": ["coop", "--version"],
                    "node": ["node", "--version"],
                    "claude": ["claude", "--version"],
                }.items():
                    result = subprocess.run(
                        command, check=True, capture_output=True, text=True, timeout=30
                    )
                    versions[name] = (result.stdout or result.stderr).strip().splitlines()[0]
                self._respond(200, {"status": "valid", "versions": versions})
            elif hook in ("run", "resume"):
                RUNTIME.start(body)
                self._respond(200, RUNTIME.status())
            elif hook == "suspend":
                # NON-DESTRUCTIVE: AWS is about to snapshot the live VM (full
                # memory + disk preserved) and thaw it in place on resume. Do
                # NOT kill the agent or wipe secrets (ka-0000002917).
                RUNTIME.suspend()
                self._respond(200, {"status": "suspended"})
            elif hook == "terminate":
                RUNTIME.stop()
                self._respond(200, {"status": "stopped"})
            else:
                self._respond(404, {"error": "unknown hook"})
        except Exception as exc:
            LOG.exception("hook %s failed", hook)
            self._respond(503, {"error": str(exc), "hook": hook})


if __name__ == "__main__":
    def shutdown(signum, _frame):
        LOG.info("received signal %s; stopping runtime", signum)
        RUNTIME.stop()
        raise SystemExit(0)

    signal.signal(signal.SIGTERM, shutdown)
    signal.signal(signal.SIGINT, shutdown)
    LOG.info("starting Lambda MicroVM lifecycle server on port 8080")
    ThreadingHTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
