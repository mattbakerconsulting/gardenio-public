#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────────────
# gardenio-version — manage the local user copies of the kd/gb binaries.
#
# Reports version skew between YOUR binaries and the live daemon, and (with
# --update) rebuilds + reinstalls them. This is the operator-facing half of the
# skew signal from ka-0000002965: that MR made the CLIs *able* to warn, but a
# warning only fires mid-command and only when the local build carries a
# parseable calver — see "Why this script exists" below.
#
#   gardenio-version.sh            # report: local vs daemon, verdict, exit 1 if skewed
#   gardenio-version.sh --update   # pull + rebuild + reinstall, then re-report
#   gardenio-version.sh --build    # rebuild + reinstall the checkout as-is (no pull)
#   gardenio-version.sh --quiet    # one line, for hooks/prompts
#   gardenio-version.sh --nudge    # once-a-day cached check; AUTO-UPDATES when skewed
#
# Read-only unless --update/--build is passed, or --nudge finds you skewed (it then
# self-updates in the background — see below). No VPN, no kubectl — just the
# public CLI shim and a Go toolchain.
#
#   GARDENIO_NO_AUTO_UPDATE=1   --nudge only advises; never builds. The off switch.
#
# ── Why this script exists ───────────────────────────────────────────────────
# The daemon stamps `x-kbeads-version` on every response and kd/gb warn once on
# stderr when the local binary is materially behind (buildinfo.SkewCheck).
# But SkewCheck returns "no skew" unless BOTH sides parse as calver YYYY.DDD,
# and a plain `go build` (what the old onboard-mac.sh did) injects no ldflags —
# so the binary reports version "dev", `calverDays("dev")` fails, and the check
# silently no-ops. The safety net was off on exactly the laptops it was for.
# This script (a) stamps a real calver when it builds, so the in-CLI warning
# starts working, and (b) gives an explicit, on-demand answer that does not
# depend on the binary being able to warn about itself.
# ─────────────────────────────────────────────────────────────────────────────
set -uo pipefail

REPO="${GARDENIO_REPO:-$HOME/gardenio}"
BIN="${GARDENIO_BIN_DIR:-$HOME/.local/bin}"
# Mirrors buildinfo.SkewThresholdDays — keep in sync with
# kbeads/internal/buildinfo/buildinfo.go.
THRESHOLD_DAYS="${GARDENIO_SKEW_THRESHOLD_DAYS:-14}"
# No built-in host: a deployment names its own daemon in BEADS_HTTP_URL.
HOST_DEFAULT=""
# Every skewed --quiet line ends with this exact call-to-action clause, so
# --nudge can strip it with one expansion when it is about to do the update
# itself rather than telling you to. Keep it in sync with the quiet block.
FIXUP=" — run gardenio-version.sh --update"

MODE=report
NO_PULL=""
for a in "$@"; do
  case "$a" in
    --update|-u) MODE=update ;;
    --quiet|-q)  MODE=quiet ;;
    --nudge)     MODE=nudge ;;
    --build|-b)  MODE=update; NO_PULL=1 ;;
    --help|-h)   sed -n '2,32p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) printf 'unknown flag: %s (try --help)\n' "$a" >&2; exit 2 ;;
  esac
done

ok()   { printf '  \033[1;32m✅\033[0m %s\n' "$*"; }
adv()  { printf '  \033[1;33m⚠️ \033[0m %s\n' "$*"; }
err()  { printf '  \033[1;31m❌ %s\033[0m\n' "$*"; }
fix()  { printf '       \033[2m↳ %s\033[0m\n' "$*"; }
hdr()  { printf '\n\033[1m%s\033[0m\n' "$*"; }
say()  { printf '\033[1;36m==>\033[0m %s\n' "$*"; }

