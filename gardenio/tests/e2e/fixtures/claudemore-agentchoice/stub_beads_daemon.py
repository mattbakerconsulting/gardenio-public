#!/usr/bin/env python3
"""Stub beads-daemon for the claudemore agent-choice e2e (bead ka-0000005944).

The Phase-2 agent-choice legs (T2 right-bead selection, T3 decision divergence,
T4 wrap-up blockers judgment) assert real *bead side-effects* the real `kd`/`gb`
binaries produce — a step beyond the hooks e2e (ka-0000004452), whose legs
assert file/gate side-effects only. Those side-effects normally land in the
Postgres-backed beads daemon, which a hermetic CI shard has no way to stand up
($0, no cluster). This stub stands in for that daemon so the harness can assert
the OUTCOMES (R4-DET-1) without a real Postgres.

It is deliberately small and stateful:

  * Serves the handful of endpoints `kd`/`gb` touch — GET /v1/beads[/{id}]
    (reads), PATCH /v1/beads/{id} (claim, wrap-up), POST /v1/beads
    (decision/create), POST /v1/beads/{id}/close, and a catch-all 200 for
    comments / gates / hook-relay publishes.
  * RECORDS every write to a JSONL log (argv[2]) so the harness asserts the
    exact mutation — the claim PATCH's {status,assignee}, the decision create's
    options, the wrap-up PATCH's fields.wrapup — never the model's prose.
  * Reflects claim/close mutations onto an in-memory bead map (seeded from
    $STUB_SEED_BEADS) so reads the CLI issues mid-command stay consistent.
  * Emulates the server-side decision-divergence guard
    (kbeads/internal/server/decision_validation.go: zero options, or a single
    ack/done/continue/etc option → reject) so T3's non-divergent leg fails
    loudly through the REAL `gb decision create` exactly as prod would, rather
    than silently accepting a no-fork decision.

It is NOT a beads daemon: no auth, no dependency graph, no validation beyond the
decision guard. It exists only to give the real binaries a truthful-enough
endpoint to write to and to capture what they wrote.
"""
import http.server
import json
import os
import re
import sys

PORT = int(sys.argv[1])
WRITELOG = sys.argv[2]  # JSONL: one record per write the CLI made

# In-memory bead store, seeded by the harness so the CLI's mid-command reads
# (e.g. kd claim's claimability pre-check) resolve consistently.
BEADS = {}
_seed = os.environ.get("STUB_SEED_BEADS", "")
if _seed:
    for _b in json.loads(_seed):
        BEADS[_b["id"]] = _b

# Mirror of decision_validation.go's reservedNoOpOptionIDs: a single option with
# one of these ids is an acknowledgement, not a fork, and is rejected.
RESERVED_NOOP = {"ack", "done", "continue", "acknowledge", "wrap", "noop"}
_decision_seq = [0]


def wlog(rec):
    with open(WRITELOG, "a") as f:
        f.write(json.dumps(rec) + "\n")


class Handler(http.server.BaseHTTPRequestHandler):
    def _read(self):
        n = int(self.headers.get("Content-Length", 0))
        return self.rfile.read(n) if n else b""

    def _json(self, code, obj):
        b = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    # ── reads ────────────────────────────────────────────────────────────────
    def do_GET(self):
        m = re.match(r"^/v1/beads/([^/?]+)$", self.path)
        if m:
            bid = m.group(1)
            if bid in BEADS:
                return self._json(200, BEADS[bid])
            return self._json(404, {"error": "bead not found: " + bid})
        if self.path.startswith("/v1/beads"):
            # A list query. Filters are ignored — the harness controls the seed,
            # so returning the full seeded set is sufficient and keeps the stub
            # dumb. kd/gb list responses are a bare array (advice ka-0000003286).
            return self._json(200, list(BEADS.values()))
        return self._json(200, {})

    # ── writes ───────────────────────────────────────────────────────────────
    def do_PATCH(self):
        d = self._parse_body()
        m = re.match(r"^/v1/beads/([^/?]+)$", self.path)
        bid = m.group(1) if m else ""
        wlog({"op": "patch", "path": self.path, "id": bid, "body": d})
        if bid in BEADS and isinstance(d, dict):
            if "status" in d:
                BEADS[bid]["status"] = d["status"]
            if "assignee" in d:
                BEADS[bid]["assignee"] = d["assignee"]
            if isinstance(d.get("fields"), dict):
                BEADS[bid].setdefault("fields", {}).update(d["fields"])
            return self._json(200, BEADS[bid])
        return self._json(200, {"id": bid, **(d if isinstance(d, dict) else {})})

    def do_POST(self):
        d = self._parse_body()
        p = self.path

        if p == "/v1/beads":
            return self._create_bead(d)

        mc = re.match(r"^/v1/beads/([^/?]+)/close$", p)
        if mc:
            bid = mc.group(1)
            wlog({"op": "close", "id": bid, "body": d})
            if bid in BEADS:
                BEADS[bid]["status"] = "closed"
            return self._json(200, BEADS.get(bid, {"id": bid, "status": "closed"}))

        # Comments, decision-gate satisfy, hook-relay publishes, agents/* — record
        # and 200. These are side-channels the harness does not assert on, but a
        # non-200 here would make the CLI print a scary (harmless) warning.
        wlog({"op": "post", "path": p, "body": d})
        return self._json(200, {"ok": True})

    def do_DELETE(self):
        wlog({"op": "delete", "path": self.path})
        return self._json(200, {})

    # ── helpers ──────────────────────────────────────────────────────────────
    def _parse_body(self):
        raw = self._read()
        if not raw:
            return {}
        try:
            return json.loads(raw)
        except json.JSONDecodeError:
            return {}

    def _create_bead(self, d):
        btype = d.get("type", "") if isinstance(d, dict) else ""
        if btype == "decision":
            opts = (d.get("fields") or {}).get("options") or []
            reject = self._decision_reject_reason(opts)
            wlog({
                "op": "create", "type": "decision", "options": opts,
                "reject": reject,
                "prompt": (d.get("fields") or {}).get("prompt", ""),
            })
            if reject:
                # Mirror the daemon's 400 so the REAL `gb decision create` exits
                # non-zero and prints the guard's message (T3 non-divergent leg).
                return self._json(400, {"error": reject, "field": "options"})
            _decision_seq[0] += 1
            did = "gd-DEC%05d" % _decision_seq[0]
            bead = {
                "id": did, "title": d.get("title", ""), "type": "decision",
                "status": "open", "fields": d.get("fields", {}),
                "labels": d.get("labels", []),
            }
            BEADS[did] = bead
            return self._json(200, bead)

        # Non-decision create (rare in these legs — record and echo).
        wlog({"op": "create", "type": btype, "title": d.get("title", "") if isinstance(d, dict) else ""})
        nid = (d.get("id") if isinstance(d, dict) else None) or ("gd-NEW%05d" % len(BEADS))
        bead = {"id": nid, "status": "open"}
        if isinstance(d, dict):
            bead.update(d)
            bead["id"] = nid
        BEADS[nid] = bead
        return self._json(200, bead)

    @staticmethod
    def _decision_reject_reason(opts):
        if len(opts) == 0:
            return "decision has no options"
        if len(opts) == 1:
            oid = str(opts[0].get("id", "")).strip().lower()
            if oid in RESERVED_NOOP:
                return 'decision has a single "%s" option (no fork)' % oid
        return None

    def log_message(self, *args):
        pass  # quiet


if __name__ == "__main__":
    http.server.HTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
