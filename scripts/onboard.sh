#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────────────
# gardenio onboarding for Linux, macOS and WSL.
#
# Gets the kd and gb CLIs on this machine talking to a gardenio daemon as you.
# Doctor first: it checks everything, shows the plan, and changes nothing until
# you say yes to each step. Run it twice in a row and the second run changes
# nothing.
#
#   scripts/onboard.sh                  # doctor, plan, then ask before each change
#   scripts/onboard.sh --doctor         # read-only report; exit 1 if anything is broken
#   scripts/onboard.sh --yes            # answer yes to every change
#   scripts/onboard.sh --url https://gardenio.example.com/kd --operated-by oidc:you@example.com
#
# The changes it can make, each asked for separately:
#   1. install kd + gb into ~/.local/bin: the release binaries when the latest
#      release carries them (kd-<os>-<arch>.tar.gz + .sha256), otherwise
#      `go build` from a checkout with the calver ldflags (gardenio-version.sh
#      --build), so the version-skew warning works;
#   2. install gb-claude and gardenio-version next to them;
#   3. write the env block (PATH, BEADS_HTTP_URL, BEADS_OPERATED_BY) into your
#      shell rc as a marked block that a rerun replaces in place. It replaces
#      the old onboard-mac.sh block too. No token is ever written;
#   4. merge scripts/claude-hooks.json into ~/.claude/settings.local.json;
#   5. start `gb login`, which you finish in your browser.
# It never prints a token: credentials are reported as "set (length N)".
#
# Not in scope: coop (only needed to run a local agent; releases carry it for
# linux-amd64 only, on macOS build it with `cd coop && cargo build --release`),
# kubectl, helm, and running a daemon.
#
# Environment:
#   GARDENIO_REPO          checkout to build from (default: the one holding this
#                          script, else ~/gardenio)
#   GARDENIO_BIN_DIR       install dir (default ~/.local/bin)
#   GARDENIO_RC            shell rc to write (default: picked from $SHELL)
#   GARDENIO_RELEASE_BASE  where release assets live (default: the GitHub
#                          latest-release download URL; "none" skips it)
# ─────────────────────────────────────────────────────────────────────────────
set -uo pipefail

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN="${GARDENIO_BIN_DIR:-$HOME/.local/bin}"
if [ -n "${GARDENIO_REPO:-}" ]; then REPO="$GARDENIO_REPO"
elif [ -d "$SELF_DIR/../kbeads" ]; then REPO="$(cd "$SELF_DIR/.." && pwd)"
else REPO="$HOME/gardenio"; fi
RELEASE_BASE="${GARDENIO_RELEASE_BASE:-https://github.com/mattbakerconsulting/gardenio/releases/latest/download}"
CC="$HOME/.claude/settings.local.json"
HOOKS_SRC="$REPO/scripts/claude-hooks.json"

BEGIN_MARK="# >>> gardenio env (scripts/onboard.sh) >>>"
END_MARK="# <<< gardenio env (scripts/onboard.sh) <<<"
# The block the old onboard-mac.sh wrote. It exported a GitLab PAT and a
# retired host; writing the new block removes it.
OLD_BEGIN="# >>> gardenio operator env >>>"
OLD_END="# <<< gardenio operator env <<<"

MODE=apply; YES=""; URL_ARG=""; OPBY_ARG=""
while [ $# -gt 0 ]; do
  case "$1" in
    --doctor|-d) MODE=doctor ;;
    --yes|-y) YES=1 ;;
    --url) URL_ARG="${2:-}"; shift ;;
    --url=*) URL_ARG="${1#--url=}" ;;
    --operated-by) OPBY_ARG="${2:-}"; shift ;;
    --operated-by=*) OPBY_ARG="${1#--operated-by=}" ;;
    --help|-h) sed -n '2,39p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) printf 'unknown argument: %s (try --help)\n' "$1" >&2; exit 2 ;;
  esac
  shift
done

