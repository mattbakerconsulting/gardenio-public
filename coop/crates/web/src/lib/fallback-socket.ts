// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

// A WebSocket-compatible transport that transparently falls back to
// Server-Sent Events (SSE) + HTTP POST when a real WebSocket upgrade cannot
// complete.
//
// Why this exists: the unified gardenio host sits behind AWS Verified Access
// (AVA), whose HTTP/2 endpoints have no classic WebSocket-upgrade path
// (RFC 8441 extended-CONNECT is not negotiated). So `new WebSocket(wss://…)`
// never opens and the coopmux dashboard shows "Disconnected" — even though
// plain GET/POST sail through. SSE rides a plain `GET` `text/event-stream`,
// which AVA carries, and a paired `POST` channel carries the upstream control
// messages a WebSocket would otherwise send. See ka-0000006211.
//
// `FallbackSocket` exposes the small slice of the `WebSocket` API that the mux
// frontend actually uses (`readyState`, `OPEN`, `send`, `close`, the four
// `on*` handlers), so `WsRpc` and the existing call sites work unchanged. It
// first attempts a real WebSocket; if that fails to open within a short
// window (or errors/closes before opening), it tears it down and switches to
// the SSE+POST pair against the matching `/sse/*` endpoints.

import { withBasePath } from "@/lib/basePath";

/** Event carrying a payload, mirroring `MessageEvent`/SSE `message`. */
export interface MessageLike {
  data: string;
}

type MessageHandler = ((ev: MessageLike) => void) | null;
type EventHandler = ((ev: Event) => void) | null;

/** How long to wait for a real WS to open before falling back to SSE. */
const WS_OPEN_TIMEOUT_MS = 4000;

/**
 * Derive the `/sse/*` endpoint pair from a `/ws/*` path.
 *   /ws/mux            → { stream: /sse/mux,            send: /sse/mux/send }
 *   /ws/{id}?subscribe → { stream: /sse/session/{id}?…, send: /sse/session/{id}/send }
 */
export function sseEndpoints(wsPath: string): { stream: string; send: string } | null {
  const [path, query] = wsPath.split("?");
  const qs = query ? `?${query}` : "";
  if (path === "/ws/mux") {
    return { stream: `/sse/mux${qs}`, send: "/sse/mux/send" };
  }
  const m = path.match(/^\/ws\/([^/]+)$/);
  if (m) {
    const id = m[1];
    return { stream: `/sse/session/${id}${qs}`, send: `/sse/session/${id}/send` };
  }
  return null;
}

/** Generate a short random connection id correlating the SSE GET with its POSTs. */
function randomConnId(): string {
  if (typeof crypto !== "undefined" && "randomUUID" in crypto) {
    return crypto.randomUUID();
  }
  return `c${Date.now().toString(36)}${Math.random().toString(36).slice(2, 10)}`;
}

export class FallbackSocket {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSING = 2;
  static readonly CLOSED = 3;
  // Instance copies so `socket.OPEN`-style access (and WsRpc's
  // `WebSocket.OPEN` comparisons against `readyState`) line up.
  readonly OPEN = 1;
  readonly CONNECTING = 0;
  readonly CLOSING = 2;
  readonly CLOSED = 3;

  onopen: EventHandler = null;
  onmessage: MessageHandler = null;
  onclose: EventHandler = null;
  onerror: EventHandler = null;

  readyState = 0; // CONNECTING

  /** "ws" while a real socket is live, "sse" after fallback. */
  transport: "ws" | "sse" = "ws";

  private ws: WebSocket | null = null;
  private openTimer: ReturnType<typeof setTimeout> | null = null;
  private opened = false;
  private closedByCaller = false;

  // SSE state
  private es: EventSource | null = null;
  private connId = randomConnId();
  private sendUrl = "";
  private token: string | null = null;

  private readonly wsUrl: string;
  private readonly sse: { stream: string; send: string } | null;

  /** `path` is the same `/ws/...` path the WebSocket transport would use. */
  constructor(path: string) {
    this.token = new URLSearchParams(location.search).get("token");
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    const sep = path.includes("?") ? "&" : "?";
    const prefixed = withBasePath(path);
    this.wsUrl = this.token
      ? `${proto}//${location.host}${prefixed}${sep}token=${encodeURIComponent(this.token)}`
      : `${proto}//${location.host}${prefixed}`;
    this.sse = sseEndpoints(path);

    this.connectWs();
  }

  // ── Real WebSocket attempt ────────────────────────────────────────────

