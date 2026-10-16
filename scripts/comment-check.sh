#!/usr/bin/env bash
# name: comment-check
# summary: Check Go comments against the repository rule and baseline.
# effects: read, write-workspace, exec
# parameters: --root:path=.; --baseline:path=scripts/comment-baseline.txt; --prune:bool=false; --init:bool=false
# example: go test -count=1 ./scripts/comment-check && out=$(bash scripts/comment-check.sh) && [[ $out == *'"new":[]'* ]] && [[ $out == *'"stale":[]'* ]]
# hash: 5d30ccd0a75006051f468e39d9c66000a878b7e40ec8a8692225882e905b81ff
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
exec go run ./scripts/comment-check "$@"