# calver_days <YYYY.DDD[.N]> -> absolute day count on stdout, or empty if the
# value is not a parseable calver (a "dev" build, a git-describe tag, empty).
# Same coarse ordering as buildinfo.calverDays: year*366 + day-of-year.
calver_days() {
  local v="${1:-}"
  v="${v#v}"; v="${v%%[-+]*}"
  # Assign in separate statements: bash expands an entire `local a=… b=$a` line
  # before executing it, so a same-line back-reference is unset under `set -u`.
  local year="${v%%.*}"
  local rest="${v#*.}"
  local doy="${rest%%.*}"
  case "$year" in ''|*[!0-9]*) return 0 ;; esac
  case "$doy"  in ''|*[!0-9]*) return 0 ;; esac
  [ "$year" -ge 2000 ] && [ "$year" -le 3000 ] || return 0
  [ "$doy"  -ge 1 ]    && [ "$doy"  -le 366 ]  || return 0
  echo $(( year * 366 + doy ))
}

# local_version <kd|gb> -> the binary's reported version, or empty.
local_version() { command -v "$1" >/dev/null 2>&1 && "$1" --version 2>/dev/null | head -1 | tr -d '[:space:]'; }

# daemon_version -> the daemon's calver from the x-kbeads-version response
# header, or empty. Needs the bearer: the shim 401s unauthenticated (and the
# WAF 403s an empty User-Agent, hence the explicit -A).
daemon_version() {
  local url="${BEADS_HTTP_URL:-$HOST_DEFAULT}" tok="${GARDENIO_TOKEN:-${GITLAB_TOKEN:-}}"
  [ -n "$tok" ] && [ -n "$url" ] || return 0
  curl -sS --max-time 12 -D - -o /dev/null -A "gardenio-version" \
       -H "Authorization: Bearer $tok" "${url%/}/v1/health" 2>/dev/null \
    | tr -d '\r' | awk 'tolower($1)=="x-kbeads-version:"{print $2}' | head -1
}