ok()   { printf '  \033[1;32m✅\033[0m %s\n' "$*"; }
adv()  { printf '  \033[1;33m⚠️ \033[0m %s\n' "$*"; warns=$((warns+1)); }
err()  { printf '  \033[1;31m❌ %s\033[0m\n' "$*"; bad=$((bad+1)); }
fix()  { printf '       \033[2m↳ %s\033[0m\n' "$*"; }
hdr()  { printf '\n\033[1m%s\033[0m\n' "$*"; }
say()  { printf '\033[1;36m==>\033[0m %s\n' "$*"; }

# ── platform ─────────────────────────────────────────────────────────────────
KERNEL="$(uname -s 2>/dev/null || echo unknown)"
WSL=""
case "$KERNEL" in
  Linux)  OS=linux
          grep -qi microsoft "${GARDENIO_PROC_VERSION:-/proc/version}" 2>/dev/null && WSL=1 ;;
  Darwin) OS=darwin ;;
  *)      OS="" ;;
esac
case "$(uname -m 2>/dev/null)" in
  x86_64|amd64)  ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *)             ARCH="" ;;
esac

# The rc an interactive shell reads. macOS terminals start bash as a login
# shell, which reads ~/.bash_profile, not ~/.bashrc.
if [ -n "${GARDENIO_RC:-}" ]; then RC="$GARDENIO_RC"
else
  case "$(basename "${SHELL:-}")" in
    zsh)  RC="${ZDOTDIR:-$HOME}/.zshrc" ;;
    bash) if [ "$OS" = darwin ]; then RC="$HOME/.bash_profile"; else RC="$HOME/.bashrc"; fi ;;
    fish) RC="" ;;   # not POSIX syntax: the block is printed for you to add by hand
    *)    RC="$HOME/.profile" ;;
  esac
fi

# ── helpers ──────────────────────────────────────────────────────────────────
have() { command -v "$1" >/dev/null 2>&1; }

# with_timeout <secs> <cmd...>: macOS has no timeout(1) unless coreutils is in.
with_timeout() {
  local s="$1"; shift
  if have timeout; then timeout "$s" "$@"
  elif have gtimeout; then gtimeout "$s" "$@"
  else "$@"; fi
}

# is_calver <version>: true for YYYY.DDD[.N], the stamp the skew check needs.
is_calver() { grep -qE '^20[0-9]{2}\.[0-9]{1,3}([.+-]|$)' <<<"${1#v}"; }

tty_ok() { { : </dev/tty; } 2>/dev/null; }

# ask <question>: yes/no on the terminal, default no. --yes answers yes.
ask() {
  [ -n "$YES" ] && return 0
  tty_ok || return 1
  local a
  printf '\033[1m?\033[0m %s [y/N] ' "$1" >/dev/tty
  read -r a </dev/tty || return 1
  case "$a" in y|Y|yes|YES|Yes) return 0 ;; *) return 1 ;; esac
}

# prompt <question> <default>: free text on the terminal; prints the answer.
prompt() {
  tty_ok || { printf '%s' "$2"; return 0; }
  local a
  printf '\033[1m?\033[0m %s%s: ' "$1" "${2:+ [$2]}" >/dev/tty
  read -r a </dev/tty || a=""
  printf '%s' "${a:-$2}"
}

# block_of <file>: the current gardenio block (new or old marker), or nothing.
block_of() {
  [ -f "$1" ] || return 0
  awk -v b="$BEGIN_MARK" -v e="$END_MARK" -v ob="$OLD_BEGIN" -v oe="$OLD_END" '
    $0==b || $0==ob {f=1} f {print} $0==e || $0==oe {f=0}' "$1"
}

# block_value <VAR>: the value the rc block exports for VAR.
block_value() {
  block_of "$RC" | sed -n "s/^export $1='\\(.*\\)'\$/\\1/p" | head -1
}

