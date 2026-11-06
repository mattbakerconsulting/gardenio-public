# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>
"""LiteLLM pre-call callback that upgrades agent prompt-cache breakpoints to a
1-hour TTL — the proxy lever chosen for the prompt-cache-utilization epic
(ka-0000003469, impl ka-0000004405, lever investigation ka-0000003909).

WHY THIS EXISTS (the economics)
------------------------------------------------------------------------------
Claude Code stamps its prompt-cache breakpoints with the DEFAULT ephemeral
cache_control: ``{"type": "ephemeral"}``, whose server-side TTL is 5 minutes.
gardenio's lifecycle is respawn-heavy: a pod despawns, the conversation resumes
minutes later, and the >5m gap means the cached prefix has already expired — so
every respawn pays a cold (full base-input) prefix. Measured cache-hit
utilization sits around 22%.

Anthropic's 1-hour cache TTL (now GA — the ``extended-cache-ttl-2025-04-11``
beta header is NO LONGER required; you simply send ``ttl: "1h"`` inside the
cache_control block) keeps the prefix warm across those respawn gaps. The cost
trade is favourable at our utilization: a 1h cache WRITE costs 2x base input
(vs 1.25x for the 5m write), but cache READS stay 0.1x base input either way.
With respawn-heavy traffic re-reading the same large prefix many times per
write, lifting the read window from 5m to 1h converts cold misses into 0.1x
reads — a net win well below break-even.

WHY THE DOCUMENTED ``cache_control_injection_points`` LEVER DOES NOT WORK HERE
------------------------------------------------------------------------------
(Full write-up on ka-0000003909.) LiteLLM's documented config-only
``cache_control_injection_points`` setting is wired ONLY through
``Logging.get_chat_completion_prompt`` — i.e. the OpenAI ``/v1/chat/completions``
path. But 100% of gardenio's Claude Code agents talk to the proxy over the
Anthropic-native ``/v1/messages`` PASSTHROUGH route (``ANTHROPIC_BASE_URL``).
That route dispatches through ``base_llm_http_handler.anthropic_messages_handler``
and never calls ``get_chat_completion_prompt``, so the injection-points hook is a
silent no-op for our traffic. (Same class of gotcha as the thinking-capture
callback, ka-0000005574.)

THE LEVER THAT DOES RUN ON /v1/messages
------------------------------------------------------------------------------
``ProxyLogging.pre_call_hook`` runs for EVERY route_type, including
``anthropic_messages`` (litellm/proxy/common_request_processing.py — it is
called unconditionally before the model call). It iterates ``litellm.callbacks``
and invokes ``async_pre_call_hook(data=...)`` on any CustomLogger that overrides
it; a dict return REPLACES the outgoing request body. ``data`` here is the raw
Anthropic request: ``system`` (str | list[block]), ``messages`` (each
``content`` str | list[block]), and ``tools`` — exactly where Claude Code's
ephemeral cache_control breakpoints live. So this callback walks the body and
rewrites every ``{"type": "ephemeral"}`` breakpoint to carry ``ttl``.

Unlike the thinking-capture callback, this one needs NO monkeypatch of the
success-callback lists: the proxy invokes ``async_pre_call_hook`` directly off
``litellm.callbacks``, which is exactly where the ``litellm_settings.callbacks``
string lands. It is also pure stdlib (no nats, no event loop, no network) and
import-safe.

HOW TO WIRE IT INTO THE PROXY
------------------------------------------------------------------------------
Add to the proxy config (the Helm chart does this when
``litellm.cacheTTL.enabled``):

    litellm_settings:
      callbacks: ["litellm_cache_ttl_callback.instance"]

and mount this file at ``/app/litellm_cache_ttl_callback.py`` (subPath), so
litellm's ``get_instance_fn`` resolves the bare module name as a file under the
``--config`` dir (/app). The TTL is read from ``LITELLM_CACHE_TTL`` (default
``1h``); set it to ``5m`` to effectively disable the upgrade without removing the
callback. No change to coop, Claude Code, or agent pods.
"""

from __future__ import annotations

import logging
import os
from typing import Any, Optional

from litellm.integrations.custom_logger import CustomLogger

_log = logging.getLogger("cache-ttl-callback")

# Anthropic accepts exactly these two TTL tokens on a cache_control block.
_ALLOWED_TTLS = ("5m", "1h")


