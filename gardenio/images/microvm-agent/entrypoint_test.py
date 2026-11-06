#!/usr/bin/env python3
"""Unit tests for entrypoint.py's suspend vs terminate lifecycle hooks.

Guards the ka-0000002917 fix: the AWS /suspend hook must be NON-DESTRUCTIVE.

Before the fix, do_POST routed both "suspend" and "terminate" to
RUNTIME.stop() -> _stop_locked(), which SIGTERM-killed the coop agent process
(the very process AWS SuspendMicrovm snapshots — "preserving its full memory and
disk state" — and thaws in place on ResumeMicrovm) AND unlinked every per-run
secret. Both defeat native suspend/resume: the controller's lifecycle.Suspend
deliberately leaves the bead open and does NOT revoke per-run credentials or
Ziti leases so the resumed VM stays authenticated across standby. So AWS
suspending an egress-only VM ~T+60s after boot killed a WORKING agent ~13s into
its first turn (CloudWatch stream agent/gd-rtnPwmNkDH, 2026-08-17).

Contract pinned here:
  (1) SUSPEND IS NON-DESTRUCTIVE — RuntimeManager.suspend() leaves the agent +
      ziti processes running and every secret file on disk, so AWS captures a
      live, resumable snapshot.
  (2) TERMINATE STILL TEARS DOWN — RuntimeManager.stop() (the /terminate hook)
      still kills the processes and wipes secrets, unchanged.
  (3) The two are DISTINCT do_POST branches — suspend calls suspend(), terminate
      calls stop() — never the shared kill path again.

Hermetic: loads entrypoint.py by path (no HTTP server — that only starts under
__main__), stubs subprocess.Popen with a fake process, and points the secret
paths at a tmp dir. No AWS, no network, no coop, no ziti.
"""
import importlib.util
import inspect
import unittest
from pathlib import Path
from unittest import mock


def _load_entrypoint():
    here = Path(__file__).resolve().parent
    spec = importlib.util.spec_from_file_location("entrypoint", here / "entrypoint.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class _FakeProcess:
    """Minimal subprocess.Popen stand-in: alive until terminate()/kill()."""

    def __init__(self):
        self._alive = True
        self.terminated = False
        self.killed = False

    def poll(self):
        return None if self._alive else 0

    def terminate(self):
        self.terminated = True
        self._alive = False

    def kill(self):
        self.killed = True
        self._alive = False

    def wait(self, timeout=None):
        return 0


class SuspendHookTest(unittest.TestCase):
    def setUp(self):
        self.ep = _load_entrypoint()
        # Redirect every secret path into a throwaway tmp dir so the terminate
        # path's unlink() can't touch anything real, and we can assert on it.
        import tempfile

        self._tmpdir = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmpdir.cleanup)
        base = Path(self._tmpdir.name)
        self._secret_paths = {
            "IDENTITY_PATH": base / "identity.json",
            "NATS_TOKEN_PATH": base / "nats-token",
            "CLAUDE_CREDENTIALS_PATH": base / "credentials.json",
            "SSH_PRIVATE_KEY_PATH": base / "id_ed25519",
        }
        self._patches = []
        for name, path in self._secret_paths.items():
            path.write_text("secret-material", encoding="utf-8")
            p = mock.patch.object(self.ep, name, path)
            p.start()
            self._patches.append(p)
        self.addCleanup(lambda: [p.stop() for p in self._patches])

        # A RuntimeManager holding two live fake processes, as if an agent turn
        # is in flight.
        self.rt = self.ep.RuntimeManager()
        self.agent = _FakeProcess()
        self.ziti = _FakeProcess()
        self.rt._agent_process = self.agent
        self.rt._ziti_process = self.ziti
        self.rt._agent_env = {"BOAT_AGENT": "test"}

    def _secrets_present(self):
        return all(path.exists() for path in self._secret_paths.values())

    # (1) SUSPEND IS NON-DESTRUCTIVE
    def test_suspend_leaves_processes_running(self):
        self.rt.suspend()
        self.assertIsNotNone(self.rt._agent_process, "agent handle must survive suspend")
        self.assertIsNotNone(self.rt._ziti_process, "ziti handle must survive suspend")
        self.assertFalse(self.agent.terminated, "coop must NOT be SIGTERMed on suspend")
        self.assertFalse(self.agent.killed, "coop must NOT be killed on suspend")
        self.assertFalse(self.ziti.terminated, "ziti must NOT be torn down on suspend")

    def test_suspend_preserves_secrets_and_env(self):
        self.rt.suspend()
        self.assertTrue(
            self._secrets_present(),
            "per-run secrets must survive standby so the resumed VM stays authenticated",
        )
        self.assertEqual(
            self.rt._agent_env,
            {"BOAT_AGENT": "test"},
            "agent env must survive suspend",
        )

    # (2) TERMINATE STILL TEARS DOWN
    def test_terminate_kills_processes_and_wipes_secrets(self):
        self.rt.stop()
        self.assertTrue(self.agent.terminated, "terminate must still SIGTERM coop")
        self.assertTrue(self.ziti.terminated, "terminate must still tear down ziti")
        self.assertIsNone(self.rt._agent_process)
        self.assertIsNone(self.rt._ziti_process)
        self.assertFalse(
            any(path.exists() for path in self._secret_paths.values()),
            "terminate must still wipe per-run secrets",
        )
        self.assertEqual(self.rt._agent_env, {})


class DoPostRoutingTest(unittest.TestCase):
    """The suspend/terminate do_POST branches must call distinct methods —
    suspend() (non-destructive) vs stop() (teardown) — never the shared kill
    path again (ka-0000002917)."""

    def setUp(self):
        self.ep = _load_entrypoint()

    def _run_hook(self, hook):
        calls = []
        fake_runtime = mock.Mock()
        fake_runtime.suspend.side_effect = lambda: calls.append("suspend")
        fake_runtime.stop.side_effect = lambda: calls.append("stop")
        fake_runtime.status.return_value = {"status": "ok"}

        handler = self.ep.Handler.__new__(self.ep.Handler)
        handler.path = f"{self.ep.HOOK_PREFIX}/{hook}"
        responses = []
        with mock.patch.object(self.ep, "RUNTIME", fake_runtime), mock.patch.object(
            self.ep.Handler, "_body", return_value=""
        ), mock.patch.object(
            self.ep.Handler,
            "_respond",
            side_effect=lambda status, payload: responses.append((status, payload)),
        ):
            self.ep.Handler.do_POST(handler)
        return calls, responses

    def test_suspend_branch_calls_suspend_not_stop(self):
        calls, responses = self._run_hook("suspend")
        self.assertEqual(calls, ["suspend"], "suspend hook must call RUNTIME.suspend()")
        self.assertEqual(responses[0][0], 200)
        self.assertEqual(responses[0][1], {"status": "suspended"})

    def test_terminate_branch_calls_stop(self):
        calls, responses = self._run_hook("terminate")
        self.assertEqual(calls, ["stop"], "terminate hook must call RUNTIME.stop()")
        self.assertEqual(responses[0][0], 200)
        self.assertEqual(responses[0][1], {"status": "stopped"})

    def test_source_no_longer_shares_kill_branch(self):
        # Regression guard: the exact defective shape ("suspend", "terminate")
        # routing both to a single stop() must not return.
        src = inspect.getsource(self.ep.Handler.do_POST)
        self.assertNotIn(
            'hook in ("suspend", "terminate")',
            src,
            "suspend must no longer share the terminate kill branch",
        )


if __name__ == "__main__":
    unittest.main()
