#!/usr/bin/env bash
set -euo pipefail

if [[ ${1:-} != report ]]; then
  printf 'expected report command\n' >&2
  exit 2
fi
cat "$(dirname "${BASH_SOURCE[0]}")/report.json"