# ── nudge: cheap once-a-day check, safe to put in a shell rc ─────────────────
# Prints nothing when current (or already checked today), so it can't become
# shell noise. When it finds you skewed it AUTO-UPDATES (operator ruling,
# ka-0000000468): the friction this whole change exists to fix was a human
# never getting around to rebuilding, and an advisory line is exactly what a
# busy person ignores. Escape hatch: GARDENIO_NO_AUTO_UPDATE=1.
#
# Three properties make an unattended build safe enough to run from a shell rc:
#   1. It is BACKGROUNDED. A login shell never blocks on a Go compile.
#   2. The install is an atomic rename (see install_bin), so a session already
#      running kd/gb keeps its inode — the binary is never yanked mid-command.
#   3. It refuses to touch a checkout that isn't clean and on main. Auto-pulling
#      or auto-building someone's work-in-progress is a bigger promise than we
#      should make unattended, so those cases degrade to the advisory line.
# The stamp file is touched BEFORE the network call so a hang or a flapping
# daemon cannot re-cost every new shell.
if [ "$MODE" = nudge ]; then
  STATE="${XDG_CACHE_HOME:-$HOME/.cache}/gardenio"
  STAMP="$STATE/version-check"
  FAILED="$STATE/update-failed"
  LOG="$STATE/update.log"
  LOCK="$STATE/update.lock"

  # Surface a failed background update once, promptly — the build that failed
  # was detached, so this is the only place its exit status can reach a human.
  # Checked before the daily gate: costs a file test, and a silent failure that
  # waits until tomorrow to admit itself is worse than the skew it was fixing.
  if [ -f "$FAILED" ]; then
    printf '\033[1;31m%s\033[0m\n' "gardenio: automatic kd/gb update failed — see $LOG" >&2
    printf '\033[1;33m%s\033[0m\n' "         retry by hand: $0 --update" >&2
    rm -f "$FAILED" 2>/dev/null
  fi

  if [ -f "$STAMP" ] && [ -z "$(find "$STAMP" -mtime +0 2>/dev/null)" ]; then exit 0; fi
  mkdir -p "$STATE" 2>/dev/null && : > "$STAMP" 2>/dev/null
  line="$("$0" --quiet 2>/dev/null)" && exit 0

  # Skewed. Decide synchronously whether we may fix it ourselves, so the reason
  # we did not lands in the shell the operator is actually looking at.
  why=""
  if [ -n "${GARDENIO_NO_AUTO_UPDATE:-}" ]; then why="auto-update disabled (GARDENIO_NO_AUTO_UPDATE)"
  elif ! command -v go >/dev/null 2>&1; then why="no Go toolchain to build with"
  elif [ ! -d "$REPO/kbeads" ] || [ ! -d "$REPO/gardenio/controller" ]; then why="no gardenio checkout at $REPO"
  elif [ -n "$(git -C "$REPO" status --porcelain 2>/dev/null)" ]; then why="$REPO has uncommitted changes"
  elif [ "$(git -C "$REPO" rev-parse --abbrev-ref HEAD 2>/dev/null)" != "main" ]; then why="$REPO is not on main"
  else
    # An update installs into $BIN. If PATH resolves kd/gb somewhere else, the
    # build would land and the version you run would not move — and since the
    # skew never clears, we would rebuild every single day forever without ever
    # saying why. Refuse, and name the fix; the full report explains shadowing.
    for b in kd gb; do
      r="$(command -v "$b" 2>/dev/null)"
      if [ -n "$r" ] && [ "$r" != "$BIN/$b" ]; then
        why="$b runs from $r, not $BIN — an update there would not change it"; break
      fi
    done
  fi

  if [ -n "$why" ]; then
    printf '\033[1;33m%s\033[0m\n' "$line" >&2
    printf '\033[2m%s\033[0m\n' "         (not auto-updating: $why)" >&2
    exit 0
  fi

  # One updater at a time: several terminals can open at once and they would
  # otherwise race on the same checkout and the same install path. mkdir is the
  # atomic test-and-set. A lock older than an hour is a corpse, not a build.
  if [ -d "$LOCK" ] && [ -n "$(find "$LOCK" -maxdepth 0 -mmin +60 2>/dev/null)" ]; then rmdir "$LOCK" 2>/dev/null; fi
  mkdir "$LOCK" 2>/dev/null || exit 0   # another shell is already on it

  printf '\033[1;33m%s\033[0m\n' "${line%"$FIXUP"}" >&2
  printf '\033[2m%s\033[0m\n' "         updating in the background; log: $LOG" >&2
  # Detached so the shell returns immediately. Failure leaves a marker for the
  # next nudge rather than dying silently in a log nobody opens.
  nohup bash -c '
    if "$1" --update >"$2" 2>&1; then rm -f "$3"; else : > "$3"; fi
    rmdir "$4" 2>/dev/null
  ' _ "$0" "$LOG" "$FAILED" "$LOCK" >/dev/null 2>&1 &
  exit 0   # advisory only: never fail a login shell
fi

