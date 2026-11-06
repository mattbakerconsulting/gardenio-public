/**
 * SPDX-License-Identifier: BUSL-1.1
 * Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>
 */

import { describe, expect, it } from "vitest";
import { ReplayGate } from "./replay-gate";

// ===== Unit tests ============================================================

describe("ReplayGate", () => {
  it("pty_before_replay_dropped", () => {
    const gate = new ReplayGate();
    expect(gate.onPty(10, 0)).toBeNull();
  });

  it("first_replay_accepts_all", () => {
    const gate = new ReplayGate();
    const action = gate.onReplay(100, 0, 100);
    expect(action).not.toBeNull();
    expect(action!.skip).toBe(0);
    expect(action!.isFirst).toBe(true);
    expect(gate.offset()).toBe(100);
  });

  it("pty_after_replay_no_overlap", () => {
    const gate = new ReplayGate();
    gate.onReplay(100, 0, 100);
    const skip = gate.onPty(20, 100);
    expect(skip).toBe(0);
    expect(gate.offset()).toBe(120);
  });

  it("pty_fully_covered_by_replay", () => {
    const gate = new ReplayGate();
    gate.onReplay(100, 0, 100);
    expect(gate.onPty(30, 50)).toBeNull();
  });

  it("pty_partial_overlap", () => {
    const gate = new ReplayGate();
    gate.onReplay(100, 0, 100);
    const skip = gate.onPty(20, 90);
    expect(skip).toBe(10);
    expect(gate.offset()).toBe(110);
  });

  it("pty_with_gap", () => {
    const gate = new ReplayGate();
    gate.onReplay(100, 0, 100);
    const skip = gate.onPty(10, 120);
    expect(skip).toBe(0);
    expect(gate.offset()).toBe(130);
  });

  it("second_replay_dedup", () => {
    const gate = new ReplayGate();
    gate.onReplay(100, 0, 100);
    const action = gate.onReplay(150, 0, 150);
    expect(action).not.toBeNull();
    expect(action!.skip).toBe(100);
    expect(action!.isFirst).toBe(false);
    expect(gate.offset()).toBe(150);
  });

  it("second_replay_no_new_data", () => {
    const gate = new ReplayGate();
    gate.onReplay(100, 0, 100);
    expect(gate.onReplay(80, 0, 80)).toBeNull();
  });

  it("reset_returns_to_pending", () => {
    const gate = new ReplayGate();
    gate.onReplay(100, 0, 100);
    gate.reset();
    expect(gate.offset()).toBe(-1);
    expect(gate.onPty(10, 100)).toBeNull();
    const action = gate.onReplay(50, 0, 50);
    expect(action).not.toBeNull();
    expect(action!.isFirst).toBe(true);
  });

  it("empty_replay_still_syncs", () => {
    const gate = new ReplayGate();
    const action = gate.onReplay(0, 0, 0);
    expect(action).not.toBeNull();
    expect(action!.skip).toBe(0);
    expect(action!.isFirst).toBe(true);
    expect(gate.offset()).toBe(0);
  });

  it("sequential_pty_stream", () => {
    const gate = new ReplayGate();
    gate.onReplay(0, 0, 0);
    for (let i = 0; i < 5; i++) {
      const skip = gate.onPty(10, i * 10);
      expect(skip).toBe(0);
      expect(gate.offset()).toBe((i + 1) * 10);
    }
  });

  it("replay_after_pty_stream", () => {
    const gate = new ReplayGate();
    gate.onReplay(10, 0, 10);
    gate.onPty(10, 10); // gate = 20
    gate.onPty(10, 20); // gate = 30
    const action = gate.onReplay(50, 0, 50);
    expect(action).not.toBeNull();
    expect(action!.skip).toBe(30);
    expect(action!.isFirst).toBe(false);
    expect(gate.offset()).toBe(50);
  });
});

// ===== RenderHarness =========================================================

class RenderHarness {
  gate = new ReplayGate();
  output: number[] = [];
  resets = 0;

  replay(data: string, offset: number, nextOffset: number): void {
    const bytes = new TextEncoder().encode(data);
    const action = this.gate.onReplay(bytes.length, offset, nextOffset);
    if (action) {
      if (action.isFirst) {
        this.output = [];
        this.resets++;
      }
      for (let i = action.skip; i < bytes.length; i++) this.output.push(bytes[i]);
    }
  }

  pty(data: string, offset: number): void {
    const bytes = new TextEncoder().encode(data);
    const skip = this.gate.onPty(bytes.length, offset);
    if (skip !== null) {
      for (let i = skip; i < bytes.length; i++) this.output.push(bytes[i]);
    }
  }

  reconnect(): void {
    this.gate.reset();
    this.output = [];
  }

  outputStr(): string {
    return new TextDecoder().decode(new Uint8Array(this.output));
  }
}

// ===== RenderHarness scenario tests ==========================================

describe("RenderHarness", () => {
  it("race_pty_before_replay", () => {
    const h = new RenderHarness();
    h.pty("AB", 0);
    h.replay("ABCD", 0, 4);
    expect(h.outputStr()).toBe("ABCD");
    expect(h.resets).toBe(1);
  });

  it("race_pty_overlapping_replay", () => {
    const h = new RenderHarness();
    h.pty("AB", 0);
    h.pty("CD", 2);
    h.replay("ABCDEF", 0, 6);
    expect(h.outputStr()).toBe("ABCDEF");
  });

  it("clean_connect", () => {
    const h = new RenderHarness();
    h.replay("HELLO", 0, 5);
    h.pty("!", 5);
    expect(h.outputStr()).toBe("HELLO!");
  });

  it("lag_recovery", () => {
    const h = new RenderHarness();
    h.replay("AB", 0, 2);
    h.pty("CD", 2);
    h.pty("EF", 4);
    h.replay("ABCDEF", 0, 6);
    expect(h.outputStr()).toBe("ABCDEF");
  });

  it("reconnect_full_replay", () => {
    const h = new RenderHarness();
    h.replay("OLD", 0, 3);
    h.reconnect();
    h.replay("NEW", 0, 3);
    expect(h.outputStr()).toBe("NEW");
    expect(h.resets).toBe(2);
  });

  it("resize_refresh", () => {
    const h = new RenderHarness();
    h.replay("AB", 0, 2);
    h.pty("CD", 2);
    h.reconnect();
    h.replay("ABCD", 0, 4);
    expect(h.outputStr()).toBe("ABCD");
    expect(h.resets).toBe(2);
  });

  it("interleaved_stream", () => {
    const h = new RenderHarness();
    h.replay("A", 0, 1);
    h.pty("B", 1);
    h.pty("C", 2);
    h.pty("BC", 1); // late duplicate — fully covered
    expect(h.outputStr()).toBe("ABC");
  });
});
