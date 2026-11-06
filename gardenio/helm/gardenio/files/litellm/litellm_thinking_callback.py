# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>
"""LiteLLM logging callback that captures extended-thinking PLAINTEXT and
publishes it to NATS — the capture source chosen for ka-0000004553.

WHY THIS EXISTS (the empirical finding, see report ka-0000001698 / decision
ka-0000001028)
------------------------------------------------------------------------------
The telepathy v1 plan (epic ka-0000007154) assumed coop already forwards
thinking *plaintext* and only needed the 512-byte truncation cap lifted. That
is WRONG. coop's `.thinking` NATS events are fed by the Tier-2 LogWatcher of
Claude Code's *persisted* session-log JSONL
(coop/crates/cli/src/transport/nats_forward.rs:121, extract_thinking at :225).
Anthropic strips reasoning plaintext on persist, keeping only the crypto
`signature`: every persisted thinking block is `{"thinking":"", "signature":
"..."}`. Verified empirically (24/24 in prod logs; reproduced 7/7 in this
agent's own live session log). Un-truncating an empty string yields nothing.

The plaintext exists only transiently in the LIVE model response stream
(Anthropic SSE `thinking_delta`), before persistence. Every gardenio agent's
inference already transits a LiteLLM proxy (this agent runs with
ANTHROPIC_BASE_URL=https://litellm.app.devops.acme.example). A LiteLLM logging
callback sees the fully-assembled streamed response — including the reasoning
plaintext — on `async_log_success_event`. That is capture **Source A** from the
task, the intentional interrelation with the litellm-in-cluster epic
(ka-0000000560).

WHAT THIS CALLBACK DOES
------------------------------------------------------------------------------
On every successful (streaming or non-streaming) completion it extracts the
reasoning plaintext (LiteLLM normalizes Anthropic thinking into
`message.reasoning_content` and `message.thinking_blocks`) and, when non-empty,
publishes a JSON event onto

    {prefix}.messages.{actor}.thinking          (prefix defaults to coop.events)

with an envelope that is BYTE-COMPATIBLE with what coop's NatsForwarder emits
(nats_forward.rs:165-198) so the beads-viewer consumer (ka-0000007528) needs no
new code path:

    {
      "event_type": "thinking",
      "text":        "<full reasoning plaintext>",   # NON-EMPTY (the whole point)
      "full_length": <int>,
      "actor":       "<agent name>",
      "session_id":  "<litellm call id, best-effort>",
      "ts":          <epoch_ms>,
      "source":      "litellm-callback"               # provenance marker
    }

The `actor` segment is taken from the per-request `x-litellm-tags: agent:<name>`
header that gardenio already injects (see this agent's ANTHROPIC_CUSTOM_HEADERS),
falling back to BOAT_AGENT. That keeps per-agent routing identical to coop.

HOW TO WIRE IT INTO THE PROXY
------------------------------------------------------------------------------
On the in-cluster LiteLLM (epic ka-0000000560), add to the proxy config:

    litellm_settings:
      callbacks: ["litellm_thinking_callback.instance"]

and mount this file on PYTHONPATH with COOP_NATS_URL / COOP_NATS_PREFIX /
COOP_NATS_TOKEN(_FILE) in the proxy pod env (the same NATS coords coop already
gets via podspec.go:631). No change to coop, Claude Code, or agent pods.

This module is import-safe with no NATS/event loop running; the NATS client is
created lazily on first publish.

THE /v1/messages PASSTHROUGH GOTCHA (ka-0000005574)
------------------------------------------------------------------------------
Listing this callback under `litellm_settings.callbacks` is NOT sufficient on
its own. The proxy only does `litellm.callbacks.extend([...])`; it never adds
the callback to `litellm._async_success_callback`. Plain `/v1/chat/completions`
calls survive because litellm's per-request `function_setup` fans
`litellm.callbacks` into `_async_success_callback`. But every gardenio Claude
Code agent uses the Anthropic-native `/v1/messages` PASSTHROUGH route, whose
streaming-logging handler iterates ONLY `_async_success_callback` and never runs
`function_setup` — so a callback that lives only in `litellm.callbacks` is
silently skipped and not one thinking event is ever published. This module
therefore self-registers into `_async_success_callback` at import time (see
`_register_on_success_callback_lists`). Likewise, on the passthrough path the
raw request headers are not always reattached to the callback kwargs, so
`_resolve_actor` also reads the agent tag out of
`standard_logging_object.request_tags`.
"""

