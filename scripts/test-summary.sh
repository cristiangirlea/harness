#!/usr/bin/env bash
# name: test-summary
# summary: Count test results and return capped failure names as JSON.
# effects: read, exec
# parameters: file:path=stdin
# example: test "$(printf '%s\n' '{"Action":"pass","Test":"ok"}' | bash scripts/test-summary.sh)" = '{"passed":1,"failed":0,"skipped":0,"failures":[],"truncated":false}'
# hash: 4214943f921fff773d1c06c9d0bddebc887dca7afb392f50becfc9ed60ae919d
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
if (( $# > 1 )); then
  printf '{"error":"usage: test-summary.sh [file]"}\n' >&2
  exit 2
fi
# Preserve the caller's working directory for relative input paths.
exec go run "$root/scripts/answers.go" summary "$@"
