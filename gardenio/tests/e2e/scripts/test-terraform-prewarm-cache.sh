#!/usr/bin/env bash
# test-terraform-prewarm-cache.sh — sanity check that the agent-image terraform
# provider PREWARM actually populates a non-empty, CONSUMABLE plugin cache
# (task ka-0000002834, epic ka-0000004837).
#
# ── Why this test exists ──────────────────────────────────────────────────────
#
# The gardenio agent image pre-downloads the terraform providers used by the
# site-deployment terragrunt/terraform stack into a shared plugin cache
# (TF_PLUGIN_CACHE_DIR=/opt/terraform-plugin-cache) so that a `terraform init` /
# `terragrunt init` during EKS provisioning finds providers already on disk
# instead of re-fetching ~700 MB from the registry on every run. That prewarm is
# `gardenio/images/agent/install/terraform-providers.sh`, run by the agent
# image Dockerfile.
#
# The originating bead (ka-0000002834, from the MR !870 thread on site-deployment)
# flagged a real class of regression: a prewarm step that SILENTLY produces an
# EMPTY cache. On site-deployment the culprit was a `|| echo continuing` that
# swallowed init failures; in THIS monorepo the script is `set -e` + a real init,
# so a hard failure would red the build — but there was NO positive assertion
# that the cache is (a) non-empty, (b) contains exactly the declared providers,
# and (c) is actually CONSUMED by a subsequent init. A future edit that drops a
# provider, mismatches a version against versions.env, or leaves a truncated
# cache would pass silently and quietly re-introduce the slow-init regression the
# prewarm exists to prevent. This test closes that gap.
#
# ── Why NOT "provision a real EKS cluster" ────────────────────────────────────
#
# The acceptance criteria mention provisioning a cluster and measuring the apply
# speedup. A real EKS apply needs live AWS creds, costs money, and takes ~20 min
# — infeasible as a per-MR gate. The prewarm's PURPOSE is to make that apply's
# `terraform init` fast; the cheap, deterministic sanity check is to prove the
# cache the apply consumes is populated AND that terraform actually serves a
# provider "from the shared cache directory" (the exact fast-path signal, no AWS
# creds required). That is what this test asserts.
#
# ── Modes ─────────────────────────────────────────────────────────────────────
#
#   (default)         Full check. Resolves a cache directory (see below), asserts
#                     it is populated with one binary per declared provider at the
#                     versions.env-pinned version, asserts a total-size floor, and
#                     — when a terraform binary is available — proves the cache is
#                     CONSUMED via the "from the shared cache directory" signal.
#   --contract-only   Hermetic, bash-only. Runs just the CONTRACT checks (declared
#                     providers lockstep versions.env; Dockerfile COPYs the cache
#                     and sets TF_PLUGIN_CACHE_DIR). No terraform, no network —
#                     runs on bare ubuntu.
#
# Cache resolution order (default mode):
#   1. $PREWARM_CACHE_DIR            — explicit (point this at a freshly-built
#                                      layer cache).
#   2. $TF_PLUGIN_CACHE_DIR          — if already populated (running INSIDE the
#                                      built agent image / an agent pod).
#   3. run the real prewarm into a temp dir — if a terraform binary is on PATH
#                                      (or $TERRAFORM_BIN is set) and the network
#                                      is reachable.
#   If none of the above yields a cache, the test LOUD-SKIPS via the run-shard
#   skip-sentinel protocol (never a silent green).
#
# ── Anti-vacuity ──────────────────────────────────────────────────────────────
#
# The run asserts a floor of executed checks (MIN_CHECKS). If terraform is
# available the consume-proof leg is load-bearing: revert the prewarm to a no-op
# (empty cache) and the populate + consume legs FAIL; drop a provider from
# terraform-providers.sh without dropping its versions.env var (or vice versa)
# and the CONTRACT leg FAILS. A gutted cache cannot pass green.
#
# Hermetic (not wired into run-shard.sh). No CI job runs it today: checks.yml
# script-selftests leaves it out (39s, pulls providers over the network). Run it
# by hand, --contract-only or against a freshly-built cache.