# render_block <url> <operated-by>: the block this script writes. POSIX sh,
# so bash and zsh both read it. It holds no token.
render_block() {
  local bin_expr="$BIN"
  case "$BIN" in "$HOME"/*) bin_expr="\$HOME${BIN#"$HOME"}" ;; esac
  cat <<EOF
$BEGIN_MARK
# Written by gardenio's scripts/onboard.sh; a rerun replaces this block in
# place. No token lives here: sign in with \`gb login\`.
case ":\$PATH:" in *":$bin_expr:"*) ;; *) export PATH="$bin_expr:\$PATH" ;; esac
export BEADS_HTTP_URL='$1'
export BEADS_OPERATED_BY='$2'
# claude runs primed with the gardenio workflow (\`command claude\` for vanilla).
command -v gb-claude >/dev/null 2>&1 && alias claude=gb-claude
# Once a day: warn (and rebuild) when kd/gb fall behind the daemon.
command -v gardenio-version >/dev/null 2>&1 && gardenio-version --nudge 2>/dev/null
$END_MARK
EOF
}

# write_block <file> <blockfile>: replace the gardenio block in place, or
# append it. Rewrites through `cat >` so the rc keeps its inode, mode and any
# symlink.
write_block() {
  local rc="$1" blk="$2" tmp
  # A begin marker with no end would swallow the rest of the rc; refuse.
  local nb ne
  nb="$(grep -cxF -e "$BEGIN_MARK" -e "$OLD_BEGIN" "$rc" 2>/dev/null)"
  ne="$(grep -cxF -e "$END_MARK" -e "$OLD_END" "$rc" 2>/dev/null)"
  [ "${nb:-0}" = "${ne:-0}" ] || { err "$rc has an unmatched gardenio marker; fix it by hand"; return 1; }
  tmp="$(mktemp "${TMPDIR:-/tmp}/gardenio-rc.XXXXXX")" || return 1
  if [ -n "$(block_of "$rc")" ]; then
    awk -v b="$BEGIN_MARK" -v e="$END_MARK" -v ob="$OLD_BEGIN" -v oe="$OLD_END" '
      NR==FNR {blk = blk $0 "\n"; next}
      $0==b || $0==ob { if (!done) {printf "%s", blk; done=1}; skip=1; next }
      skip { if ($0==e || $0==oe) skip=0; next }
      {print}' "$blk" "$rc" >"$tmp"
  else
    { [ -s "$rc" ] && cat "$rc" && echo; cat "$blk"; } >"$tmp"
  fi
  cat "$tmp" >"$rc"; local rc_status=$?
  rm -f "$tmp"
  return $rc_status
}

token_var() {
  local v
  for v in GARDENIO_TOKEN KBEADS_TOKEN BEADS_AUTH_TOKEN BEADS_DAEMON_TOKEN; do
    [ -n "${!v:-}" ] && { echo "$v"; return 0; }
  done
}

# login_state <url>: "ok <daemon> <who>", "other <daemon>", or "none" for the
# gb login session (gb login --status prints no token).
login_state() {
  local st who daemon
  st="$(with_timeout 20 gb login --status --http-url "$1" 2>/dev/null)"
  who="$(printf '%s\n' "$st" | sed -n 's/^Session: *signed in *//p' | head -1)"
  daemon="$(printf '%s\n' "$st" | sed -n 's/^Daemon: *//p' | head -1)"
  if [ -z "$who$daemon" ]; then echo none
  elif grep -q 'not the current --http-url host' <<<"$st"; then echo "other $daemon"
  else echo "ok $daemon $who"; fi
}

hooks_wired() {
  [ -f "$CC" ] && have python3 && python3 - "$CC" <<'PY' 2>/dev/null
import json,sys
h=json.load(open(sys.argv[1])).get("hooks",{})
def cmds(ev): return [k.get("command","") for g in h.get(ev,[]) for k in g.get("hooks",[])]
sys.exit(0 if any("gb hook prime" in c for c in cmds("SessionStart")) and any("gb hook stop-gate" in c for c in cmds("Stop")) else 1)
PY
}

release_asset_url() { echo "$RELEASE_BASE/$1-$OS-$ARCH.tar.gz"; }
release_has() {
  [ "$RELEASE_BASE" != none ] && [ -n "$OS" ] && [ -n "$ARCH" ] && have curl || return 1
  curl -fsIL --max-time 10 -o /dev/null "$(release_asset_url "$1")" 2>/dev/null
}