from __future__ import annotations

import asyncio
import json
import os
import time
from typing import Any, Optional

import litellm
from litellm.integrations.custom_logger import CustomLogger

try:  # nats-py is only needed at runtime in the proxy pod
    # nats-py ships no type stubs; this optional runtime-only import is a known
    # false positive for the type checker.
    import nats  # type: ignore
# import guard: nats is absent everywhere but the proxy pod, so this fallback
# branch is unreachable under CI/type-check and cannot be covered by a test.
except Exception:  # pragma: no cover - import guard for non-proxy environments
    # rebinds the module name to a None sentinel; mypy flags the redefinition,
    # which is exactly the intended fallback behaviour.
    nats = None  # type: ignore


def _epoch_ms() -> int:
    return int(time.time() * 1000)


def _extract_thinking(response_obj: Any, kwargs: dict) -> str:
    """Pull reasoning plaintext out of a (possibly streamed) LiteLLM response.

    LiteLLM normalizes Anthropic extended thinking onto the assistant message:
      - message.reasoning_content : the concatenated reasoning text
      - message.thinking_blocks   : [{"type":"thinking","thinking": "...", ...}]
    For streaming calls the fully-assembled response is handed to the success
    callback as kwargs["complete_streaming_response"].
    """
    resp = kwargs.get("complete_streaming_response") or response_obj
    if resp is None:
        return ""
    try:
        # resp is duck-typed (object with .choices OR a dict); the .get path is
        # guarded by the surrounding try/except, so the union-attr warning is a
        # false positive here.
        choices = getattr(resp, "choices", None) or resp.get("choices")  # type: ignore[union-attr]
        msg = choices[0].message if hasattr(choices[0], "message") else choices[0]["message"]
    except Exception:
        return ""

    # Preferred: the flattened reasoning_content string.
    reasoning = _attr(msg, "reasoning_content")
    if isinstance(reasoning, str) and reasoning.strip():
        return reasoning.strip()

    # Fallback: concatenate thinking_blocks (handles providers that only emit
    # block form).
    blocks = _attr(msg, "thinking_blocks")
    if blocks:
        parts = []
        for b in blocks:
            t = b.get("thinking") if isinstance(b, dict) else _attr(b, "thinking")
            if isinstance(t, str) and t:
                parts.append(t)
        joined = "\n".join(parts).strip()
        if joined:
            return joined
    return ""


def _attr(obj: Any, name: str) -> Any:
    if obj is None:
        return None
    if isinstance(obj, dict):
        return obj.get(name)
    return getattr(obj, name, None)


def _actor_from_tags(tags: Any) -> Optional[str]:
    """Pull the agent name out of a list/string of `agent:<name>` tags."""
    if isinstance(tags, str):
        tags = tags.split(",")
    if not isinstance(tags, (list, tuple)):
        return None
    for tag in tags:
        tag = str(tag).strip()
        if tag.startswith("agent:"):
            name = tag[len("agent:"):]
            if name:
                return name
    return None


