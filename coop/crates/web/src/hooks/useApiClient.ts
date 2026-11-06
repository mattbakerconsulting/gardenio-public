import { useRef } from "react";
import { withBasePath } from "@/lib/basePath";
import type { ApiResult } from "@/lib/types";

function getToken(): string | null {
  return new URLSearchParams(window.location.search).get("token");
}

function authHeaders(): Record<string, string> {
  const token = getToken();
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  if (token) headers["Authorization"] = `Bearer ${token}`;
  return headers;
}

// Debounce so concurrent 401s from parallel calls only reload once.
let reauthTriggered = false;

/**
 * When oauth2-proxy's session cookie is expired and we're behind
 * Traefik ForwardAuth, an authed XHR returns 401. A full page load,
 * however, goes through ForwardAuth and gets redirected to Keycloak
 * login. Force a reload so the user re-authenticates instead of
 * being stranded on a dysfunctional UI.
 */
function triggerReauth(): void {
  if (reauthTriggered) return;
  reauthTriggered = true;
  // Bypass the browser cache so the reload hits ForwardAuth fresh.
  window.location.reload();
}

async function request(method: string, path: string, body?: unknown): Promise<ApiResult> {
  const res = await fetch(`${location.origin}${withBasePath(path)}`, {
    method,
    headers: authHeaders(),
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  if (res.status === 401 && !getToken()) {
    // Static-token auth failures shouldn't reload (no ForwardAuth in play).
    // Cookie-auth 401 means the session expired — reload to re-auth.
    triggerReauth();
  }
  const text = await res.text();
  let json: unknown;
  try {
    json = JSON.parse(text);
  } catch {
    json = null;
  }
  return { ok: res.ok, status: res.status, json, text };
}

export function useApiClient() {
  // Stable refs so callbacks never change identity
  const apiGet = useRef((path: string) => request("GET", path)).current;
  const apiPost = useRef((path: string, body?: unknown) => request("POST", path, body)).current;
  const apiPut = useRef((path: string, body?: unknown) => request("PUT", path, body)).current;
  const apiDelete = useRef((path: string) => request("DELETE", path)).current;

  return { apiGet, apiPost, apiPut, apiDelete };
}

// Non-hook versions for use outside React components
export const apiGet = (path: string) => request("GET", path);
export const apiPost = (path: string, body?: unknown) => request("POST", path, body);
export const apiPut = (path: string, body?: unknown) => request("PUT", path, body);
export const apiDelete = (path: string) => request("DELETE", path);
