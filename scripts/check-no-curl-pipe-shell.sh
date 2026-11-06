#!/usr/bin/env bash
# Ban `curl … | sh` / `wget … | bash`: a downloaded script must be written to a
# file, proven non-empty, and only then executed.
#
# WHY THIS PIPELINE IS DIFFERENT FROM A MERELY UGLY ONE. It cannot report
# failure. On a transient fetch error curl writes nothing to the pipe, `sh`
# reads EOF, and `sh` exits 0 -- so the install "succeeds". The job then dies
# minutes later at exit 127 on the first use of the thing that was supposedly
# installed, having already held a runner through everything before it. On the
# CI pool that is a whole heavy node per occurrence plus the rerun it provokes:
# 2 of the 6 failures in the last 100 ci.yml runs were exactly this signature
# (exit 127 in `rustup`). See bead gd-ahaGQcSZFp.
#
# `set -o pipefail` IS NOT A FIX and must not be accepted as one in review.
# With pipefail a hard curl error does fail the step, but a TRUNCATED body
# still exits curl 0, and `sh` executes the partial script -- a half-installed
# toolchain that then fails somewhere unrelated. Only the three-step form is
# correct:
#
#     init="$(mktemp)"
#     curl -sSfL --retry 5 --retry-delay 2 --retry-connrefused \
#          --max-time 180 -o "$init" https://example.invalid/install.sh
#     [ -s "$init" ] || { echo "::error::installer downloaded empty"; exit 1; }
#     sh "$init" -y …
#     rm -f "$init"
#
# THE POLICY IS NOT NEW -- ENFORCEMENT IS. gardenio/images/agent/install/
# already states it (clis.sh:8 "downloads without a pinned checksum -- no
# `curl | sh`, no `curl … | tar`") and clis.sh:116, clis.sh:138 and
# go-tools.sh:28 each record having replaced such a pipeline with a pinned
# tarball. It was applied deliberately in the agent image and then stopped at
# that directory's edge, because nothing looked for the pattern anywhere else.
# Three CI workflows had quietly drifted back to it by 2026-09-22.
#
# Scope note: `curl … | tar` is banned by that same comment but is NOT checked
# here, deliberately. tar errors on a truncated or empty archive, so it fails
# loudly; `sh` is the uniquely silent case. Widening this guard to tar is a
# separate decision with its own allowlist.

set -euo pipefail

# Only the tree scan needs the repo root; explicit-file mode is given absolute
# paths by its caller and must work from a temp dir.
if [ "$#" -eq 0 ]; then
  cd "$(git rev-parse --show-toplevel)"
fi

# Paths where a violation is currently tolerated. EVERY entry is a known defect,
# not an approved exception -- the list exists so that landing the guard did not
# require fixing node bootstrap in the same change. Burn them down; do not grow
# the list without saying why on gd-ahaGQcSZFp.
#
# A stale entry is an error too: see the second pass below.
allowlist() {
  cat <<'EOF'
gardenio/images/ci-runner/Dockerfile|helm get-helm-3; image build, not a CI slot
gardenio/images/coop-claude/Dockerfile|claude install.sh; image build, not a CI slot
scripts/bootstrap-ec2.sh|get.k3s.io + get-helm-3; runs on NODE JOIN -- fix needs a real node-join test, not a CI run
gardenio/terraform/modules/hq/files/hq-bootstrap.sh|nodesource + claude install; HQ host bootstrap, same untestable-in-CI problem
gardenio/terraform/modules/k3s/templates/node.sh.tftpl|get.k3s.io x3 via `| ENV=v sh`; runs on NODE JOIN, same untestable-in-CI problem as scripts/bootstrap-ec2.sh
EOF
}

# Here-string, NOT `allowlist | grep -q`. grep -q exits at the first match
# without draining stdin, the producer takes EPIPE, and `pipefail` promotes
# that to the pipeline's status -- so the allowlist would intermittently read
# as empty and this guard would red on its own exceptions. That is the exact
# class scripts/check-sigpipe-producers.py exists to keep out of this repo.
is_allowlisted() { grep -q "^$1|" <<<"$(allowlist)"; }

# Tracked files only, and only where an installer plausibly runs. docs/archive/
# is the decommissioned RWX tree -- it matches in a dozen places and is dead
# code, so scanning it would force an allowlist longer than the real one.
#
# gardenio/helm/ added 2026-09-23. The ci runner values files embed ~80 lines
# of shell that runs as ROOT in an init container on every CI node, and this
# guard could not see one line of it. Found by planting the literal pattern
# in arc-runners-values.yaml and watching the guard stay green -- a POSITIVE
# CONTROL, the same way the node.sh.tftpl miss was found. Planting it in a
# COMMENT is not that control: comments are dropped below, so a comment probe
# reads identical whether the file is scanned or not, and the first attempt
# at this control was wrong in exactly that way.
candidates() {
  git ls-files -z \
    '.github/workflows/*' 'scripts/*' 'gardenio/images/*' 'gardenio/terraform/*' \
    'gardenio/helm/*' \
  | tr '\0' '\n' \
  | grep -v '^docs/archive/' \
  | grep -vE '_test\.(sh|py|bash)$' \
  | grep -vE '^scripts/check-no-curl-pipe-shell\.sh$'
}