def _resolve_actor(kwargs: dict) -> str:
    """Per-request agent name, mirroring coop's actor segment.

    gardenio injects `x-litellm-tags: agent:<name>` on every agent request
    (see ANTHROPIC_CUSTOM_HEADERS). Resolution order:

    1. The `x-litellm-tags` request header (the OpenAI-style /v1/chat/completions
       path surfaces it under litellm_params.proxy_server_request.headers).
    2. The `request_tags` field of the standard_logging_object. This is the
       reliable source on the Anthropic-native /v1/messages PASSTHROUGH path,
       which is what 100% of Claude Code agents use: that path rebuilds the
       callback kwargs from scratch and does NOT always carry the raw request
       headers, but litellm DOES populate request_tags from x-litellm-tags into
       the standard logging payload. Without this fallback every passthrough
       thought is attributed to "unknown" (ka-0000005574).
    3. BOAT_AGENT env (single-agent / non-proxy capture).
    4. "unknown".
    """
    def _dig(*path):
        cur: Any = kwargs
        for k in path:
            if not isinstance(cur, dict):
                return None
            cur = cur.get(k)
        return cur

    headers = (
        _dig("litellm_params", "proxy_server_request", "headers")
        or _dig("proxy_server_request", "headers")
        or _dig("headers")
        or {}
    )
    if isinstance(headers, dict):
        name = _actor_from_tags(
            headers.get("x-litellm-tags") or headers.get("X-Litellm-Tags") or ""
        )
        if name:
            return name

    # Passthrough path: x-litellm-tags is flattened into the standard logging
    # payload's request_tags (and, on some versions, litellm_params.metadata.tags).
    slo = kwargs.get("standard_logging_object")
    if isinstance(slo, dict):
        name = _actor_from_tags(slo.get("request_tags"))
        if name:
            return name
    name = _actor_from_tags(_dig("litellm_params", "metadata", "tags"))
    if name:
        return name

    return os.environ.get("BOAT_AGENT") or "unknown"


def _session_id(kwargs: dict, response_obj: Any) -> str:
    resp = kwargs.get("complete_streaming_response") or response_obj
    sid = _attr(resp, "id")
    if isinstance(sid, str) and sid:
        return sid
    return kwargs.get("litellm_call_id", "") or ""


class ThinkingNatsCallback(CustomLogger):
    """Publishes non-empty thinking plaintext to coop.events.messages.*.thinking."""

    def __init__(self) -> None:
        self._nc = None  # lazily-connected async_nats client
        self._lock = asyncio.Lock()
        self.prefix = os.environ.get("COOP_NATS_PREFIX", "coop.events")
        self.url = os.environ.get("COOP_NATS_URL", "nats://gardenio-nats:4222")
        # last published payload — exposed for the spike's e2e verifier
        self.last_published: Optional[dict] = None

    async def _client(self):
        if self._nc is not None and self._nc.is_connected:
            return self._nc
        async with self._lock:
            if self._nc is not None and self._nc.is_connected:
                return self._nc
            if nats is None:
                raise RuntimeError("nats-py not installed in this environment")
            token = self._token()
            user = os.environ.get("COOP_NATS_USER", "")
            if user and token:
                # Multi-user server auth (subject isolation) matches
                # user/password entries only — send the token as this user's
                # password (ka-0000000002).
                self._nc = await nats.connect(self.url, user=user, password=token)
            else:
                self._nc = await nats.connect(self.url, token=token or None)
            return self._nc

    @staticmethod
    def _token() -> str:
        tf = os.environ.get("COOP_NATS_TOKEN_FILE", "")
        if tf and os.path.exists(tf):
            try:
                return open(tf).read().strip()
            except Exception:
                pass
        return os.environ.get("COOP_NATS_TOKEN", "")

    def _build_payload(self, kwargs: dict, response_obj: Any) -> Optional[dict]:
        text = _extract_thinking(response_obj, kwargs)
        if not text:
            return None  # no thinking this turn — publish nothing (parity w/ coop)
        actor = _resolve_actor(kwargs)
        return {
            "event_type": "thinking",
            "text": text,
            "full_length": len(text),
            "actor": actor,
            "session_id": _session_id(kwargs, response_obj),
            "ts": _epoch_ms(),
            "source": "litellm-callback",
        }

    async def _publish(self, payload: dict) -> None:
        subject = f"{self.prefix}.messages.{payload['actor']}.thinking"
        nc = await self._client()
        await nc.publish(subject, json.dumps(payload).encode())
        await nc.flush(timeout=5)
        self.last_published = payload

    # ── LiteLLM callback entry points ──────────────────────────────────────
    async def async_log_success_event(self, kwargs, response_obj, start_time, end_time):
        try:
            payload = self._build_payload(kwargs, response_obj)
            if payload is not None:
                await self._publish(payload)
        except Exception as e:  # never break inference on a telemetry error
            import logging

            logging.getLogger("thinking-callback").warning("publish failed: %s", e)

    def log_success_event(self, kwargs, response_obj, start_time, end_time):
        # Sync entrypoint (non-async proxy paths). Best-effort: schedule on the
        # running loop if there is one, else skip (the async path is primary).
        try:
            payload = self._build_payload(kwargs, response_obj)
            if payload is None:
                return
            loop = asyncio.get_event_loop()
            if loop.is_running():
                loop.create_task(self._publish(payload))
        except Exception:
            pass


