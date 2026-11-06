/**
 * SPDX-License-Identifier: BUSL-1.1
 * Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>
 */

import { describe, expect, it } from "vitest";
import { WsMessageHarness } from "./ws-harness";

// ===== Category A: Reproduce racy server offsets ============================

describe("Category A: racy server offsets", () => {
  it("inflated_pty_causes_gap", () => {
    const h = new WsMessageHarness();
    h.replay("ABCDE", 0, 5);
    // Server race: offset is 7 instead of 5 (2 bytes inflated)
    h.pty("FG", 7);
    expect(h.gaps()).toEqual([{ from: 5, to: 7 }]);
    expect(h.outputStr()).toBe("ABCDEFG");
  });

  it("two_concurrent_inflated", () => {
    const h = new WsMessageHarness();
    h.replay("AB", 0, 2);
    // Both offsets inflated by +2
    h.pty("CD", 4);
    h.pty("EF", 6);
    expect(h.gaps()).toEqual([{ from: 2, to: 4 }]);
    // Note: [4,6) and [6,8) are contiguous, so only [2,4) is a gap
    expect(h.outputStr()).toBe("ABCDEF");
  });

  it("burst_with_some_inflated", () => {
    const h = new WsMessageHarness();
    h.replay("", 0, 0); // empty replay syncs gate to 0
    // 10 single-byte PTYs, some offsets skipped
    const offsets = [0, 1, 2, 5, 6, 7, 10, 11, 12, 13];
    const letters = "ABCDEFGHIJ";
    for (let i = 0; i < offsets.length; i++) {
      h.pty(letters[i], offsets[i]);
    }
    expect(h.gaps()).toEqual([
      { from: 3, to: 5 },
      { from: 8, to: 10 },
    ]);
  });
});

// ===== Category B: Expand session flow ======================================

describe("Category B: expand session flow", () => {
  it("expand_clean", () => {
    const h = new WsMessageHarness();
    h.reconnect();
    h.replay("Hello, World! ok!", 0, 17);
    h.pty("0123456789", 17);
    expect(h.gaps()).toEqual([]);
    expect(h.overlaps()).toEqual([]);
    expect(h.resets).toBe(1);
    expect(h.outputStr()).toBe("Hello, World! ok!0123456789");
  });

  it("expand_pty_before_replay", () => {
    const h = new WsMessageHarness();
    h.reconnect();
    // PTY arrives before replay — should be dropped (pre-replay)
    h.pty("0123456789", 10);
    h.replay("ABCDEFGHIJKLMNOP", 0, 16);
    h.pty("QR", 16);
    expect(h.gaps()).toEqual([]);
    expect(h.overlaps()).toEqual([]);
    expect(h.outputStr()).toBe("ABCDEFGHIJKLMNOPQR");
  });

  it("expand_reconnect", () => {
    const h = new WsMessageHarness();
    h.replay("ABC", 0, 3);
    h.pty("D", 3);
    h.reconnect();
    h.replay("ABCDEFG", 0, 7);
    expect(h.gaps()).toEqual([]);
    expect(h.overlaps()).toEqual([]);
    expect(h.resets).toBe(2);
    expect(h.outputStr()).toBe("ABCDEFG");
  });

  it("expand_resize", () => {
    const h = new WsMessageHarness();
    h.replay("ABCDE", 0, 5);
    h.pty("FG", 5);
    h.reconnect();
    h.replay("ABCDEFG", 0, 7);
    h.pty("HI", 7);
    expect(h.gaps()).toEqual([]);
    expect(h.overlaps()).toEqual([]);
    expect(h.resets).toBe(2);
    expect(h.outputStr()).toBe("ABCDEFGHI");
  });
});

// ===== Category C: Edge cases ===============================================

