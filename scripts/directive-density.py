#!/usr/bin/env python3
"""directive-density.py — the over-constraint proxy metric for injected prompt text.

Counts emphatic *directive markers* (MUST / NEVER / DO NOT / CRITICAL / Prohibited)
per 1k tokens across the corpora that get injected into every agent session, so we
have a reproducible baseline for the Opus/Fable-5 context audit (epic ka-0000007337,
task ka-0000001681). Directive density is a proxy for over-constraint: the "new rules
of context engineering for Claude 5-generation models" favour trusting the model's
judgment over stacking imperative shout-directives, so a high MUST/NEVER/DO-NOT count
per token is a smell even when the byte budget is fine.

USAGE
  # Measure the injected-prompt strings inside init_prompts.go plus arbitrary text files:
  python3 scripts/directive-density.py \
      --go gardenio/controller/internal/bridge/init_prompts.go \
      --file "CLAUDE.md (root)=CLAUDE.md" \
      --file "advice corpus=/path/to/advice_corpus.txt" \
      [--csv out.csv] [--json out.json] [--sections]

  # --go extracts only the injected VALUE string literals (skips the short map keys /
  #   Go boilerplate) so you measure the text an agent actually sees.
  # --file NAME=PATH measures a plain-text corpus verbatim.
  # --sections adds a per-(map-key) breakdown for the --go file.

TOKENIZER
  tiktoken cl100k_base — a documented PROXY tokenizer. The fleet's own injected-context
  budgets (gb advice audit, gb prime --size) are proxy-token based, so this is
  apples-to-apples for a baseline. Absolute token counts shift ~10-20% under the real
  Anthropic tokenizer, but per-source density RATIOS (the point of the baseline) are
  stable across tokenizers. Requires `pip install tiktoken`.

DIRECTIVE MARKER SET (primary / headline)
  MUST       \bMUST\b            uppercase; also catches "MUST NOT"
  NEVER      \bNEVER\b           uppercase
  DO NOT     \b[Dd][Oo] NOT\b    emphatic uppercase NOT: "Do NOT" / "do NOT" / "DO NOT"
  CRITICAL   \bCRITICAL\b        uppercase
  Prohibited \bProhibited\b      any case; always a directive header

  These corpora write the emphatic negative as "Do NOT"/"do NOT" (NOT uppercased for
  emphasis) far more than the fully-uppercase "DO NOT", so the primary count matches the
  realistic emphatic case-forms. Two sensitivity bands are also reported:
    STRICT  fully-uppercase literals only (MUST/NEVER/"DO NOT"/CRITICAL/PROHIBITED)
    BROAD   primary + every other emphatic uppercase marker (standalone NOT, IMPORTANT,
            WARNING, ALWAYS, REQUIRED) — the fuller over-constraint surface.
"""
import argparse
import csv
import json
import re
import sys

try:
    import tiktoken
except ImportError:
    sys.exit("error: needs tiktoken — `pip install tiktoken` (a documented proxy tokenizer)")

ENC = tiktoken.get_encoding("cl100k_base")

PRIMARY = {
    "MUST": re.compile(r"\bMUST\b"),
    "NEVER": re.compile(r"\bNEVER\b"),
    "DO NOT": re.compile(r"\b[Dd][Oo] NOT\b"),
    "CRITICAL": re.compile(r"\bCRITICAL\b"),
    "Prohibited": re.compile(r"\bProhibited\b"),
}
STRICT = {
    "MUST": re.compile(r"\bMUST\b"),
    "NEVER": re.compile(r"\bNEVER\b"),
    "DO NOT": re.compile(r"\bDO NOT\b"),
    "CRITICAL": re.compile(r"\bCRITICAL\b"),
    "PROHIBITED": re.compile(r"\bPROHIBITED\b"),
}
BROAD_EXTRA = {
    "NOT(all uc)": re.compile(r"\bNOT\b"),
    "IMPORTANT": re.compile(r"\bIMPORTANT\b"),
    "WARNING": re.compile(r"\bWARNING\b"),
    "ALWAYS": re.compile(r"\bALWAYS\b"),
    "REQUIRED": re.compile(r"\bREQUIRED\b"),
}