# LiteLLM resolves "litellm_thinking_callback.instance" to this object.
instance = ThinkingNatsCallback()


def _register_on_success_callback_lists() -> None:
    """Force-register `instance` into litellm's SUCCESS callback lists.

    WHY THIS IS REQUIRED (ka-0000005574)
    ------------------------------------------------------------------------
    Listing this callback under `litellm_settings.callbacks` only makes the
    proxy run `litellm.callbacks.extend([...])` (see
    litellm.proxy.common_utils.callback_utils.initialize_callbacks_on_proxy).
    It is NEVER added to `litellm.success_callback` / `litellm._async_success_callback`.

    For ordinary `/v1/chat/completions` calls that is fine: litellm's per-request
    `function_setup` (litellm.utils) fans every entry of `litellm.callbacks` into
    `_async_success_callback`, so the success handler finds us. That is the only
    path the original capture spike (ka-0000004553) ever exercised.

    But 100% of gardenio's Claude Code agents hit the Anthropic-native
    `/v1/messages` PASSTHROUGH route, whose streaming-logging path
    (PassThroughStreamingHandler._route_streaming_logging_to_handler ->
    Logging.async_success_handler) iterates ONLY `_async_success_callback`
    (via get_combined_callback_list(global_callbacks=litellm._async_success_callback)).
    It does not run `function_setup`, so a callback that lives only in
    `litellm.callbacks` is silently skipped — `async_log_success_event` is never
    invoked and not a single thinking event is ever published. Empirically: the
    in-cluster cutover landed, yet 0 thinking events across the entire retained
    MESSAGE_EVENTS stream while usage/assistant_text flowed normally.

    Registering directly into the success-callback lists (idempotently, via the
    logging_callback_manager which de-dupes) makes the passthrough success
    handler see us. Import-safe: if litellm's internals shift, we swallow the
    error rather than break proxy startup.
    """
    try:
        mgr = getattr(litellm, "logging_callback_manager", None)
        if mgr is not None:
            mgr.add_litellm_callback(instance)
            # Only the ASYNC success list — the passthrough streaming-logging
            # handler dispatches via async_log_success_event. Deliberately NOT
            # registering on the sync `success_callback` list: that would make
            # async calls additionally schedule the sync success_handler
            # (_should_run_sync_callbacks_for_async_calls), risking a duplicate
            # publish. The async list is sufficient for both the passthrough and
            # the /v1/chat/completions paths.
            mgr.add_litellm_async_success_callback(instance)
            return
        # Fallback for older litellm without the manager: append guarding dupes.
        for attr in ("callbacks", "_async_success_callback"):
            lst = getattr(litellm, attr, None)
            if isinstance(lst, list) and instance not in lst:
                lst.append(instance)
    # registration must never break proxy import: any failure to attach the
    # telemetry callback is swallowed so the proxy still boots. This defensive
    # catch-all is unreachable under CI and cannot be covered by a test.
    except Exception:  # pragma: no cover - never break proxy import on telemetry
        pass