# ── update: pull + rebuild with a real calver stamp + reinstall ──────────────
if [ "$MODE" = update ]; then
  [ -d "$REPO/kbeads" ] && [ -d "$REPO/gardenio/controller" ] || {
    err "no gardenio checkout at $REPO"
    fix "git clone https://github.com/mattbakerconsulting/gardenio.git $REPO"
    fix "(or set GARDENIO_REPO to your checkout)"; exit 1; }
  command -v go >/dev/null 2>&1 || { err "go not installed — needed to build"; fix "install Go: https://go.dev/dl/"; exit 1; }
  mkdir -p "$BIN"

  if [ -n "$NO_PULL" ]; then
    say "building $REPO as checked out (--build: no pull)"
  elif [ -n "$(git -C "$REPO" status --porcelain 2>/dev/null)" ]; then
    adv "$REPO has local changes — skipping pull, building your working tree as-is"
  else
    say "updating $REPO (fast-forward only)"
    git -C "$REPO" fetch --quiet origin 2>/dev/null || adv "git fetch failed — building the current checkout"
    git -C "$REPO" pull --ff-only --quiet origin main 2>/dev/null \
      || adv "fast-forward pull failed (diverged or offline) — building the current checkout"
  fi

  # Stamp the COMMIT date, not the build date: it reflects how old the CODE is.
  # Building a months-old checkout today must not claim to be current — that
  # would defeat the entire skew signal. (The Makefiles use `git describe
  # --tags`, which also yields a calver here because the release tags ARE
  # calvers — but only on a checkout with tags fetched. The commit date needs
  # no tags, so it works on a shallow or tagless clone too.)
  CALVER="$(git -C "$REPO" log -1 --format=%cd --date=format:%Y.%j 2>/dev/null || date -u +%Y.%j)"
  COMMIT="$(git -C "$REPO" rev-parse --short=12 HEAD 2>/dev/null || echo unknown)"
  BRANCH="$(git -C "$REPO" rev-parse --abbrev-ref HEAD 2>/dev/null || echo unknown)"
  DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  say "building kd + gb at calver $CALVER (commit $COMMIT)"

  # Build to a sibling temp, then rename(2) into place. `go build -o` writes its
  # output IN PLACE, which fails ETXTBSY against a running binary on Linux and
  # can corrupt a live process on macOS — a real hazard now that --nudge can
  # fire this unattended while you are using kd. A rename is atomic and leaves
  # anything already running on the old inode until it exits. Sibling rather
  # than $TMPDIR so src and dest are guaranteed to share a filesystem.
  trap 'rm -f "$BIN"/.kd.new.$$ "$BIN"/.gb.new.$$ 2>/dev/null' EXIT
  KD_BI="gardenio/kbeads/internal/buildinfo"
  ( cd "$REPO/kbeads" && CGO_ENABLED=0 go build \
      -ldflags "-X main.Version=$CALVER -X main.Commit=$COMMIT -X $KD_BI.Branch=$BRANCH -X $KD_BI.Date=$DATE" \
      -o "$BIN/.kd.new.$$" ./cmd/kd/ ) || { err "kd build failed"; exit 1; }
  GB_BI="gardenio/controller/internal/buildinfo"
  ( cd "$REPO/gardenio/controller" && CGO_ENABLED=0 go build \
      -ldflags "-X main.version=$CALVER -X main.commit=$COMMIT -X $GB_BI.Branch=$BRANCH -X $GB_BI.Date=$DATE" \
      -o "$BIN/.gb.new.$$" ./cmd/gb/ ) || { err "gb build failed"; exit 1; }
  # Both built — only now swap either, so a failed gb build cannot leave you
  # with a freshly-installed kd and a stale gb (the mismatched pair the report
  # below flags).
  chmod 0755 "$BIN/.kd.new.$$" "$BIN/.gb.new.$$" 2>/dev/null
  mv -f "$BIN/.kd.new.$$" "$BIN/kd" || { err "installing kd into $BIN failed"; exit 1; }
  mv -f "$BIN/.gb.new.$$" "$BIN/gb" || { err "installing gb into $BIN failed"; exit 1; }
  [ -f "$REPO/scripts/gb-claude" ] && install -m 0755 "$REPO/scripts/gb-claude" "$BIN/gb-claude"
  ok "installed kd + gb into $BIN at $CALVER"
fi

# ── report ───────────────────────────────────────────────────────────────────
KD_V="$(local_version kd)"; GB_V="$(local_version gb)"; D_V="$(daemon_version)"
KD_D="$(calver_days "$KD_V")"; GB_D="$(calver_days "$GB_V")"; D_D="$(calver_days "$D_V")"

# The local side is whichever binary is OLDER — you are only as current as your
# stalest binary, and a mismatched kd/gb pair is itself a problem.
LOCAL_V="$KD_V"; LOCAL_D="$KD_D"
if [ -n "$GB_D" ] && { [ -z "$LOCAL_D" ] || [ "$GB_D" -lt "$LOCAL_D" ]; }; then LOCAL_V="$GB_V"; LOCAL_D="$GB_D"; fi

GAP=""
[ -n "$LOCAL_D" ] && [ -n "$D_D" ] && GAP=$(( D_D - LOCAL_D ))

