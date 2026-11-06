#!/usr/bin/env bash
# gardenio doctor: the read-only half of scripts/onboard.sh. Diagnoses the kd/gb
# setup and prints the fix for anything broken; exit 0 iff nothing is broken.
exec "$(dirname "${BASH_SOURCE[0]}")/onboard.sh" --doctor "$@"
