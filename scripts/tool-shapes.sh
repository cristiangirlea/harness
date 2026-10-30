#!/usr/bin/env bash
# name: tool-shapes
# summary: Measure recorded tool output, repeated inline scripts and tool definitions without exposing text.
# effects: read, exec
# parameters: --home:path=HRN_HOME or ~/.hrn; --days:int=30; --json:bool=true; --definitions:bool=false
# example: go test -count=1 ./scripts/tool-shapes && out=$(bash scripts/tool-shapes.sh --home scripts/testdata/tool-shapes --days 36500 --json) && test "$(printf '%s' "$out" | tr ',' '\n' | sed -n 's/.*"read":\([0-9]*\).*/\1/p')" = 2 && test "$(printf '%s' "$out" | tr ',' '\n' | sed -n 's/.*"with_usage":\([0-9]*\).*/\1/p')" = 2 && [[ $out == *'"tool_result_bytes":27'* ]] && [[ $out == *'"shape":"heredoc","count":2,"runs":2'* ]] && defs=$(bash scripts/tool-shapes.sh --home scripts/testdata/tool-shapes --days 36500 --definitions) && [[ $defs == *'"mcp_definitions":"not recorded"'* ]] && [[ $defs == *'"api":{"runs":1,"runs_from_usage":1,"calls":1,"median_tokens":200,"runs_with_cache_write":0,"cache_write_tokens":0}'* ]]
# hash: 5ea3647675c6daf4570c081360bba93a375f9f5ecfdc6b6be588e179653e0f33
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
exec go run ./scripts/tool-shapes "$@"