describe("Category C: edge cases", () => {
  it("out_of_order_pty", () => {
    const h = new WsMessageHarness();
    h.replay("AB", 0, 2);
    h.pty("EF", 4); // gap: [2,4)
    h.pty("CD", 2); // late — gate already at 6, fully covered
    expect(h.gaps()).toEqual([{ from: 2, to: 4 }]);
    // Late PTY is dropped
    const latePty = h.writes.find((w) => w.kind === "pty" && w.offset === 2);
    expect(latePty?.dropped).toBe(true);
  });

  it("duplicate_replay", () => {
    const h = new WsMessageHarness();
    h.replay("ABCD", 0, 4);
    h.replay("ABCD", 0, 4); // duplicate — should be dropped
    h.pty("EF", 4);
    expect(h.gaps()).toEqual([]);
    expect(h.overlaps()).toEqual([]);
    const secondReplay = h.writes[1];
    expect(secondReplay.dropped).toBe(true);
    expect(h.outputStr()).toBe("ABCDEF");
  });

  it("empty_session", () => {
    const h = new WsMessageHarness();
    h.replay("", 0, 0);
    h.pty("0123456789", 0);
    expect(h.gaps()).toEqual([]);
    expect(h.overlaps()).toEqual([]);
    expect(h.outputStr()).toBe("0123456789");
  });

  it("large_replay_then_ptys", () => {
    const h = new WsMessageHarness();
    const bigData = "X".repeat(10240);
    h.replay(bigData, 0, 10240);
    for (let i = 0; i < 100; i++) {
      h.pty("Y".repeat(100), 10240 + i * 100);
    }
    expect(h.gaps()).toEqual([]);
    expect(h.overlaps()).toEqual([]);
    expect(h.output().length).toBe(10240 + 10000);
  });
});

// ===== Category D: Reconnect stale-gate race =================================

describe("Category D: reconnect stale-gate race", () => {
  it("reconnect_stale_gate_pty_leak", () => {
    const h = new WsMessageHarness();
    // Initial session: replay + PTY advance gate to 100
    h.replay("X".repeat(50), 0, 50);
    h.pty("Y".repeat(50), 50);
    // Gate is now at 100

    // Simulate reconnect WITHOUT calling h.reconnect() first
    // (mirrors the App.tsx race where WS reconnects but useEffect
    //  hasn't fired gateRef.current.reset() yet)
    h.pty("A".repeat(10), 100); // gate accepts (100 >= 100)
    h.pty("B".repeat(10), 110); // gate accepts (110 >= 110)

    // Verify leaks: stale gate accepted these PTY events
    const leak1 = h.writes.find((w) => w.kind === "pty" && w.offset === 100);
    const leak2 = h.writes.find((w) => w.kind === "pty" && w.offset === 110);
    expect(leak1?.dropped).toBe(false);
    expect(leak2?.dropped).toBe(false);

    // NOW the reset fires (delayed useEffect in App.tsx)
    h.reconnect();

    // Replay covers everything from 0
    h.replay("Z".repeat(120), 0, 120);

    // Gate-level view is clean after reset — the corruption happens
    // downstream in xterm's write buffer where the leaked PTY writes
    // are still queued when term.reset() fires
    expect(h.gaps()).toEqual([]);

    // The writes log proves the leak: PTY data at [100,120) was
    // written once via stale-gate PTY, and will be written again
    // by the post-reconnect replay
    const leakedPtyWrites = h.writes.filter(
      (w) => !w.dropped && w.kind === "pty" && w.offset >= 100,
    );
    expect(leakedPtyWrites).toHaveLength(2);
  });

  it("reconnect_proper_reset_no_leak", () => {
    const h = new WsMessageHarness();
    h.replay("X".repeat(50), 0, 50);
    h.pty("Y".repeat(50), 50);

    // ExpandedSession pattern: reset BEFORE creating WS
    h.reconnect();

    // PTY before replay → dropped (gate pending, nextOffset = -1)
    h.pty("A".repeat(10), 100);

    // Replay covers full range
    h.replay("Z".repeat(120), 0, 120);

    const droppedPty = h.writes.find((w) => w.kind === "pty" && w.offset === 100);
    expect(droppedPty?.dropped).toBe(true);
    expect(h.overlaps()).toEqual([]);
    expect(h.gaps()).toEqual([]);
  });

  it("lag_recovery_replay_during_stream", () => {
    const h = new WsMessageHarness();
    h.replay("", 0, 0);

    // Normal PTY stream
    for (let i = 0; i < 50; i++) h.pty("x", i);

    // Server sends lag-recovery Replay covering [50, 200)
    h.replay("y".repeat(150), 50, 200);

    // More PTY events after the recovery
    for (let i = 200; i < 210; i++) h.pty("z", i);

    expect(h.gaps()).toEqual([]);
    expect(h.overlaps()).toEqual([]);
  });
});