set -uo pipefail

CONTRACT_ONLY=0
for arg in "$@"; do
  case "$arg" in
    --contract-only) CONTRACT_ONLY=1 ;;
    *) echo "unknown arg: $arg" >&2; exit 2 ;;
  esac
done

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Repo root: scripts/ -> e2e/ -> tests/ -> gardenio/ -> <root>
REPO_ROOT="$(cd "$SCRIPT_DIR/../../../.." && pwd)"
INSTALL_DIR="$REPO_ROOT/gardenio/images/agent/install"
PREWARM_SH="$INSTALL_DIR/terraform-providers.sh"
VERSIONS_ENV="$INSTALL_DIR/versions.env"
DOCKERFILE="$REPO_ROOT/gardenio/images/agent/Dockerfile"

PASS=0
FAIL=0
CHECKS=0
ERRORS=()
pass() { PASS=$((PASS + 1)); CHECKS=$((CHECKS + 1)); echo "  PASS: $1"; }
fail() { FAIL=$((FAIL + 1)); CHECKS=$((CHECKS + 1)); ERRORS+=("$1"); echo "  FAIL: $1"; }

# LOUD skip via the run-shard.sh skip-sentinel protocol (ka-0000000989): when
# run under run_module, writing the reason to $E2E_SKIP_SENTINEL and exiting 0
# is classified as SKIPPED, not PASSED. Standalone, it just prints + exits 0.
skip() {
  local reason="$1"
  echo "SKIP: $reason"
  # Write the sentinel on the line immediately preceding `exit 0` so the
  # skip-visibility scanner (test-skip-visibility-lint.sh Part B) sees a
  # sentinel-write guard on every skip-family exit-0 site — no green-by-skip.
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then echo "$reason" > "$E2E_SKIP_SENTINEL"; fi
  exit 0
}

for f in "$PREWARM_SH" "$VERSIONS_ENV"; do
  if [ ! -f "$f" ]; then
    echo "FAIL: required file not found: $f" >&2
    exit 1
  fi
done

# ── Load the pinned provider versions (single source of truth) ────────────────
# shellcheck disable=SC1090
source "$VERSIONS_ENV"

