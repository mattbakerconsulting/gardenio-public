#!/usr/bin/env python3
"""decode-pty-trace.py — pretty-print a v1 PTY trace file.

The v1 trace format is produced both by coop's NativePty backend
(gardenio/coop/crates/cli/src/backend/spawn.rs, env COOP_PTY_TRACE_FILE)
and by the isolated repro test-claudeless-multi-nudge-isolated.sh
(same env var). Both write:

    # coop pty trace v1 pid=<n> ts_micros=<unix_us>
    <micros>\t<W|R>\t<hex>

This decoder renders each record as:

    [+0.123456s] W  4 bytes  | 70 69 6e 67 0a               | "ping\\n"

Useful flags:
  --raw       skip ASCII rendering (hex only)
  --only=W    only show writes (or R for reads)
  --max=N     truncate hex/text to N bytes per record (default 64)

Diagnostic facility for ka-0000002034. Diffing the W column of a passing
isolated repro against a failing E2E shard pinpoints any byte-stream
divergence in coop's PTY delivery path.
"""
import argparse
import sys


def decode_ascii(b: bytes) -> str:
    out = []
    for byte in b:
        if byte == 0x1b:
            out.append("\\e")
        elif byte == 0x0d:
            out.append("\\r")
        elif byte == 0x0a:
            out.append("\\n")
        elif byte == 0x09:
            out.append("\\t")
        elif byte == 0x15:
            out.append("^U")
        elif byte == 0x04:
            out.append("^D")
        elif 0x20 <= byte < 0x7f:
            out.append(chr(byte))
        else:
            out.append(f"\\x{byte:02x}")
    return "".join(out)


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("path", help="trace file path")
    ap.add_argument("--raw", action="store_true", help="hex only, no ASCII column")
    ap.add_argument("--only", choices=["W", "R"], help="filter direction")
    ap.add_argument("--max", type=int, default=64, help="max bytes per record (default 64)")
    args = ap.parse_args()

    base_us = None
    with open(args.path, "r", encoding="utf-8", errors="replace") as fh:
        for line in fh:
            line = line.rstrip("\n")
            if not line:
                continue
            if line.startswith("#"):
                print(line)
                continue
            cols = line.split("\t", 2)
            if len(cols) < 3:
                continue
            us_str, direction, hex_str = cols
            try:
                us = int(us_str)
            except ValueError:
                continue
            if args.only and direction != args.only:
                continue
            if base_us is None:
                base_us = us
            delta = (us - base_us) / 1_000_000.0
            try:
                data = bytes.fromhex(hex_str)
            except ValueError:
                continue
            shown = data[: args.max]
            hex_pretty = " ".join(f"{b:02x}" for b in shown)
            suffix = "..." if len(data) > args.max else ""
            if args.raw:
                print(f"[+{delta:8.6f}s] {direction} {len(data):4d} bytes | {hex_pretty}{suffix}")
            else:
                ascii_pretty = decode_ascii(shown)
                print(f"[+{delta:8.6f}s] {direction} {len(data):4d} bytes | {hex_pretty:<48s}{suffix:<3} | {ascii_pretty}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