def _count(patterns, text):
    per = {k: len(p.findall(text)) for k, p in patterns.items()}
    return sum(per.values()), per


def _tokens(text):
    return len(ENC.encode(text))


def measure(name, text):
    ptot, pper = _count(PRIMARY, text)
    stot, _ = _count(STRICT, text)
    btot, _ = _count(BROAD_EXTRA, text)
    broad = pper["MUST"] + pper["NEVER"] + pper["CRITICAL"] + pper["Prohibited"] + btot
    tk = _tokens(text)

    def dens(n):
        return round(n / tk * 1000, 2) if tk else 0.0

    return {
        "source": name,
        "directive_count": ptot,
        "total_tokens": tk,
        "directive_density": dens(ptot),
        "per_marker": pper,
        "strict_count": stot,
        "strict_density": dens(stot),
        "broad_count": broad,
        "broad_density": dens(broad),
        "chars": len(text),
    }


def snippets(text, k=3):
    cand = []
    for ln in text.split("\n"):
        s = ln.strip()
        if len(s) < 15:
            continue
        d, _ = _count(PRIMARY, s)
        if d >= 1:
            cand.append((d, _tokens(s), s))
    cand.sort(key=lambda x: (x[0], -x[1]), reverse=True)
    out, seen = [], set()
    for d, _tk, s in cand:
        if s[:60] in seen:
            continue
        seen.add(s[:60])
        out.append({"directives": d, "text": s[:220]})
        if len(out) >= k:
            break
    return out


# ---- Go string-literal extraction -------------------------------------------
def _tokenize_go(src):
    """Yield ('str', unescaped) and ('punct', ':') tokens, skipping // comments."""
    i, n, out = 0, len(src), []
    while i < n:
        c = src[i]
        if c == '"':
            j, buf = i + 1, []
            while j < n:
                cj = src[j]
                if cj == "\\":
                    nx = src[j + 1]
                    buf.append({"n": "\n", "t": "\t", "r": "\r", '"': '"',
                                "\\": "\\", "`": "`", "'": "'"}.get(nx, nx))
                    j += 2
                    continue
                if cj == '"':
                    j += 1
                    break
                buf.append(cj)
                j += 1
            out.append(("str", "".join(buf)))
            i = j
        elif c == "`":
            j = src.find("`", i + 1)
            j = n if j < 0 else j
            out.append(("str", src[i + 1:j]))
            i = j + 1
        elif c == "/" and i + 1 < n and src[i + 1] == "/":
            j = src.find("\n", i)
            i = n if j < 0 else j
        elif c == ":":
            out.append(("punct", ":"))
            i += 1
        else:
            i += 1
    return out


def extract_go_values(src):
    """Return only VALUE string literals (a literal is a KEY iff followed by ':')."""
    toks = _tokenize_go(src)
    vals = []
    for idx, t in enumerate(toks):
        if t[0] != "str":
            continue
        nxt = toks[idx + 1] if idx + 1 < len(toks) else None
        is_key = nxt and nxt[0] == "punct" and nxt[1] == ":"
        if not is_key:
            vals.append(t[1])
    return "\n".join(vals)


def go_sections(src, topkeys):
    """(top-key, sub-key) -> concatenated value text, for a per-section breakdown."""
    toks = _tokenize_go(src)
    seq = []
    for idx, t in enumerate(toks):
        if t[0] != "str":
            continue
        nxt = toks[idx + 1] if idx + 1 < len(toks) else None
        seq.append(("key" if (nxt and nxt[0] == "punct" and nxt[1] == ":") else "val", t[1]))
    sections, top, sub = {}, None, None
    for kind, s in seq:
        if kind == "key":
            if s in topkeys:
                top, sub = s, None
            else:
                sub = s
                sections.setdefault((top, sub), "")
        elif top is not None and sub is not None:
            sections[(top, sub)] += s
    return sections