def _resolve_ttl() -> str:
    """Target TTL for ephemeral cache breakpoints. Defaults to 1h; an invalid
    value falls back to 1h so a typo can never silently disable caching."""
    ttl = (os.environ.get("LITELLM_CACHE_TTL") or "1h").strip()
    if ttl not in _ALLOWED_TTLS:
        _log.warning(
            "LITELLM_CACHE_TTL=%r is not one of %s; falling back to '1h'",
            ttl,
            _ALLOWED_TTLS,
        )
        return "1h"
    return ttl


def _upgrade_cache_control(obj: Any, ttl: str) -> int:
    """If ``obj`` is an ephemeral cache_control dict missing a ttl, stamp it.

    Returns the number of breakpoints upgraded (0 or 1). Only touches
    ``{"type": "ephemeral"}`` blocks with no existing ``ttl`` — an explicit ttl
    set by the client (or already-1h) is left untouched, so this is idempotent.
    """
    if not isinstance(obj, dict):
        return 0
    if obj.get("type") != "ephemeral":
        return 0
    if "ttl" in obj:
        return 0
    obj["ttl"] = ttl
    return 1


def _walk_content_blocks(content: Any, ttl: str) -> int:
    """Stamp cache_control on every block in a content value (str content has no
    block to carry cache_control, so it is a no-op)."""
    upgraded = 0
    if isinstance(content, list):
        for block in content:
            if isinstance(block, dict) and "cache_control" in block:
                upgraded += _upgrade_cache_control(block.get("cache_control"), ttl)
    return upgraded


def upgrade_request_cache_ttl(data: dict, ttl: str) -> int:
    """Walk an Anthropic /v1/messages request body and upgrade every ephemeral
    cache breakpoint to ``ttl``. Returns the count upgraded.

    Covers the three places Claude Code sets cache_control:
      * top-level ``system`` (list of content blocks)
      * each ``messages[*].content`` block
      * each entry of ``tools`` (a tool may carry a top-level cache_control)
    Defensive throughout: any unexpected shape is skipped, never raised."""
    if not isinstance(data, dict):
        return 0
    upgraded = 0

    # system: usually a list of content blocks on the Anthropic native route.
    upgraded += _walk_content_blocks(data.get("system"), ttl)

    messages = data.get("messages")
    if isinstance(messages, list):
        for msg in messages:
            if not isinstance(msg, dict):
                continue
            # message-level cache_control (set for string content)
            if "cache_control" in msg:
                upgraded += _upgrade_cache_control(msg.get("cache_control"), ttl)
            upgraded += _walk_content_blocks(msg.get("content"), ttl)

    tools = data.get("tools")
    if isinstance(tools, list):
        for tool in tools:
            if isinstance(tool, dict) and "cache_control" in tool:
                upgraded += _upgrade_cache_control(tool.get("cache_control"), ttl)

    return upgraded


class CacheTTLPreCallHook(CustomLogger):
    """Upgrades ephemeral prompt-cache breakpoints to a 1h TTL before the
    upstream Anthropic call. Registered via litellm_settings.callbacks; the
    proxy invokes async_pre_call_hook directly off litellm.callbacks."""

    def __init__(self) -> None:
        self.ttl = _resolve_ttl()
        # last upgrade count — exposed for tests / ad-hoc verification.
        self.last_upgraded: int = 0

    async def async_pre_call_hook(
        self,
        user_api_key_dict: Any,
        cache: Any,
        data: dict,
        call_type: str,
    ) -> Optional[dict]:
        # Only the Anthropic message routes carry Claude Code's cache_control
        # breakpoints. Other call types (embeddings, etc.) are passed through
        # untouched. We accept both the passthrough route_type and the
        # completion types defensively — the upgrade is a no-op when no
        # ephemeral breakpoints are present, so being permissive is safe.
        if self.ttl == "5m":
            # 5m is the Anthropic default; nothing to upgrade. Acts as an
            # in-place kill switch without removing the callback.
            return None
        try:
            upgraded = upgrade_request_cache_ttl(data, self.ttl)
        # Telemetry/perf hardening must never break inference: swallow and pass
        # the original body through unmodified on any unexpected shape.
        except Exception as e:  # pragma: no cover - defensive guard
            _log.warning("cache-ttl upgrade skipped (%s): %s", call_type, e)
            return None
        self.last_upgraded = upgraded
        if upgraded:
            _log.debug(
                "upgraded %d cache breakpoint(s) to ttl=%s (call_type=%s)",
                upgraded,
                self.ttl,
                call_type,
            )
            return data
        return None


# litellm resolves "litellm_cache_ttl_callback.instance" to this object.
instance = CacheTTLPreCallHook()