# ── doctor ───────────────────────────────────────────────────────────────────
# Read-only. Sets the NEED_* flags the plan is built from.
doctor() {
  bad=0; warns=0
  NEED_BIN=""; NEED_HELPERS=""; NEED_ENV=""; NEED_HOOKS=""; NEED_LOGIN=""

  hdr "Platform"
  if [ -z "$OS" ]; then err "unsupported OS '$KERNEL' (Linux, macOS and WSL are supported)"
  elif [ -n "$WSL" ]; then ok "WSL (Linux $(uname -r 2>/dev/null)) ${ARCH:-$(uname -m)}"
  elif [ "$OS" = darwin ]; then ok "macOS $(sw_vers -productVersion 2>/dev/null) ${ARCH:-$(uname -m)}"
  else ok "Linux $(uname -r 2>/dev/null) ${ARCH:-$(uname -m)}"; fi
  [ -n "$OS" ] && [ -z "$ARCH" ] && adv "unrecognised CPU $(uname -m): no release binaries, build from source"
  if [ -n "$WSL" ]; then
    case "$REPO" in /mnt/*) adv "checkout $REPO is on the Windows drive: builds are slow and CRLF checkouts break scripts"
                            fix "clone inside WSL instead, e.g. git clone https://github.com/mattbakerconsulting/gardenio.git ~/gardenio" ;; esac
  fi

  hdr "Tools"
  local kdv="" gbv="" b v p
  for b in kd gb; do
    if have "$b"; then
      p="$(command -v "$b")"; v="$("$b" --version 2>/dev/null | head -1 | awk '{print $NF}')"
      [ "$b" = kd ] && kdv="$v" || gbv="$v"
      if is_calver "$v"; then ok "$b $v ($p)"
      else adv "$b reports '${v:-nothing}', not a calver: the version-skew warning cannot fire"; NEED_BIN=1; fi
    else err "$b not found on PATH"; NEED_BIN=1; fi
  done
  if [ -n "$kdv" ] && [ -n "$gbv" ] && [ "$kdv" != "$gbv" ]; then
    adv "kd and gb are from different builds ($kdv vs $gbv)"; NEED_BIN=1
  fi
  for b in coop claude git; do
    if have "$b"; then ok "$b $("$b" --version 2>/dev/null | head -1)"
    else
      case "$b" in
        coop)   adv "coop not found: needed only to run a local agent (gb agent start --local)"
                fix "build it: cd $REPO/coop && cargo build --release, then copy target/release/coop into $BIN" ;;
        claude) adv "claude not found: needed to run agents"; fix "install Claude Code: https://claude.com/claude-code" ;;
        git)    adv "git not found"; fix "install git from your package manager" ;;
      esac
    fi
  done
  for b in gb-claude gardenio-version; do
    local src="$REPO/scripts/$b"; [ "$b" = gardenio-version ] && src="$REPO/scripts/gardenio-version.sh"
    if [ -f "$src" ] && ! cmp -s "$src" "$BIN/$b"; then
      if [ -e "$BIN/$b" ]; then adv "$b in $BIN differs from $src"; else adv "$b not installed"; fi
      NEED_HELPERS=1
    elif [ -x "$BIN/$b" ] || have "$b"; then ok "$b installed"
    else adv "$b not installed (no checkout at $REPO to install it from)"; fi
  done
  case ":$PATH:" in *":$BIN:"*) ok "$BIN is on PATH" ;;
    *) adv "$BIN is not on PATH in this shell"; [ -n "$(block_of "$RC")" ] || NEED_ENV=1 ;; esac

  hdr "Environment"
  local blk_url blk_op
  blk_url="$(block_value BEADS_HTTP_URL)"; blk_op="$(block_value BEADS_OPERATED_BY)"
  if [ -n "${BEADS_HTTP_URL:-}" ]; then ok "BEADS_HTTP_URL=$BEADS_HTTP_URL"
  elif [ -n "$blk_url" ]; then
    adv "BEADS_HTTP_URL is set in $RC but not in this shell"; fix "open a new shell, or: . $RC"
    export BEADS_HTTP_URL="$blk_url"
  else err "BEADS_HTTP_URL unset: kd and gb do not know which daemon to use"; NEED_ENV=1; fi
  if [ -n "${BEADS_OPERATED_BY:-}" ]; then ok "BEADS_OPERATED_BY=$BEADS_OPERATED_BY"
  elif [ -n "$blk_op" ]; then
    adv "BEADS_OPERATED_BY is set in $RC but not in this shell"; fix "open a new shell, or: . $RC"
    export BEADS_OPERATED_BY="$blk_op"
  else err "BEADS_OPERATED_BY unset: your writes are not attributed to you"; NEED_ENV=1; fi
  if [ -z "$RC" ]; then
    adv "your shell is fish: add the env block to config.fish by hand (the plan prints it)"
  else
    local cur want
    cur="$(block_of "$RC")"
    if grep -qxF "$OLD_BEGIN" "$RC" 2>/dev/null; then
      adv "$RC has the old onboard-mac.sh block (a GitLab PAT and a retired host)"; NEED_ENV=1
    elif [ -z "$cur" ]; then
      adv "$RC has no gardenio block"; NEED_ENV=1
    else
      want="$(render_block "${URL_ARG:-${BEADS_HTTP_URL:-}}" "${OPBY_ARG:-${BEADS_OPERATED_BY:-}}")"
      if [ "$cur" = "$want" ]; then ok "$RC has the current gardenio block"
      else adv "the gardenio block in $RC is out of date"; NEED_ENV=1; fi
    fi
  fi

  hdr "Auth"
  local tv; tv="$(token_var)"
  if [ -n "$tv" ]; then local tval="${!tv}"; ok "bearer in \$$tv (set, length ${#tval})"; fi
  if have gb && [ -n "${BEADS_HTTP_URL:-}" ]; then
    local ls; ls="$(login_state "$BEADS_HTTP_URL")"
    case "$ls" in
      ok*)    ok "gb login session for ${ls#ok }" ;;
      other*) adv "gb login session is for ${ls#other }, not $(printf '%s' "$BEADS_HTTP_URL" | sed -E 's#^[a-z]+://##; s#[/:?].*$##')"; NEED_LOGIN=1 ;;
      *)      if [ -z "$tv" ]; then err "not signed in, and no bearer token in the environment"; NEED_LOGIN=1
              else adv "no gb login session (using the bearer in \$$tv)"; fi ;;
    esac
  elif [ -z "$tv" ]; then err "not signed in (no gb, or no daemon URL yet), and no bearer token"; NEED_LOGIN=1; fi

  hdr "Daemon"
  if have gb && [ -n "${BEADS_HTTP_URL:-}" ]; then
    local out
    if out="$(with_timeout 30 gb ping --http-url "$BEADS_HTTP_URL" 2>&1)"; then ok "gb ping reached $BEADS_HTTP_URL"
    else
      err "gb ping could not reach $BEADS_HTTP_URL"
      fix "$(printf '%s' "$out" | head -1)"
      fix "check the URL; a 401 means sign in again (gb login); a hang means the network path or a VPN"
    fi
    local name
    name="$(with_timeout 30 gb prime --system-prompt --no-advice --http-url "$BEADS_HTTP_URL" 2>/dev/null \
            | sed -n 's/^- \*\*Agent\*\*: *//p' | head -1)"
    case "$(printf '%s' "$name" | tr '[:upper:]' '[:lower:]')" in
      ""|gardenio|unknown) adv "gb prime names '${name:-nobody}', not you"
                           fix "set your git user.name, or KD_ACTOR, to the name you want on the roster" ;;
      *) ok "gb prime names '$name'" ;;
    esac
    if [ -x "$REPO/scripts/gardenio-version.sh" ] && is_calver "$kdv"; then
      # Needs a bearer in the env to read the daemon's version; silent without one.
      local skew
      if skew="$(with_timeout 30 "$REPO/scripts/gardenio-version.sh" --quiet 2>/dev/null)"; then
        case "$skew" in *unknown*) ;; *) ok "${skew#gardenio: }" ;; esac
      else adv "${skew#gardenio: }"; fix "pull and rebuild: $REPO/scripts/gardenio-version.sh --update"; fi
    fi
  else adv "skipped the daemon checks (need gb and BEADS_HTTP_URL)"; fi

  hdr "Claude Code"
  if ! have claude; then adv "skipped (claude not installed)"
  elif hooks_wired; then ok "gb hooks wired in $CC"
  elif [ -f "$HOOKS_SRC" ] && have python3; then adv "gb hooks not wired in $CC"; NEED_HOOKS=1
  else adv "gb hooks not wired in $CC"; fix "merge the 'hooks' key of scripts/claude-hooks.json into $CC"; fi

  hdr "Summary"
  if [ $bad -eq 0 ] && [ $warns -eq 0 ]; then printf '\033[1;32mAll green.\033[0m\n'
  elif [ $bad -eq 0 ]; then printf '\033[1;33m%d advisory note(s), nothing broken.\033[0m\n' "$warns"
  else printf '\033[1;31m%d broken, %d advisory.\033[0m\n' "$bad" "$warns"; fi
}