  private connectWs() {
    let ws: WebSocket;
    try {
      ws = new WebSocket(this.wsUrl);
    } catch {
      this.fallbackToSse();
      return;
    }
    this.ws = ws;

    // If the upgrade hasn't completed within the window, assume the edge
    // dropped it (AVA) and switch to SSE.
    this.openTimer = setTimeout(() => {
      if (!this.opened && !this.closedByCaller) {
        try {
          ws.onopen = null;
          ws.onmessage = null;
          ws.onclose = null;
          ws.onerror = null;
          ws.close();
        } catch {
          /* ignore */
        }
        this.ws = null;
        this.fallbackToSse();
      }
    }, WS_OPEN_TIMEOUT_MS);

    ws.onopen = (ev) => {
      this.opened = true;
      this.clearOpenTimer();
      this.readyState = this.OPEN;
      this.transport = "ws";
      this.onopen?.(ev);
    };

    ws.onmessage = (ev) => {
      this.onmessage?.(ev);
    };

    ws.onerror = (ev) => {
      // A pre-open error is the AVA signature: fall back rather than surface
      // a dead connection. A post-open error closes normally (handled below).
      if (!this.opened && !this.closedByCaller) {
        this.clearOpenTimer();
        try {
          ws.close();
        } catch {
          /* ignore */
        }
        this.ws = null;
        this.fallbackToSse();
        return;
      }
      this.onerror?.(ev);
    };

    ws.onclose = (ev) => {
      if (this.ws !== ws) return; // superseded by fallback
      this.ws = null;
      this.clearOpenTimer();
      if (this.opened) {
        // Normal close after a working session — propagate so callers reconnect.
        this.readyState = this.CLOSED;
        this.onclose?.(ev);
      } else if (!this.closedByCaller) {
        // Closed before ever opening → treat as upgrade failure, fall back.
        this.fallbackToSse();
      }
    };
  }

  private clearOpenTimer() {
    if (this.openTimer !== null) {
      clearTimeout(this.openTimer);
      this.openTimer = null;
    }
  }

  // ── SSE fallback ──────────────────────────────────────────────────────

  private fallbackToSse() {
    if (this.closedByCaller) return;
    if (!this.sse) {
      // No SSE mapping for this path — report a normal close.
      this.readyState = this.CLOSED;
      this.onclose?.(new Event("close"));
      return;
    }
    this.transport = "sse";

    const q = new URLSearchParams();
    q.set("conn", this.connId);
    if (this.token) q.set("token", this.token);
    // Merge any query already on the stream path (e.g. ?subscribe=…).
    const [streamPath, streamQuery] = this.sse.stream.split("?");
    if (streamQuery) {
      for (const [k, v] of new URLSearchParams(streamQuery)) q.set(k, v);
    }
    const streamUrl = `${location.origin}${withBasePath(streamPath)}?${q.toString()}`;

    const sendQ = new URLSearchParams();
    sendQ.set("conn", this.connId);
    if (this.token) sendQ.set("token", this.token);
    this.sendUrl = `${location.origin}${withBasePath(this.sse.send)}?${sendQ.toString()}`;

    let es: EventSource;
    try {
      es = new EventSource(streamUrl, { withCredentials: true });
    } catch {
      this.readyState = this.CLOSED;
      this.onclose?.(new Event("close"));
      return;
    }
    this.es = es;

    es.onopen = () => {
      this.opened = true;
      this.readyState = this.OPEN;
      this.onopen?.(new Event("open"));
    };

    es.onmessage = (ev) => {
      // Drop server liveness pings; they exist only to detect dead streams.
      if (ev.data === '{"event":"ping"}') return;
      this.onmessage?.(ev);
    };

    es.onerror = (ev) => {
      // EventSource auto-reconnects on transient errors; only act on a hard
      // close (readyState CLOSED) so we don't tear down a recoverable stream.
      if (es.readyState === EventSource.CLOSED && !this.closedByCaller) {
        this.es = null;
        this.readyState = this.CLOSED;
        this.onclose?.(ev);
      }
    };
  }

  // ── WebSocket-compatible surface ──────────────────────────────────────

  send(data: string) {
    if (this.transport === "ws") {
      if (this.ws && this.ws.readyState === WebSocket.OPEN) {
        this.ws.send(data);
      }
      return;
    }
    // SSE: control messages go out-of-band via POST. Fire-and-forget; the
    // server routes the body to this connection's inbound feed by `conn`.
    if (this.readyState !== this.OPEN) return;
    void fetch(this.sendUrl, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        ...(this.token ? { Authorization: `Bearer ${this.token}` } : {}),
      },
      body: data,
      keepalive: true,
    }).catch(() => {
      /* transient POST failures are non-fatal; the stream is the source of truth */
    });
  }

  close() {
    this.closedByCaller = true;
    this.readyState = this.CLOSED;
    this.clearOpenTimer();
    if (this.ws) {
      try {
        this.ws.close();
      } catch {
        /* ignore */
      }
      this.ws = null;
    }
    if (this.es) {
      try {
        this.es.close();
      } catch {
        /* ignore */
      }
      this.es = null;
    }
  }
}