# ── Parse the declared providers from terraform-providers.sh ──────────────────
# The heredoc block declares each provider as:
#     <alias> = {
#       source  = "<namespace>/<name>"
#       version = "${TF_PROVIDER_<KEY>_VERSION}"
#     }
# Pair each `source` line with the following `version` line, in order, so the
# script that BUILDS the cache is the single source of truth for what the cache
# must CONTAIN. Emits: "<namespace> <name> <versions.env var name>" per provider.
declare -a P_NS P_NAME P_VAR
while IFS= read -r line; do
  if [[ "$line" =~ source[[:space:]]*=[[:space:]]*\"([^/\"]+)/([^\"]+)\" ]]; then
    P_NS+=("${BASH_REMATCH[1]}")
    P_NAME+=("${BASH_REMATCH[2]}")
  elif [[ "$line" =~ version[[:space:]]*=[[:space:]]*\"\$\{([A-Z_]+)\}\" ]]; then
    P_VAR+=("${BASH_REMATCH[1]}")
  fi
done < "$PREWARM_SH"

N_PROVIDERS=${#P_NS[@]}

echo "=== CONTRACT: declared providers lockstep with versions.env ==="
if [ "$N_PROVIDERS" -lt 7 ]; then
  fail "expected >= 7 declared providers in terraform-providers.sh, parsed $N_PROVIDERS"
else
  pass "parsed $N_PROVIDERS providers from terraform-providers.sh"
fi

if [ "${#P_VAR[@]}" -ne "$N_PROVIDERS" ]; then
  fail "source/version lines are unbalanced: $N_PROVIDERS sources vs ${#P_VAR[@]} versions (heredoc shape changed?)"
else
  pass "each declared provider has a paired version line ($N_PROVIDERS/$N_PROVIDERS)"
fi

# Every declared provider's version var must resolve to a non-empty value from
# versions.env — a source added without a matching pinned version reds here.
declare -a P_VER
CONTRACT_OK=1
for i in "${!P_NS[@]}"; do
  var="${P_VAR[$i]:-}"
  val="${!var:-}"
  P_VER+=("$val")
  if [ -z "$val" ]; then
    fail "provider ${P_NS[$i]}/${P_NAME[$i]}: version var \$$var is unset/empty in versions.env"
    CONTRACT_OK=0
  fi
done
if [ "$CONTRACT_OK" = 1 ] && [ "$N_PROVIDERS" -ge 7 ]; then
  pass "all $N_PROVIDERS provider version vars resolve from versions.env"
fi

# The Dockerfile must actually SHIP the cache and point terraform at it —
# otherwise a perfectly-built layer never reaches runtime (silent no-op).
if [ -f "$DOCKERFILE" ]; then
  if grep -Eq 'COPY --from=terraform-providers /opt/terraform-plugin-cache/' "$DOCKERFILE"; then
    pass "Dockerfile COPYs the prewarmed /opt/terraform-plugin-cache into the image"
  else
    fail "Dockerfile does not COPY /opt/terraform-plugin-cache from the terraform-providers stage"
  fi
  if grep -Eq 'ENV TF_PLUGIN_CACHE_DIR=/opt/terraform-plugin-cache' "$DOCKERFILE"; then
    pass "Dockerfile sets TF_PLUGIN_CACHE_DIR=/opt/terraform-plugin-cache"
  else
    fail "Dockerfile does not set TF_PLUGIN_CACHE_DIR=/opt/terraform-plugin-cache"
  fi
else
  echo "  note: Dockerfile not found at $DOCKERFILE (skipping ship-path contract checks)"
fi

if [ "$CONTRACT_ONLY" = 1 ]; then
  echo
  echo "=== CONTRACT-ONLY summary: $PASS passed, $FAIL failed ($CHECKS checks) ==="
  MIN_CHECKS=4
  if [ "$CHECKS" -lt "$MIN_CHECKS" ]; then
    echo "FAIL: only $CHECKS checks ran (< $MIN_CHECKS) — contract surface was gutted" >&2
    exit 1
  fi
  if [ "$FAIL" -gt 0 ]; then
    printf '  - %s\n' "${ERRORS[@]}" >&2
    exit 1
  fi
  echo "OK: prewarm provider/version contract holds"
  exit 0
fi

# ── Resolve a terraform binary (optional — enables the consume-proof) ─────────
TERRAFORM="${TERRAFORM_BIN:-}"
if [ -z "$TERRAFORM" ] && command -v terraform >/dev/null 2>&1; then
  TERRAFORM="$(command -v terraform)"
fi
HAVE_TF=0
if [ -n "$TERRAFORM" ] && [ -x "$TERRAFORM" ]; then
  HAVE_TF=1
fi

# ── Resolve the cache directory to verify ─────────────────────────────────────
CACHE_DIR=""
CACHE_SRC=""
TMP_CACHE=""
cleanup() { [ -n "$TMP_CACHE" ] && rm -rf "$TMP_CACHE"; }
trap cleanup EXIT

count_providers() { find "$1" -name 'terraform-provider-*' -type f 2>/dev/null | wc -l | tr -d ' '; }

if [ -n "${PREWARM_CACHE_DIR:-}" ] && [ -d "$PREWARM_CACHE_DIR" ]; then
  CACHE_DIR="$PREWARM_CACHE_DIR"
  CACHE_SRC="PREWARM_CACHE_DIR (explicit)"
elif [ -n "${TF_PLUGIN_CACHE_DIR:-}" ] && [ -d "${TF_PLUGIN_CACHE_DIR:-}" ] && [ "$(count_providers "$TF_PLUGIN_CACHE_DIR")" -gt 0 ]; then
  CACHE_DIR="$TF_PLUGIN_CACHE_DIR"
  CACHE_SRC="TF_PLUGIN_CACHE_DIR (baked image cache)"
elif [ "$HAVE_TF" = 1 ]; then
  # Live mode: run the REAL prewarm into a temp dir (needs network).
  TMP_CACHE="$(mktemp -d)"
  echo "=== no pre-populated cache found; running the real prewarm into $TMP_CACHE ==="
  if TERRAFORM_BIN="$TERRAFORM" TF_PLUGIN_CACHE_DIR="$TMP_CACHE" bash "$PREWARM_SH" >/tmp/prewarm.$$.log 2>&1; then
    CACHE_DIR="$TMP_CACHE"
    CACHE_SRC="live prewarm run of terraform-providers.sh"
  else
    echo "--- prewarm output (tail) ---"
    tail -30 /tmp/prewarm.$$.log 2>/dev/null || true
    rm -f /tmp/prewarm.$$.log
    fail "live prewarm run failed — terraform-providers.sh did not complete (see output above)"
    CACHE_DIR=""
  fi
  rm -f /tmp/prewarm.$$.log
fi

if [ -z "$CACHE_DIR" ]; then
  if [ "$FAIL" -gt 0 ]; then
    # A live-prewarm failure is a real FAIL, not a skip.
    echo
    echo "Results: $PASS passed, $FAIL failed"
    printf '  - %s\n' "${ERRORS[@]}" >&2
    exit 1
  fi
  skip "no terraform provider cache to verify: PREWARM_CACHE_DIR unset, TF_PLUGIN_CACHE_DIR empty/unset, and no terraform binary on PATH to run a live prewarm. Run inside the agent image, or with PREWARM_CACHE_DIR set, or with terraform installed + network."
fi

echo
echo "=== CACHE: verifying $CACHE_SRC at $CACHE_DIR ==="

# ── Populated: at least one binary per declared provider, at the pinned version
FOUND_TOTAL="$(count_providers "$CACHE_DIR")"
if [ "$FOUND_TOTAL" -gt 0 ]; then
  pass "cache contains $FOUND_TOTAL terraform-provider-* binaries (nonzero)"
else
  fail "cache is EMPTY — zero terraform-provider-* binaries under $CACHE_DIR (this is exactly the silent-empty-cache regression the prewarm must never re-introduce)"
fi

for i in "${!P_NS[@]}"; do
  ns="${P_NS[$i]}"; name="${P_NAME[$i]}"; ver="${P_VER[$i]:-}"
  [ -z "$ver" ] && continue  # already failed in the contract leg
  # linux_amd64 only — matches the prewarm's single-arch download.
  pdir="$CACHE_DIR"/registry.terraform.io/"$ns"/"$name"/"$ver"/linux_amd64
  binary="$(find "$pdir" -name 'terraform-provider-*' -type f 2>/dev/null | head -1)"
  if [ -n "$binary" ] && [ -x "$binary" ] && [ -s "$binary" ]; then
    sz="$(du -m "$binary" 2>/dev/null | cut -f1)"
    pass "$ns/$name v$ver present, executable, ${sz}MB ($(basename "$binary"))"
  else
    fail "$ns/$name v$ver MISSING or not an executable nonzero binary under $pdir"
  fi
done

# ── Cache size floor: aws alone is ~600 MB, so a healthy cache is > 100 MB. A
# truncated/partial cache (e.g. metadata-only, or a single small provider) reds.
CACHE_MB="$(du -sm "$CACHE_DIR" 2>/dev/null | cut -f1)"
CACHE_FLOOR_MB="${PREWARM_CACHE_FLOOR_MB:-100}"
echo "  measured cache size: ${CACHE_MB}MB (floor ${CACHE_FLOOR_MB}MB)"
if [ "${CACHE_MB:-0}" -ge "$CACHE_FLOOR_MB" ]; then
  pass "cache size ${CACHE_MB}MB >= ${CACHE_FLOOR_MB}MB floor"
else
  fail "cache size ${CACHE_MB}MB is below the ${CACHE_FLOOR_MB}MB floor — cache looks truncated/partial"
fi

# ── CONSUME PROOF (anti-vacuity core) ─────────────────────────────────────────
# Presence on disk is necessary but not sufficient — the prewarm only pays off if
# `terraform init` actually SERVES a provider FROM the cache. Drive a fresh module
# pinned to one small cached provider (hashicorp/local) and assert terraform
# reports it "from the shared cache directory". No AWS creds, no backend.
if [ "$HAVE_TF" = 1 ]; then
  echo
  echo "=== CONSUME: proving terraform serves a provider from the shared cache ==="
  local_ver=""
  for i in "${!P_NS[@]}"; do
    if [ "${P_NS[$i]}" = "hashicorp" ] && [ "${P_NAME[$i]}" = "local" ]; then
      local_ver="${P_VER[$i]:-}"; break
    fi
  done
  if [ -z "$local_ver" ]; then
    fail "could not determine hashicorp/local version for the consume-proof (provider dropped from prewarm?)"
  else
    WORKDIR="$(mktemp -d)"
    cat > "$WORKDIR/main.tf" <<EOF
terraform {
  required_providers {
    local = {
      source  = "hashicorp/local"
      version = "$local_ver"
    }
  }
}
EOF
    # First init records the lock file (provider selection + hashes). Then wipe
    # the installed plugins but KEEP the lock, so the second init has no version
    # discovery to do and — on a cache hit — prints the shared-cache line.
    if TF_PLUGIN_CACHE_DIR="$CACHE_DIR" "$TERRAFORM" -chdir="$WORKDIR" init -backend=false -input=false -no-color >/dev/null 2>&1; then
      rm -rf "$WORKDIR/.terraform"
      OUT="$(TF_PLUGIN_CACHE_DIR="$CACHE_DIR" "$TERRAFORM" -chdir="$WORKDIR" init -backend=false -input=false -no-color 2>&1)"
      if grep -q "from the shared cache directory" <<<"$OUT"; then
        pass "terraform init serves hashicorp/local v$local_ver 'from the shared cache directory' (cache is consumed → fast path proven)"
      else
        echo "--- terraform init output ---"; printf '%s\n' "$OUT"
        fail "terraform init did NOT report 'from the shared cache directory' — prewarmed cache is present but not consumed"
      fi
    else
      fail "terraform init (lock-priming pass) failed for the consume-proof"
    fi
    rm -rf "$WORKDIR"
  fi
else
  echo
  echo "  note: no terraform binary available — skipping the consume-proof leg."
  echo "        (cache-populated assertions above still ran against $CACHE_SRC.)"
fi

# ── Summary + anti-vacuity floor ──────────────────────────────────────────────
echo
echo "Results: $PASS passed, $FAIL failed ($CHECKS checks, cache=$CACHE_SRC)"
# Floor: contract legs (>=5) + populated (1) + per-provider (>=7) + size (1).
MIN_CHECKS=13
if [ "$CHECKS" -lt "$MIN_CHECKS" ]; then
  echo "FAIL: only $CHECKS checks ran (< $MIN_CHECKS) — the test surface was gutted" >&2
  exit 1
fi
if [ "$FAIL" -gt 0 ]; then
  printf '  - %s\n' "${ERRORS[@]}" >&2
  exit 1
fi
echo "OK: terraform provider prewarm populates a non-empty, consumable cache"