# ── changes ──────────────────────────────────────────────────────────────────
install_cli() {
  mkdir -p "$BIN" || return 1
  if [ -n "$CAN_RELEASE" ]; then
    local tmp b sum want
    tmp="$(mktemp -d "${TMPDIR:-/tmp}/gardenio-bin.XXXXXX")" || return 1
    for b in kd gb; do
      say "downloading $(release_asset_url "$b")"
      if ! curl -fsSL --max-time 120 -o "$tmp/$b.tgz" "$(release_asset_url "$b")" \
         || ! curl -fsSL --max-time 30 -o "$tmp/$b.sha" "$(release_asset_url "$b").sha256"; then
        err "downloading $b failed"; rm -rf "$tmp"; return 1
      fi
      want="$(awk '{print $1; exit}' "$tmp/$b.sha")"
      if have sha256sum; then sum="$(sha256sum "$tmp/$b.tgz" | awk '{print $1}')"
      else sum="$(shasum -a 256 "$tmp/$b.tgz" | awk '{print $1}')"; fi
      if [ -z "$want" ] || [ "$sum" != "$want" ]; then err "$b checksum mismatch"; rm -rf "$tmp"; return 1; fi
      if ! tar -xzf "$tmp/$b.tgz" -C "$tmp" "$b" || ! "$tmp/$b" --version >/dev/null 2>&1; then
        err "$b from the release does not run here"; rm -rf "$tmp"; return 1
      fi
    done
    # Both verified before either is swapped in; rename keeps running copies intact.
    for b in kd gb; do
      if ! { cp "$tmp/$b" "$BIN/.$b.new.$$" && chmod 0755 "$BIN/.$b.new.$$" && mv -f "$BIN/.$b.new.$$" "$BIN/$b"; }; then
        rm -rf "$tmp"; return 1
      fi
    done
    rm -rf "$tmp"
    ok "installed release kd + gb into $BIN"
  else
    GARDENIO_REPO="$REPO" GARDENIO_BIN_DIR="$BIN" "$REPO/scripts/gardenio-version.sh" --build || return 1
  fi
}