TOPKEYS = {
    "config:nudge-prompts:global",
    "config:claude-instructions:global",
    "config:claude-instructions:role:thread",
    "config:claude-instructions:role:gardener",
    "config:claude-instructions:role:polecat",
}


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--go", help="Go source; measure only injected VALUE string literals")
    ap.add_argument("--file", action="append", default=[], metavar="NAME=PATH",
                    help="measure a plain-text corpus verbatim (repeatable)")
    ap.add_argument("--sections", action="store_true", help="per-map-key breakdown of --go")
    ap.add_argument("--csv", help="write results CSV to this path")
    ap.add_argument("--json", help="write full results JSON to this path")
    args = ap.parse_args()

    results = []
    if args.go:
        src = open(args.go).read()
        r = measure(f"{args.go.split('/')[-1]} (injected values)", extract_go_values(src))
        r["examples"] = snippets(extract_go_values(src))
        results.append(r)
    for spec in args.file:
        name, _, path = spec.partition("=")
        text = open(path).read()
        r = measure(name, text)
        r["examples"] = snippets(text)
        results.append(r)

    # console table
    print(f"{'source':<40} {'PRIM':>4} {'tok':>6} {'d/1k':>6} | {'STR':>3} {'sd/1k':>5} | {'BRD':>3} {'bd/1k':>5}")
    print("-" * 90)
    td = tt = 0
    for r in results:
        print(f"{r['source']:<40} {r['directive_count']:>4} {r['total_tokens']:>6} "
              f"{r['directive_density']:>6} | {r['strict_count']:>3} {r['strict_density']:>5} | "
              f"{r['broad_count']:>3} {r['broad_density']:>5}")
        td += r["directive_count"]
        tt += r["total_tokens"]
    print("-" * 90)
    if tt:
        print(f"{'CORPUS TOTAL':<40} {td:>4} {tt:>6} {round(td / tt * 1000, 2):>6}")

    if args.sections and args.go:
        print("\nPer-section breakdown of", args.go)
        rows = []
        for (top, sub), text in go_sections(src, TOPKEYS).items():
            d, _ = _count(PRIMARY, text)
            tk = _tokens(text)
            rows.append((d, tk, top, sub, round(d / tk * 1000, 2) if tk else 0.0))
        rows.sort(key=lambda x: x[0], reverse=True)
        print(f"  {'top-key':<38} {'sub':<20} {'dir':>3} {'tok':>5} {'d/1k':>6}")
        for d, tk, top, sub, dn in rows:
            if d or tk > 200:
                short = (top or "(none)").replace("config:claude-instructions:", "cci:").replace("config:nudge-prompts:", "np:")
                print(f"  {short:<38} {str(sub)[:20]:<20} {d:>3} {tk:>5} {dn:>6}")

    if args.csv:
        cols = ["source", "directive_count", "total_tokens", "directive_density_per_1k",
                "MUST", "NEVER", "DO_NOT", "CRITICAL", "Prohibited",
                "strict_count", "strict_density_per_1k", "broad_count", "broad_density_per_1k", "examples"]
        with open(args.csv, "w", newline="") as fh:
            w = csv.DictWriter(fh, fieldnames=cols)
            w.writeheader()
            for r in results:
                pm = r["per_marker"]
                w.writerow({
                    "source": r["source"], "directive_count": r["directive_count"],
                    "total_tokens": r["total_tokens"], "directive_density_per_1k": r["directive_density"],
                    "MUST": pm["MUST"], "NEVER": pm["NEVER"], "DO_NOT": pm["DO NOT"],
                    "CRITICAL": pm["CRITICAL"], "Prohibited": pm["Prohibited"],
                    "strict_count": r["strict_count"], "strict_density_per_1k": r["strict_density"],
                    "broad_count": r["broad_count"], "broad_density_per_1k": r["broad_density"],
                    "examples": "; ".join(f'"{e["text"][:90].strip()}" ({e["directives"]})' for e in r["examples"][:3]),
                })
            w.writerow({"source": "CORPUS TOTAL", "directive_count": td, "total_tokens": tt,
                        "directive_density_per_1k": round(td / tt * 1000, 2) if tt else 0.0})
        print("wrote CSV:", args.csv)

    if args.json:
        json.dump(results, open(args.json, "w"), indent=2)
        print("wrote JSON:", args.json)


if __name__ == "__main__":
    main()
