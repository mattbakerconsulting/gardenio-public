#!/usr/bin/env bash
# Replaced by scripts/onboard.sh (Linux, macOS, WSL). An email argument becomes --operated-by.
case "${1:-}" in *@*) e="$1"; shift; set -- --operated-by "oidc:$e" "$@" ;; esac; exec "$(dirname "${BASH_SOURCE[0]}")/onboard.sh" "$@"