install_helpers() {
  mkdir -p "$BIN" || return 1
  [ -f "$REPO/scripts/gb-claude" ] && { cmp -s "$REPO/scripts/gb-claude" "$BIN/gb-claude" \
    || install -m 0755 "$REPO/scripts/gb-claude" "$BIN/gb-claude"; }
  [ -f "$REPO/scripts/gardenio-version.sh" ] && { cmp -s "$REPO/scripts/gardenio-version.sh" "$BIN/gardenio-version" \
    || install -m 0755 "$REPO/scripts/gardenio-version.sh" "$BIN/gardenio-version"; }
  ok "installed gb-claude and gardenio-version into $BIN"
}

merge_hooks() {
  mkdir -p "$(dirname "$CC")" || return 1
  [ -f "$CC" ] || echo '{}' >"$CC"
  python3 - "$CC" "$HOOKS_SRC" <<'PY'
import json, sys
settings_path, hooks_path = sys.argv[1], sys.argv[2]
with open(settings_path) as f: settings = json.load(f)
with open(hooks_path) as f: hooks = json.load(f)["hooks"]
settings["hooks"] = hooks  # gb owns the hooks block wholesale
with open(settings_path, "w") as f: json.dump(settings, f, indent=2)
print("  merged %d hook events" % len(hooks))
PY
}

