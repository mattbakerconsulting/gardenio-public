import { afterEach, describe, expect, it, vi } from "vitest";
import { handleReplayReady, inputSendFrame } from "./ExpandedSession";

describe("inputSendFrame", () => {
  // Regression for ka-0000006818: the direct per-session WS used to send normal
  // keystrokes as `input:send:raw`, which the edge now reserves for admin-tier
  // raw byte injection. An edge-authed operator therefore got plain typing
  // refused. Keystrokes must be operator-tier `input:send` on BOTH transports.
  it("uses operator-tier input:send, never admin-reserved input:send:raw", () => {
    const frame = inputSendFrame("sess-1", "ls\r");
    expect(frame.event).toBe("input:send");
    expect(frame.event).not.toBe("input:send:raw");
  });

  it("carries the raw text unmodified (no base64, no mangling)", () => {
    // Control chars and escape sequences must pass through verbatim — the
    // upstream writes `text` bytes directly to the PTY, byte-identical to the
    // old raw path's base64-decoded payload.
    const frame = inputSendFrame("sess-2", "\x1b[A\x03");
    expect(frame.text).toBe("\x1b[A\x03");
  });

  it("includes the session id required by the multiplexed feed's router", () => {
    expect(inputSendFrame("sess-3", "x").session).toBe("sess-3");
  });
});

describe("handleReplayReady", () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("enables stdin and sets ready synchronously", () => {
    vi.stubGlobal("requestAnimationFrame", vi.fn());

    const term = { options: { disableStdin: true }, focus: vi.fn() };
    const setReady = vi.fn();

    handleReplayReady(term, setReady);

    expect(term.options.disableStdin).toBe(false);
    expect(setReady).toHaveBeenCalledWith(true);
  });

  it("defers focus to next animation frame (after React render)", () => {
    const rafCallbacks: (() => void)[] = [];
    vi.stubGlobal("requestAnimationFrame", (cb: () => void) => {
      rafCallbacks.push(cb);
      return 0;
    });

    const term = { options: { disableStdin: true }, focus: vi.fn() };
    const setReady = vi.fn();

    handleReplayReady(term, setReady);

    // Focus must NOT be called synchronously — the terminal is still
    // invisible at this point because React hasn't re-rendered yet.
    expect(term.focus).not.toHaveBeenCalled();
    expect(rafCallbacks).toHaveLength(1);

    // Simulate the browser firing the animation frame (after React paint)
    for (const cb of rafCallbacks) cb();

    expect(term.focus).toHaveBeenCalledOnce();
  });

  it("calls setReady before scheduling focus", () => {
    const order: string[] = [];
    vi.stubGlobal("requestAnimationFrame", (cb: () => void) => {
      // rAF is scheduled, record that and run it
      order.push("raf-scheduled");
      cb();
      return 0;
    });

    const term = { options: { disableStdin: true }, focus: vi.fn(() => order.push("focus")) };
    const setReady = vi.fn(() => order.push("setReady"));

    handleReplayReady(term, setReady);

    // setReady must come before focus is even scheduled
    expect(order).toEqual(["setReady", "raf-scheduled", "focus"]);
  });
});
