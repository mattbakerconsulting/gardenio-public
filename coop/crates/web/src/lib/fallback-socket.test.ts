/**
 * SPDX-License-Identifier: BUSL-1.1
 * Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>
 */

import { describe, expect, it } from "vitest";
import { sseEndpoints } from "./fallback-socket";

describe("sseEndpoints", () => {
  it("maps the mux aggregation feed", () => {
    expect(sseEndpoints("/ws/mux")).toEqual({
      stream: "/sse/mux",
      send: "/sse/mux/send",
    });
  });

  it("maps a per-session path, preserving the subscribe query on the stream", () => {
    expect(sseEndpoints("/ws/pod-abc?subscribe=pty,state,usage,hooks")).toEqual({
      stream: "/sse/session/pod-abc?subscribe=pty,state,usage,hooks",
      send: "/sse/session/pod-abc/send",
    });
  });

  it("maps a per-session path without a query", () => {
    expect(sseEndpoints("/ws/pod-abc")).toEqual({
      stream: "/sse/session/pod-abc",
      send: "/sse/session/pod-abc/send",
    });
  });

  it("returns null for unmapped paths", () => {
    expect(sseEndpoints("/ws")).toBeNull();
    expect(sseEndpoints("/something/else")).toBeNull();
  });
});