# valid_value <s>: safe inside the single quotes of the rc block.
valid_value() { [ -n "$1" ] && case "$1" in *"'"*|*\\*|*' '*) return 1 ;; esac; }

# ── main ─────────────────────────────────────────────────────────────────────
printf '\033[1mgardenio onboarding\033[0m (%s)\n' "$( [ "$MODE" = doctor ] && echo "doctor, read-only" || echo "doctor first, then asks before each change")"
doctor

if [ "$MODE" = doctor ]; then [ $bad -eq 0 ]; exit $?; fi

# Settle the env values before the plan, so the plan shows exactly what it writes.
WANT_URL="${URL_ARG:-${BEADS_HTTP_URL:-}}"
WANT_OPBY="${OPBY_ARG:-${BEADS_OPERATED_BY:-}}"
if [ -n "$NEED_ENV" ]; then
  if [ -z "$WANT_URL" ]; then
    WANT_URL="$(prompt "Daemon URL (ask whoever runs your gardenio, e.g. https://gardenio.example.com/kd)" "")"
  fi
  if [ -z "$WANT_OPBY" ]; then
    email="$(git config --global user.email 2>/dev/null || true)"
    WANT_OPBY="$(prompt "Who your writes are attributed to (oidc:<email> or slack:U...)" "${email:+oidc:$email}")"
  fi
  case "$WANT_URL" in http://*|https://*) ;; *) [ -n "$WANT_URL" ] && err "daemon URL must start with http:// or https://"; WANT_URL="" ;; esac
  valid_value "$WANT_URL" || WANT_URL=""
  valid_value "$WANT_OPBY" || WANT_OPBY=""
  # When the block is current the doctor only flagged the shell, not the rc.
  if [ -n "$RC" ] && [ -n "$WANT_URL" ] && [ -n "$WANT_OPBY" ] \
     && [ "$(block_of "$RC")" = "$(render_block "$WANT_URL" "$WANT_OPBY")" ]; then NEED_ENV=""; fi
fi

# The doctor could only check the session for a URL it knew.
if [ -n "$NEED_LOGIN" ] && [ -z "${BEADS_HTTP_URL:-}" ] && [ -n "$WANT_URL" ] && have gb \
   && [ "$(login_state "$WANT_URL")" != none ]; then NEED_LOGIN=""; fi

CAN_RELEASE=""; [ -n "$NEED_BIN" ] && release_has kd && release_has gb && CAN_RELEASE=1
hdr "Plan"
n=0
if [ -n "$NEED_BIN" ]; then
  if [ -n "$CAN_RELEASE" ]; then n=$((n+1)); echo "  $n. install kd + gb into $BIN from the latest release ($OS-$ARCH)"
  elif [ -d "$REPO/kbeads" ] && have go; then n=$((n+1)); echo "  $n. build kd + gb from $REPO with go and install them into $BIN"
  else
    echo "  -  cannot install kd + gb: no release binary for ${OS:-?}-${ARCH:-?}, and $( [ -d "$REPO/kbeads" ] && echo "no Go toolchain" || echo "no checkout at $REPO")"
    echo "     install Go (https://go.dev/dl/) and clone https://github.com/mattbakerconsulting/gardenio.git, then rerun"
    NEED_BIN=""
  fi
fi
[ -n "$NEED_HELPERS" ] && { n=$((n+1)); echo "  $n. install gb-claude and gardenio-version from $REPO/scripts into $BIN"; }
if [ -n "$NEED_ENV" ]; then
  if [ -z "$WANT_URL" ] || [ -z "$WANT_OPBY" ]; then
    echo "  -  cannot write the env block: need a daemon URL and an operated-by id (rerun with --url and --operated-by)"
    NEED_ENV=""
  elif [ -z "$RC" ]; then
    echo "  -  add this to ~/.config/fish/config.fish by hand:"
    echo "       fish_add_path $BIN; set -gx BEADS_HTTP_URL '$WANT_URL'; set -gx BEADS_OPERATED_BY '$WANT_OPBY'"
    NEED_ENV=""
  else
    n=$((n+1)); echo "  $n. write the gardenio block into $RC (replaces any older one in place; no token):"
    render_block "$WANT_URL" "$WANT_OPBY" | sed 's/^/       /'
    grep -qxF "$OLD_BEGIN" "$RC" 2>/dev/null && \
      echo "     this removes the old block's GitLab PAT from $RC; revoke that PAT if nothing else uses it"
  fi
fi
[ -n "$NEED_HOOKS" ] && { n=$((n+1)); echo "  $n. merge the gb hooks from $HOOKS_SRC into $CC (replaces its 'hooks' key)"; }
[ -n "$NEED_LOGIN" ] && { n=$((n+1)); echo "  $n. run gb login for ${WANT_URL:-the daemon}: you finish it in your browser"; }

if [ $n -eq 0 ]; then
  echo "  nothing to change."
  [ $bad -eq 0 ]; exit $?
fi
if [ -z "$YES" ] && ! tty_ok; then
  echo; echo "No terminal to ask on, so nothing was changed. Rerun in a terminal, or with --yes."
  exit 1
fi

hdr "Changes"
changed=""
if [ -n "$NEED_BIN" ] && ask "Install kd + gb into $BIN?"; then
  if install_cli; then
    changed=1; case ":$PATH:" in *":$BIN:"*) ;; *) PATH="$BIN:$PATH" ;; esac
  else err "installing kd + gb failed (see above)"; fi