def _patch_passthrough_error_chunk_tolerance() -> None:
    """Stop a mid-stream Anthropic error event from killing the logging task.

    WHY THIS IS REQUIRED (ka-0000007673)
    ------------------------------------------------------------------------
    On the Anthropic-native `/v1/messages` PASSTHROUGH route, Anthropic can
    inject a mid-stream error event into an otherwise-successful (HTTP 200)
    stream, e.g.

        {"type":"error","error":{"type":"overloaded_error"|"rate_limit_error",...}}

    The client already received its 200 and the good chunks — the error lands
    only in the asynchronous SUCCESS-logging task. There,
    PassThroughStreamingHandler._route_streaming_logging_to_handler ->
    AnthropicPassthroughLoggingHandler._build_complete_streaming_response loops
    every buffered SSE event through
    AnthropicModelResponseIterator.convert_str_chunk_to_generic_chunk ->
    chunk_parser. chunk_parser's `type_chunk == "error"` branch
    (litellm/llms/anthropic/chat/handler.py:851) raises AnthropicError, and the
    surrounding loop only catches (StopIteration, StopAsyncIteration) — so the
    AnthropicError propagates out and the whole logging asyncio task dies.

    Impact: lost telemetry for that turn, INCLUDING this module's thinking
    publish (async_log_success_event is never reached). Observed 2/417 turns in
    a 40m window on gardenio-litellm.

    THE FIX
    ------------------------------------------------------------------------
    Wrap `convert_str_chunk_to_generic_chunk` so an AnthropicError raised while
    converting one buffered event is swallowed and that event is dropped
    (returns None — which _build_complete_streaming_response already filters
    out: it only appends non-None chunks). The remaining good chunks still
    assemble into a complete response and logging proceeds.

    Why patch HERE and why this is safe:
      * We run the stock upstream `ghcr.io/berriai/litellm` image; this callback
        module (imported at proxy startup via litellm_settings.callbacks) is the
        only code-injection seam, and it already monkeypatches litellm internals
        (see _register_on_success_callback_lists).
      * `convert_str_chunk_to_generic_chunk` is used ONLY on the passthrough
        SUCCESS-LOGGING path (its docstring says so; grep confirms the only
        callers are the anthropic/cohere passthrough logging handlers). The LIVE
        client stream uses ModelResponseIterator.__next__ / __anext__, which call
        chunk_parser directly and are NOT touched here — so the client still sees
        the real error behaviour; only the post-hoc logging reconstruction is
        made tolerant.
      * Import-safe: any failure to attach the patch is swallowed so the proxy
        still boots (telemetry hardening must never break inference).
    """
    try:
        from litellm.llms.anthropic.chat.handler import ModelResponseIterator
        from litellm.llms.anthropic.common_utils import AnthropicError
    # if litellm's internals move, skip the patch rather than break proxy import
    except Exception:  # pragma: no cover - import guard for non-proxy environments
        return

    # Idempotent: don't double-wrap if the module is imported twice.
    if getattr(ModelResponseIterator, "_gardenio_error_tolerant", False):
        return

    _orig_convert = ModelResponseIterator.convert_str_chunk_to_generic_chunk

    def _convert_tolerant(self, chunk):
        try:
            return _orig_convert(self, chunk)
        except AnthropicError as e:
            # Mid-stream Anthropic error event on the success-logging path —
            # drop this event and keep building from the good chunks instead of
            # letting it kill the logging task (ka-0000007673).
            import logging

            logging.getLogger("thinking-callback").warning(
                "tolerating mid-stream Anthropic error chunk in passthrough "
                "logging: %s",
                e,
            )
            return None

    ModelResponseIterator.convert_str_chunk_to_generic_chunk = _convert_tolerant
    ModelResponseIterator._gardenio_error_tolerant = True


# Run at import so the registration is in place the moment litellm loads the
# callback string from litellm_settings.callbacks.
_register_on_success_callback_lists()
_patch_passthrough_error_chunk_tolerance()
