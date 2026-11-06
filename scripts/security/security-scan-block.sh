#!/usr/bin/env bash
#
# security-scan-block.sh — deterministic `Security-Scan:` MR-trailer generator.
#
#   Bundle: ka-0000005053 §2.3 (security affordance). Task: ka-0000007861.
#   Pairs with: internal/bridge/security_gate.go (ParseSecurityScanBlock +
#               EvaluateSecurityGate — the validator `gb mr security-gate` wraps)
#               and gardenio/locks/security-tools.lock (the version pins this block records
#               and the gate re-checks). Mirrors peel-trailer.sh's library/CLI
#               split so the block-assembly logic is hermetically testable.
#
# ── What it emits ────────────────────────────────────────────────────────────
# A single machine-parseable trailer line for an MR description:
#
#   Security-Scan: <sha> gitleaks=<v>=<result> govulncheck=<v>=<result> \
#       osv-scanner=<v>=<result> cargo-audit=<v>=<result> cargo-deny=<v>=<result> \
#       [security-review=<effort>=<result>]
#
# Each scanner's VERSION is pulled from gardenio/locks/security-tools.lock (never typed by
# hand — the gate rejects any block whose versions drift from the lock, so the
# generator must source the same pins). The <result> tokens come from the actual
# scanner runs the caller passes in (or from the recorded CI verdicts).
#
# This is the honest-provenance handoff: an upstream reviewer reads one line and
# `gb mr security-gate` re-validates it against the lock + the green named status
# checks, so the block cannot claim a scan that did not run at the pinned version.
#
# ── Invocation ───────────────────────────────────────────────────────────────
#   security-scan-block.sh --sha <sha> \
#       --result gitleaks=clean --result govulncheck=0-reachable \
#       --result osv-scanner=clean --result cargo-audit=clean \
#       --result cargo-deny=pass [--review high=0-findings] [--lock <path>]
#
#   --sha <sha>          head commit the scan ran against (REQUIRED).
#   --result <name>=<r>  a scanner result (repeatable). <name> is the canonical
#                        scanner token; <r> the result token (clean|0-reachable|
#                        pass|<n>-findings|fail...). A scanner with no --result
#                        defaults to result=unknown (which the gate FAILS on —
#                        fail-loud, never silently "clean").
#   --review <e>=<r>     the two-tier /security-review tuple (effort + result),
#                        e.g. high=0-findings. Omit for a low-tier diff.
#   --lock <path>        security-tools.lock path (default: gardenio/locks/security-tools.lock).
#   --verify             read a block on stdin and re-validate its versions
#                        against the lock (round-trip proof); exit 6 on drift.
#
# Distinct return codes (mirrors peel-trailer.sh):
readonly RC_OK=0        # block emitted / verified
readonly RC_USAGE=2     # bad invocation / missing required input
readonly RC_NO_LOCK=3   # lock file unreadable / no pins
readonly RC_VERIFY=6    # --verify: a recorded version drifts from the lock

set -uo pipefail

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ── Tunable config (overridable via flags or, for the test, env) ─────────────
SHA="${SHA:-}"
LOCK="${LOCK:-$SELF_DIR/../../gardenio/locks/security-tools.lock}"
REVIEW="${REVIEW:-}"                 # <effort>=<result> or empty
VERIFY=0
declare -A RESULTS=()                # canonical scanner name -> result token

# The canonical scanner set the block records, in a fixed emission order (stable
# output regardless of --result argument order). Kept in sync with
# SecurityStatusCheckNames / the lock keys in security_gate.go.
readonly SCANNERS=(gitleaks govulncheck osv-scanner cargo-audit cargo-deny)

log() { echo "[security-scan-block] $*" >&2; }

