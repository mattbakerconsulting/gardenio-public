import { useEffect, useRef, useState } from "react";
import { apiGet } from "@/hooks/useApiClient";
import { FallbackSocket } from "@/lib/fallback-socket";
import type { ApiResult } from "@/lib/types";

export type ConnectionStatus = "connecting" | "connected" | "disconnected";

/**
 * Minimal WebSocket-like surface shared by the native `WebSocket` and the
 * `FallbackSocket` (WS→SSE) transport. `WsRpc` and the mux call sites only
 * need this slice, so accepting it lets the SSE fallback drop in unchanged.
 */
export interface SocketLike {
  readyState: number;
  send(data: string): void;
  close(): void;
}

interface UseWebSocketOptions {
  /** Full path (e.g. "/ws?subscribe=pty,state") — token appended automatically */
  path: string;
  /** Called for each parsed JSON message */
  onMessage: (msg: unknown) => void;
  /** Reconnect delay in ms (default 2000) */
  reconnectDelay?: number;
  /** Whether the connection is enabled (default true) */
  enabled?: boolean;
}

/** Send a WS message and await the correlated response via `request_id`. */
export type WsRequest = (msg: Record<string, unknown>) => Promise<ApiResult>;

let nextId = 1;

/**
 * Standalone request-response helper for a raw WebSocket.
 * Used by mux expanded view where we manage the WS manually.
 */
export class WsRpc {
  private queue: Array<{
    resolve: ((r: ApiResult) => void) | null;
    timer: ReturnType<typeof setTimeout>;
  }> = [];

  constructor(
    private ws: SocketLike,
    private timeout = 10_000,
  ) {}

  /** Handle an incoming message. Returns true if it was a response (had request_id). */
  handleMessage(msg: Record<string, unknown>): boolean {
    if (!("request_id" in msg)) return false;
    // Pop entries, skipping any that already timed out (resolve nulled).
    while (this.queue.length > 0) {
      const entry = this.queue.shift()!;
      clearTimeout(entry.timer);
      if (entry.resolve) {
        const isError = msg.event === "error";
        entry.resolve({
          ok: !isError,
          status: isError ? 400 : 200,
          json: msg,
          text: JSON.stringify(msg),
        });
        return true;
      }
      // entry.resolve was nulled by timeout — this response is late, discard it.
    }
    return false;
  }

  request(msg: Record<string, unknown>): Promise<ApiResult> {
    return new Promise((resolve) => {
      if (this.ws.readyState !== WebSocket.OPEN) {
        resolve({ ok: false, status: 0, json: null, text: "WebSocket not open" });
        return;
      }
      const entry: (typeof this.queue)[number] = { resolve, timer: 0 as never };
      entry.timer = setTimeout(() => {
        entry.resolve = null; // mark expired, leave in queue as placeholder
        resolve({ ok: false, status: 408, json: null, text: "Request timeout" });
      }, this.timeout);
      this.queue.push(entry);
      this.ws.send(JSON.stringify({ ...msg, request_id: `r${nextId++}` }));
    });
  }

  dispose() {
    for (const entry of this.queue) {
      clearTimeout(entry.timer);
      entry.resolve?.({ ok: false, status: 0, json: null, text: "Disposed" });
    }
    this.queue = [];
  }
}

export function useWebSocket({
  path,
  onMessage,
  reconnectDelay = 2000,
  enabled = true,
}: UseWebSocketOptions) {
  const wsRef = useRef<SocketLike | null>(null);
  const rpcRef = useRef<WsRpc | null>(null);
  const [status, setStatus] = useState<ConnectionStatus>("disconnected");
  const onMessageRef = useRef(onMessage);
  onMessageRef.current = onMessage;

  function send(msg: unknown) {
    const ws = wsRef.current;
    if (ws && ws.readyState === FallbackSocket.OPEN) {
      ws.send(JSON.stringify(msg));
    }
  }

  const request: WsRequest = (msg: Record<string, unknown>) => {
    const rpc = rpcRef.current;
    if (!rpc) {
      return Promise.resolve({
        ok: false,
        status: 0,
        json: null,
        text: "Not connected",
      } as ApiResult);
    }
    return rpc.request(msg);
  };

  useEffect(() => {
    if (!enabled) return;

    let cancelled = false;
    let reconnectTimer: ReturnType<typeof setTimeout>;

    function connect() {
      if (cancelled) return;
      setStatus("connecting");

      // FallbackSocket attempts a real WebSocket first, then transparently
      // switches to SSE+POST if the upgrade is dropped (e.g. behind AWS
      // Verified Access). It derives the /sse/* endpoints from `path`.
      const ws = new FallbackSocket(path);
      wsRef.current = ws;
      const rpc = new WsRpc(ws);
      rpcRef.current = rpc;

      ws.onopen = () => {
        if (cancelled) {
          ws.close();
          return;
        }
        setStatus("connected");
      };

      ws.onmessage = (ev) => {
        try {
          const parsed = JSON.parse(ev.data);
          // If this is a response to a pending request, don't forward to onMessage
          if (!rpc.handleMessage(parsed)) {
            onMessageRef.current(parsed);
          }
        } catch {
          // ignore parse errors
        }
      };

      ws.onclose = () => {
        wsRef.current = null;
        rpc.dispose();
        rpcRef.current = null;
        if (!cancelled) {
          setStatus("disconnected");
          // A WS upgrade that fails with 401 (expired ForwardAuth cookie)
          // surfaces here as a generic close — the browser hides the HTTP
          // status. Probe an authed endpoint; a 401 there triggers the
          // reauth-reload in useApiClient, which resolves the stranded UI.
          apiGet("/api/v1/version").catch(() => {
            /* network errors are fine; only 401 matters and that's handled */
          });
          reconnectTimer = setTimeout(connect, reconnectDelay);
        }
      };

      ws.onerror = () => {
        ws.close();
      };
    }

    connect();

    return () => {
      cancelled = true;
      clearTimeout(reconnectTimer);
      rpcRef.current?.dispose();
      rpcRef.current = null;
      wsRef.current?.close();
      wsRef.current = null;
    };
  }, [path, reconnectDelay, enabled]);

  return { send, request, status, wsRef };
}
