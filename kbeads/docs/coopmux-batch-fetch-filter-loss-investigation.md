# Coopmux batch-fetch filter loss → unfiltered /v1/beads scans

**Bead:** ka-0000004415 (spawned from ka-0000006889 post-startup OOM investigation)
**Date:** 2026-04-30
**Status:** root-cause confirmed, fix in `coop/crates/mux/src/beads.rs`

## TL;DR

The original bead description suspected the coopmux **account-cache refresh**
(`/v1/beads?type=account&status=open` and `/v1/beads?type=config&status=open`)
of dropping its filter and triggering full-table scans. That suspicion is
**wrong** — those calls work correctly.

The actual culprit is `BeadsClient::get_beads_batch`, used every 30 s by
`enrich_claimed_titles` (`coop/crates/mux/src/lib.rs:223`). It issued
`GET /v1/beads?id=<csv>` — but `handleListBeads` does **not** parse an `id`
query parameter, so the param is silently ignored and the handler returns the
**entire bead table** (~340 MB / ~27 K beads on the live cluster). Two such
calls fire per enrich pass.

The fix routes batch fetches through the dedicated
`GET /v1/batch/beads?ids=<csv>` endpoint (`http_batch.go`), which the server
already implements with a 50-ID cap and the right response shape.

## Evidence

### Reqwest preserves the query string (client is not at fault)

A direct test against a one-shot TCP listener confirmed reqwest's URL
construction (`format!("{}{}", base_url, path)` → `Client::request(method, &url)`)
ships the query intact:

```
GET /v1/beads?type=account&status=open HTTP/1.1
```

The new unit tests in `crates/mux/src/beads.rs` lock this behavior in:
`list_account_beads_sends_filter_query`,
`list_auth_configs_sends_filter_query`,
`get_beads_batch_uses_batch_endpoint_with_ids_param`.

### Server applies the type/status filters correctly

Curl from inside the gardenio namespace, against the live `gardenio-beads`
service:

| Request                                  |       Bytes | Time   |
|------------------------------------------|------------:|--------|
| `/v1/beads?type=account&status=open`     |       8 958 | 0.008s |
| `/v1/beads?type=config&status=open`      |     240 184 | 0.013s |
| `/v1/beads`                              | 343 945 188 | 11.0 s |
| `/v1/beads?id=ka-0000001761`             | 343 945 188 | 17.1 s |
| `/v1/beads?id=ka-0000001761,ka-0000006525` | 343 945 188 | 17.6 s |

The first two confirm `type=` and `status=` parsing in
`handleListBeads` works. The bottom two are the smoking gun: passing `id=…`
behaves identically to passing **nothing**, because the handler has no
`q.Get("id")` branch — the parameter is silently dropped and the query
degenerates into the unbounded list.

(These measurements were taken before MR !1877 landed on main. After !1877's
default-1000/max-10000 cap, an `?id=` request would still drop the filter,
but the response is now bounded to ~12 MB / 1 000 rows instead of 340 MB.
The filter-loss is still latent and still wastes 10× the rows of a
properly-filtered batch fetch.)

### Why the original diagnosis missed this

* The audit middleware logs only `r.URL.Path`, not `RawQuery` — so the
  access log shows `path=/v1/beads` for both refresh calls and batch calls,
  with no way to tell which is which.
* Rough call-rate matched the suspected loop. 1 167 calls in 15 h ≈
  78 calls/h ≈ 1.3/min. Two refresh calls per minute = 120/h, and two batch
  calls per enrich (every 30 s) = 240/h *when sessions are present*. The
  observed rate is consistent with batch calls firing intermittently (only
  when there are sessions whose metadata carries an `agent_bead_id`), which
  was misread as the steady-state refresh loop.
* The slow-call distribution (8–11 s) and 340 MB payload size match the
  unfiltered scan exactly. Filtered refresh calls finish in <10 ms and
  return ~9 KB — they're below the `INFO` threshold in
  `loggingMiddleware`, so they don't appear in default-level logs at all.
  This made it look like every visible call was the refresh loop.

## Fix

`crates/mux/src/beads.rs`:

* `get_beads_batch` now hits `GET /v1/batch/beads?ids=…`.
* It parses the `{"results":[{"id","bead","error"}]}` envelope from
  `handleBatchGetBeads` (`internal/server/http_batch.go`).
* It chunks by 50 IDs to respect the server-side cap (a 51st ID was
  previously silently dropped — the unfiltered behavior masked this).
* New unit tests assert the GET line, the response parsing, and lock
  query-preservation for the two refresh calls so a future regression in
  reqwest's URL handling fails loudly.

The server-side `handleListBeads` is intentionally **not** changed to
accept `id`/`ids`. The `/v1/batch/beads` endpoint is the canonical batch
path and already exists; teaching the list handler to also accept `id=…`
would just create a second way to do the same thing. MR !1877's cap
already prevents the worst-case 340 MB amplification.

## Suggested follow-ups

* **Audit-log RawQuery** (suggested approach #1 in the bead). Even with this
  fix, future filter-loss bugs will reproduce as silent slow scans because
  the access log can't distinguish `/v1/beads?type=account&status=open`
  from `/v1/beads?frobnicate=banana`. A small change to
  `loggingMiddleware` in `internal/server/http.go:357` to log
  `r.URL.RawQuery` (with a sensitive-key redaction list — `token`,
  `code`, etc.) would make the next bug of this shape diagnose itself in
  the existing logs. Not done in this MR to keep the patch scope tight,
  but worth filing as a follow-up.
* **Reject unknown query params on `/v1/beads`?** Tempting, but risky:
  many callers add custom params (jacks, dashboards) and a strict mode
  could break compat. Leave as-is unless we see another silent-filter bug.

## Memory impact

Each `get_beads_batch` previously buffered ~340 MB of JSON in `bytes()`,
then handed it to `serde_json` — duplicating it briefly during decode. With
this fix the same call returns ~few-KB-per-ID × ≤50 IDs ≈ tens of KB.
This is the dominant remaining contributor to the post-startup
~100 Mi/min memory growth in `gardenio-beads` (ka-0000006889). MR !1877
addresses the server-side response cap; this MR addresses the client-side
filter so that a properly-shaped batch query is issued in the first place.
