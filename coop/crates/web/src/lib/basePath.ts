// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

// When the server is deployed behind a strip-prefix proxy (e.g. Traefik
// `stripPrefix: /mux` on a unified gardenio host), the server injects
// `window.__COOP_BASE_PATH__` so the SPA can prefix its outgoing fetch /
// WebSocket URLs to match the public URL the browser sees.
//
// Empty string means "no prefix" — the SPA can construct URLs as before.

declare global {
  interface Window {
    __COOP_BASE_PATH__?: string;
  }
}

export function getBasePath(): string {
  return window.__COOP_BASE_PATH__ ?? "";
}

/**
 * Prefix `path` with the configured base path. `path` should start with
 * `/`. Returns `path` unchanged when no base path is configured.
 */
export function withBasePath(path: string): string {
  const bp = getBasePath();
  if (!bp) return path;
  // Avoid double-prefixing if the caller already included it.
  if (path === bp || path.startsWith(`${bp}/`)) return path;
  return `${bp}${path}`;
}
