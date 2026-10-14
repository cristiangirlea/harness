#!/usr/bin/env bash
# name: tool-shapes
# summary: Measure recorded tool output and repeated inline scripts without exposing text.
# effects: read, exec
# parameters: --home:path=HRN_HOME or ~/.hrn; --days:int=30; --json:bool=true
# example: go test -count=1 ./scripts/tool-shapes && out=$(bash scripts/tool-shapes.sh --home scripts/testdata/tool-shapes --days 36500 --json) && test "$(printf '%s' "$out" | tr ',' '\n' | sed -n 's/.*"read":\([0-9]*\).*/\1/p')" = 2 && test "$(printf '%s' "$out" | tr ',' '\n' | sed -n 's/.*"with_usage":\([0-9]*\).*/\1/p')" = 2 && [[ $out == *'"tool_result_bytes":27'* ]] && [[ $out == *'"shape":"heredoc","count":2,"runs":2'* ]]
# hash: 124b853f462dc87db689bbf2b2bc3f96e34a7685d701e2093ff729f8974d4720
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
exec go run ./scripts/tool-shapes "$@"