// ===== Category E: Write order simulation ====================================

describe("Category E: write order simulation", () => {
  it("stale_pty_before_replay_reset_write_order", () => {
    const h = new WsMessageHarness();

    // Session 1
    h.replay("ABCDE", 0, 5);
    h.pty("FG", 5);
    // gate at 7

    // Reconnect race: PTY leaks before gate.reset()
    h.pty("HI", 7); // accepted by stale gate

    // Verify: "HI" was not dropped (leaked through stale gate)
    const hiWrite = h.writes.find((w) => w.kind === "pty" && w.offset === 7);
    expect(hiWrite?.dropped).toBe(false);

    // After reset + replay, the range [7,9) was written twice:
    // once from stale PTY, once from the replay below
    h.reconnect();
    h.replay("ABCDEFGHI", 0, 9);

    // The stale "HI" write went to xterm before term.reset().
    // xterm's write buffer may still process it after the reset,
    // causing garbled output. Verify the write log shows the leak.
    const ptyWritesAt7 = h.writes.filter((w) => !w.dropped && w.kind === "pty" && w.offset === 7);
    expect(ptyWritesAt7).toHaveLength(1);

    // And the replay also covers [7,9)
    const replayAfterReset = h.writes.find(
      (w) => !w.dropped && w.kind === "replay" && w.isFirst && w.written === 9,
    );
    expect(replayAfterReset).toBeDefined();
  });
});

// ===== Category F: diagnose() ================================================

describe("Category F: diagnose()", () => {
  it("healthy session returns empty array", () => {
    const h = new WsMessageHarness();
    h.replay("ABCDE", 0, 5);
    h.pty("FG", 5);
    h.pty("HI", 7);
    expect(h.diagnose()).toEqual([]);
  });

  it("session with gaps returns gaps issue", () => {
    const h = new WsMessageHarness();
    h.replay("ABCDE", 0, 5);
    h.pty("FG", 7); // gap at [5,7)
    const issues = h.diagnose();
    expect(issues).toHaveLength(1);
    expect(issues[0].kind).toBe("gaps");
    expect(issues[0].count).toBe(1);
    expect(issues[0].message).toContain("2 bytes missing");
  });

  it("session with overlaps returns overlaps issue", () => {
    const h = new WsMessageHarness();
    h.replay("ABCDE", 0, 5);
    h.pty("FG", 5);
    // Force an overlap by pushing a duplicate range via internals
    (h as unknown as Record<string, { from: number; to: number }[]>).ranges.push({
      from: 5,
      to: 7,
    });
    const issues = h.diagnose();
    const overlap = issues.find((i) => i.kind === "overlaps");
    expect(overlap).toBeDefined();
    expect(overlap!.count).toBeGreaterThan(0);
  });

  it("session with stale leak returns stale_leak issue", () => {
    const h = new WsMessageHarness();
    // Initial session — replay only, no PTY continuation
    h.replay("X".repeat(100), 0, 100);

    // Stale-gate leak: PTY accepted by old gate before reconnect reset
    h.pty("A".repeat(10), 100);

    // Now reconnect and replay covers [0, 120)
    h.reconnect();
    h.replay("Z".repeat(120), 0, 120);

    const issues = h.diagnose();
    const leak = issues.find((i) => i.kind === "stale_leak");
    expect(leak).toBeDefined();
    expect(leak!.count).toBe(1);
    expect(leak!.message).toContain("leaked through stale gate");
  });
});