fi
if [ -n "$NEED_HELPERS" ] && ask "Install gb-claude and gardenio-version into $BIN?"; then
  if install_helpers; then changed=1; else err "installing the helpers failed"; fi
fi
if [ -n "$NEED_ENV" ] && ask "Write the gardenio block into $RC?"; then
  blkf="$(mktemp "${TMPDIR:-/tmp}/gardenio-blk.XXXXXX")"
  render_block "$WANT_URL" "$WANT_OPBY" >"$blkf"
  if touch "$RC" && write_block "$RC" "$blkf"; then ok "wrote the gardenio block into $RC"; changed=1
  else err "could not write $RC"; fi
  rm -f "$blkf"
  export BEADS_HTTP_URL="$WANT_URL" BEADS_OPERATED_BY="$WANT_OPBY"
  case ":$PATH:" in *":$BIN:"*) ;; *) PATH="$BIN:$PATH" ;; esac
fi
if [ -n "$NEED_HOOKS" ] && ask "Merge the gb hooks into $CC?"; then
  if merge_hooks; then changed=1; else err "merging the hooks failed"; fi
fi
if [ -n "$NEED_LOGIN" ] && have gb && [ -n "${BEADS_HTTP_URL:-}" ] && ask "Start gb login now?"; then
  # Headless Linux has no browser to open; gb then only prints the link.
  login_args=(--http-url "$BEADS_HTTP_URL")
  if [ "$OS" = linux ] && [ -z "$WSL" ] && [ -z "${DISPLAY:-}${WAYLAND_DISPLAY:-}" ]; then login_args+=(--no-browser); fi
  say "gb login: open the link it prints, check the code matches, and sign in"
  if gb login "${login_args[@]}"; then changed=1; else err "gb login did not finish"; fi
fi

if [ -n "$changed" ]; then
  hdr "Checking again"
  doctor
  echo
  [ -n "$RC" ] && echo "Open a new shell (or run: . $RC) so it picks up the block."
fi
echo "Next: kd list --limit 2 and gb ready. Run $0 --doctor any time something breaks."
[ $bad -eq 0 ]
