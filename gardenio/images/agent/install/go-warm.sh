#!/bin/bash
# go-warm.sh — prewarm GOMODCACHE + GOCACHE with the Go workspace's
# THIRD-PARTY and standard-library packages, so a crew's first scoped
# `go test` on a fresh pod compiles only this repo's own packages
# (gd-j2ABjjrEL0).
#
# Measured on a fresh crew pod before this existed: `go test -run CIWait
# ./cmd/gb/` took 20m7s cold and 2m59s warm, for 0.6s of test. The time was
# compiling client-go & co., which never change between main commits.
#
# Two modes, run in two Dockerfile stages so the expensive one is cacheable:
#
#   go-warm.sh imports <root> <out>
#       <root> holds the full workspace source. Writes <out>: one
#       "<module-dir><TAB><import-path>" line per non-local package that a
#       local package (or its test) imports directly, sorted. Cheap (seconds),
#       and it re-runs on every commit, but its OUTPUT only changes when an
#       import or a go.mod changes.
#
#   go-warm.sh build <root> <imports>
#       <root> holds only go.work and the go.mod/go.sum files. For each module
#       it writes a throwaway package that blank-imports that module's list,
#       and `go test`s it, which compiles every listed package and its whole
#       transitive closure and computes vet facts for all of it, exactly as a
#       crew's `go test` would. BuildKit keys this layer on the <imports>
#       content, so it only re-runs when the dependency set changes.
#
# A blank import of the DIRECT imports is enough: their transitive deps
# (including the internal packages local code cannot import) come with them.
#
# Cache hits on the pod require the SAME toolchain (GOROOT=/usr/local/go, the
# pinned GO_FULL_VERSION, GOTOOLCHAIN=local), the same GOMODCACHE and GOCACHE
# paths, CGO_ENABLED=1 with the same gcc, and default build flags. The
# Dockerfile's go-warm stage sets all of those; see the comment there.
set -euo pipefail

mode="${1:?usage: go-warm.sh imports|build <root> <file>}"
root="${2:?usage: go-warm.sh imports|build <root> <file>}"
file="${3:?usage: go-warm.sh imports|build <root> <file>}"

cd "$root"

case "$mode" in
imports)
  modcache="$(go env GOMODCACHE)"
  out="$(mktemp)"
  # `go list -m` in workspace mode prints every `use`d module.
  for dir in $(go list -m -f '{{.Dir}}'); do
    rel="${dir#"$PWD"/}"
    # Fields: import path, "std" / module dir, the imports to warm. A package
    # matched by ./... contributes its test imports too; a dependency-only
    # package (DepOnly) contributes only what it needs to build.
    (cd "$dir" && go list -e -deps -test -f \
      '{{.ImportPath}}{{"\t"}}{{if .Standard}}std{{else if .Module}}{{.Module.Dir}}{{end}}{{"\t"}}{{join .Imports ","}}{{if not .DepOnly}},{{join .TestImports ","}},{{join .XTestImports ","}}{{end}}' \
      ./...) |
      awk -F'\t' -v mod="$rel" -v modcache="$modcache/" '
        { sub(/ \[.*$/, "", $1) }
        # Local = neither std nor under GOMODCACHE: this repo, or a replace
        # to a directory in it (clicreds, lands/contract).
        $2 == "std" || index($2, modcache) == 1 { next }
        # The generated test main imports testing/internal/testdeps, which
        # no package outside std may import.
        $1 ~ /\.test$/ { next }
        { local[$1] = 1; imps[NR] = $3 }
        END {
          for (i in imps) {
            n = split(imps[i], list, ",")
            for (j = 1; j <= n; j++) {
              p = list[j]; sub(/ \[.*$/, "", p)
              if (p == "" || p == "C" || (p in local)) continue
              print mod "\t" p
            }
          }
        }'
  done | LC_ALL=C sort -u >"$out"
  if [ ! -s "$out" ]; then
    echo "go-warm: found no packages to warm under $root" >&2
    exit 1
  fi
  mv "$out" "$file"
  echo "go-warm: $(wc -l <"$file") imports across $(cut -f1 "$file" | sort -u | wc -l) modules"
  ;;
build)
  cut -f1 "$file" | LC_ALL=C sort -u | while IFS= read -r mod; do
    pkg="$mod/zz_gowarm"
    mkdir -p "$pkg"
    {
      echo "package zzgowarm"
      echo
      echo "import ("
      awk -F'\t' -v mod="$mod" '$1 == mod { printf "\t_ \"%s\"\n", $2 }' "$file"
      echo ")"
    } >"$pkg/warm.go"
    printf 'package zzgowarm\n\nimport "testing"\n\nfunc TestWarm(*testing.T) {}\n' \
      >"$pkg/warm_test.go"
    # -exec /bin/true links the test binary but never runs it: blank-importing
    # everything at once runs every package's init, and two of them may well
    # disagree (a flag registered twice panics).
    echo "go-warm: $mod ($(grep -c '^	_ ' "$pkg/warm.go") imports)"
    (cd "$mod" && go test -count=1 -run '^$' -exec /bin/true ./zz_gowarm/)
    rm -rf "$pkg"
  done
  ;;
*)
  echo "go-warm: unknown mode $mode (want imports or build)" >&2
  exit 2
  ;;
esac