# canon_name <token> — lowercase, '_'->'-', trim. Matches canonScannerName() in
# security_gate.go so the generator and validator agree on scanner identity.
canon_name() {
  local s="$1"
  s="$(printf '%s' "$s" | tr '[:upper:]' '[:lower:]' | tr '_' '-')"
  s="${s#"${s%%[![:space:]]*}"}"
  s="${s%"${s##*[![:space:]]}"}"
  printf '%s' "$s"
}

# lock_version <canonical-name> — echo the pinned version for a scanner from the
# lock, or empty. The lock key is the UPPERCASE name with '-'->'_' + _VERSION.
lock_version() {
  local name key up
  name="$1"
  up="$(printf '%s' "$name" | tr '[:lower:]' '[:upper:]' | tr '-' '_')"
  key="${up}_VERSION"
  sed -nE "s/^${key}=[\"']?([^\"']+)[\"']?[[:space:]]*$/\1/p" "$LOCK" | head -1
}

# ── verify: re-validate a block on stdin against the lock ────────────────────
verify_block() {
  local block name ver pin drift=0
  block="$(cat)"
  # Extract the payload after the Security-Scan: key (first match).
  local payload
  payload="$(printf '%s\n' "$block" | sed -nE 's/^[[:space:]]*[Ss]ecurity-[Ss]can[[:space:]]*:[[:space:]]*(.+)$/\1/p' | head -1)"
  if [ -z "$payload" ]; then
    log "VERIFY FAIL: input carries no Security-Scan: block."
    return "$RC_VERIFY"
  fi
  local tok
  for tok in $payload; do
    case "$tok" in
      *=*=*)
        name="$(canon_name "${tok%%=*}")"
        ver="$(printf '%s' "$tok" | cut -d= -f2)"
        [ "$name" = "security-review" ] && continue
        pin="$(lock_version "$name")"
        if [ -n "$pin" ] && [ "$ver" != "$pin" ]; then
          log "VERIFY FAIL: $name version drift — block=$ver lock=$pin"
          drift=1
        fi
        ;;
      *) : ;;  # SHA or malformed token — the Go gate validates shape; we only check versions
    esac
  done
  if [ "$drift" = "1" ]; then
    return "$RC_VERIFY"
  fi
  log "VERIFY OK: all recorded scanner versions match the lock."
  return "$RC_OK"
}

# ── emit: build the block from the lock pins + result tokens ──────────────────
emit_block() {
  local line name ver res
  line="Security-Scan: ${SHA}"
  for name in "${SCANNERS[@]}"; do
    ver="$(lock_version "$name")"
    if [ -z "$ver" ]; then
      log "FATAL: no pinned version for scanner '$name' in $LOCK"
      return "$RC_NO_LOCK"
    fi
    res="${RESULTS[$name]:-unknown}"
    line="${line} ${name}=${ver}=${res}"
  done
  if [ -n "$REVIEW" ]; then
    # REVIEW is <effort>=<result>; record as security-review=<effort>=<result>.
    line="${line} security-review=${REVIEW}"
  fi
  printf '%s\n' "$line"
}

usage() {
  sed -nE 's/^# ?//; 1,/^set -uo/p' "$0" | sed '$d' >&2
}

main() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --sha)     SHA="$2"; shift 2;;
      --result)
        local kv n r
        kv="$2"; shift 2
        n="$(canon_name "${kv%%=*}")"
        r="${kv#*=}"
        if [ -z "$n" ] || [ "$n" = "$r" ]; then
          log "FATAL: --result must be <name>=<result>, got '$kv'"
          return "$RC_USAGE"
        fi
        RESULTS["$n"]="$r"
        ;;
      --review)  REVIEW="$2"; shift 2;;
      --lock)    LOCK="$2"; shift 2;;
      --verify)  VERIFY=1; shift;;
      -h|--help) usage; return "$RC_OK";;
      *) log "unknown arg: $1"; return "$RC_USAGE";;
    esac
  done

  if [ ! -r "$LOCK" ]; then
    log "FATAL: lock file not readable: $LOCK"
    return "$RC_NO_LOCK"
  fi

  if [ "$VERIFY" = "1" ]; then
    verify_block
    return $?
  fi

  if [ -z "$SHA" ]; then
    log "FATAL: --sha <head-commit> is required."
    return "$RC_USAGE"
  fi

  emit_block
}

# Library mode: define functions only (the test sources us). Otherwise run.
if [ "${SECURITY_SCAN_BLOCK_LIB:-0}" != "1" ]; then
  main "$@"
fi