if [ "$MODE" = quiet ]; then
  if [ -z "$D_V" ];   then echo "gardenio: daemon version unknown (no token or unreachable)"; exit 0; fi
  if [ -z "$LOCAL_D" ]; then echo "gardenio: kd/gb are unstamped ('${LOCAL_V:-none}'), so skew is undetectable$FIXUP"; exit 1; fi
  if [ "$GAP" -gt "$THRESHOLD_DAYS" ]; then echo "gardenio: kd/gb are ~${GAP}d behind the daemon ($LOCAL_V vs $D_V)$FIXUP"; exit 1; fi
  echo "gardenio: kd/gb current ($LOCAL_V vs daemon $D_V)"; exit 0
fi

rc=0
hdr "Local binaries"
if [ -n "$KD_V" ]; then ok "kd  $KD_V  ($(command -v kd))"; else err "kd not found on PATH"; fix "$0 --update"; rc=1; fi
if [ -n "$GB_V" ]; then ok "gb  $GB_V  ($(command -v gb))"; else err "gb not found on PATH"; fix "$0 --update"; rc=1; fi
if [ -n "$KD_D" ] && [ -n "$GB_D" ] && [ "$KD_D" -ne "$GB_D" ]; then
  adv "kd and gb are from different builds ($KD_V vs $GB_V) — install them as a pair"
  fix "$0 --update"
  rc=1
fi
# Shadowing: --update installs into $BIN, but the version reported above is
# whatever PATH resolves — so an install can succeed while the operator keeps
# running an older copy from somewhere earlier on PATH. Catch that explicitly.
for b in kd gb; do
  resolved="$(command -v "$b" 2>/dev/null)"
  if [ -x "$BIN/$b" ] && [ -n "$resolved" ] && [ "$resolved" != "$BIN/$b" ]; then
    adv "$b resolves to $resolved, shadowing the copy in $BIN"
    fix "put $BIN first: export PATH=\"\$HOME/.local/bin:\$PATH\""
    rc=1
  fi
done

hdr "Daemon"
if [ -n "$D_V" ]; then ok "daemon $D_V  ($BEADS_HTTP_URL)"; else
  adv "could not read the daemon version"
  fix "needs GARDENIO_TOKEN + BEADS_HTTP_URL; the shim 401s unauthenticated"
  fix "diagnose the whole setup: $(dirname "$0")/gardenio-doctor.sh"
fi

hdr "Skew"
if [ -z "$D_V" ]; then
  adv "unknown — no daemon version to compare against"
elif [ -z "$LOCAL_D" ]; then
  # This is the state every pre-fix onboard-mac.sh laptop is in.
  err "your binaries report '${LOCAL_V:-none}', which is not a calver — skew is UNDETECTABLE"
  fix "the in-CLI stderr warning also cannot fire in this state (SkewCheck needs a calver on both sides)"
  fix "fix by rebuilding with a version stamp: $0 --update"
  rc=1
elif [ "$GAP" -gt "$THRESHOLD_DAYS" ]; then
  err "local $LOCAL_V is ~${GAP} days behind daemon $D_V (threshold ${THRESHOLD_DAYS}d)"
  fix "you may be re-discovering already-fixed bugs — update now: $0 --update"
  rc=1
elif [ "$GAP" -gt 0 ]; then
  ok "local $LOCAL_V is ${GAP}d behind daemon $D_V — within the ${THRESHOLD_DAYS}d threshold"
else
  ok "in sync with the daemon ($LOCAL_V)"
fi

hdr "Summary"
if [ "$rc" -eq 0 ]; then printf '\033[1;32mBinaries are current enough.\033[0m\n'
else printf '\033[1;31mYour local kd/gb need attention — run: %s --update\033[0m\n' "$0"; fi

# --update exits on whether the BUILD+INSTALL landed, not on the report verdict
# above. Reaching here means both binaries were built and renamed into $BIN, so
# the update did what it promised; the report is advisory context for the human
# (PATH shadowing, or a daemon ahead of the main we just pulled). Conflating the
# two would make the background auto-updater cry "update failed" every run for
# anyone with a shadowed PATH — it has a dedicated check for that instead.
if [ "$MODE" = update ]; then exit 0; fi
exit $rc
