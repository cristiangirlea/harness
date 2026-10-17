#!/usr/bin/env bash
# name: tool-shapes
# summary: Measure recorded tool output, repeated inline scripts and tool definitions without exposing text.
# effects: read, exec
# parameters: --home:path=HRN_HOME or ~/.hrn; --days:int=30; --json:bool=true; --definitions:bool=false
# example: go test -count=1 ./scripts/tool-shapes && out=$(bash scripts/tool-shapes.sh --home scripts/testdata/tool-shapes --days 36500 --json) && test "$(printf '%s' "$out" | tr ',' '\n' | sed -n 's/.*"read":\([0-9]*\).*/\1/p')" = 2 && test "$(printf '%s' "$out" | tr ',' '\n' | sed -n 's/.*"with_usage":\([0-9]*\).*/\1/p')" = 2 && [[ $out == *'"tool_result_bytes":27'* ]] && [[ $out == *'"shape":"heredoc","count":2,"runs":2'* ]] && defs=$(bash scripts/tool-shapes.sh --home scripts/testdata/tool-shapes --days 36500 --definitions) && [[ $defs == *'"mcp_definitions":"not recorded"'* ]] && [[ $defs == *'"api":{"runs":1,"runs_from_usage":1,"calls":1,"median_tokens":200}'* ]]
# hash: 3fd3028248b09df13923888305f3a678753cb5261c0f7407b025ac8739503bcc
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
exec go run ./scripts/tool-shapes "$@"