# Join backslash continuations before matching, so splitting the pipe across
# two lines does not evade the check. Comment lines are dropped AFTER joining
# (a `#` line cannot be continued into code). Reports the line number of the
# START of the joined logical line.
violations_in() {
  awk '
    { raw[NR] = $0 }
    END {
      n = NR
      for (i = 1; i <= n; i++) {
        line = raw[i]; start = i
        while (line ~ /\\[ \t]*$/ && i < n) {
          sub(/\\[ \t]*$/, "", line)
          i++
          s = raw[i]; sub(/^[ \t]+/, "", s)
          line = line s
        }
        t = line; sub(/^[ \t]+/, "", t)
        if (t ~ /^#/) continue
        # Everything that can legally sit between the pipe and the shell has to
        # be skipped over, or the pipeline evades the check while behaving
        # identically. `curl … | FOO=1 BAR=2 sh -s - agent` is the same
        # silent-EOF bug as `curl … | sh`, and it is the idiom get.k3s.io and
        # rustup actually print, so it is the form most likely to be copied in.
        # Without the assignment run below, this guard read clean over three
        # such pipelines in gardenio/terraform/modules/k3s/templates/
        # node.sh.tftpl (found 2026-09-22 by running the guard against a file
        # known to contain the pattern -- a POSITIVE CONTROL. A tree scan could
        # not have found it: it reports the same rc=0 whether it looked or not).
        #
        # `sudo` is accepted on either side of the assignments because both
        # orders occur in the wild, and with an optional flag run (`sudo -E`)
        # because that is the form people reach for precisely when they have
        # environment to pass. None of these widen the match to a non-shell
        # command: the token after them must still be exactly `sh` or `bash`,
        # which is what keeps `| sha512sum` and `| tee -a x.sh` clean.
        if (t ~ /(curl|wget)[^|]*\|[ \t]*(sudo[ \t]+(-[A-Za-z]+[ \t]+)*)?([A-Za-z_][A-Za-z0-9_]*=[^ \t]*[ \t]+)*(sudo[ \t]+(-[A-Za-z]+[ \t]+)*)?(ba)?sh([ \t]|$)/) {
          printf "%d:%s\n", start, t
        }
      }
    }
  ' "$1"
}

rc=0
declare -a hit_paths=()

# Explicit-file mode: scan exactly the files named and report every violation,
# with no allowlist and no stale-entry pass. This is what the self-test drives,
# so the tests never depend on the real git index -- same convention as
# check-merge-conflict-markers.sh.
if [ "$#" -gt 0 ]; then
  for f in "$@"; do
    out="$(violations_in "$f")" || true
    [ -n "$out" ] || continue
    while IFS= read -r v; do printf '%s:%s\n' "$f" "$v"; done <<<"$out"
    rc=1
  done
  exit "$rc"
fi

while IFS= read -r f; do
  [ -f "$f" ] || continue
  out="$(violations_in "$f")" || true
  [ -n "$out" ] || continue
  hit_paths+=("$f")
  if is_allowlisted "$f"; then
    continue
  fi
  while IFS= read -r v; do
    printf '%s:%s\n' "$f" "$v"
  done <<<"$out"
  rc=1
done < <(candidates)

if [ "$rc" -ne 0 ]; then
  cat >&2 <<'EOF'

A downloaded installer is being piped straight into a shell.

That pipeline cannot fail correctly: on a transient fetch error curl writes
nothing, `sh` reads EOF and exits 0, the install reports success, and the job
dies later at exit 127 -- after holding a runner for the whole prologue.
Adding `set -o pipefail` does NOT fix it; a truncated body is still executed.

Write it to a file, assert it is non-empty, then run it:

    init="$(mktemp)"
    curl -sSfL --retry 5 --retry-delay 2 --retry-connrefused \
         --max-time 180 -o "$init" <url>
    [ -s "$init" ] || { echo "::error::installer downloaded empty"; exit 1; }
    sh "$init" <args>
    rm -f "$init"

Background and the remaining known sites: bead gd-ahaGQcSZFp.
EOF
fi

# Second pass: an allowlist entry that no longer matches anything is itself a
# failure. Otherwise the list silently outlives the defect it excused and the
# next reader cannot tell which entries are real. Fixing a site means deleting
# its line here in the same commit.
while IFS='|' read -r path _reason; do
  [ -n "$path" ] || continue
  found=0
  for h in ${hit_paths[@]+"${hit_paths[@]}"}; do
    [ "$h" = "$path" ] && found=1 && break
  done
  if [ "$found" -eq 0 ]; then
    echo "stale allowlist entry: $path no longer contains a curl|sh pipeline -- delete this line" >&2
    rc=1
  fi
done < <(allowlist)

exit "$rc"
