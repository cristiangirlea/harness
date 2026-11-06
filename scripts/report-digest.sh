#!/usr/bin/env bash
# name: report-digest
# summary: Summarise a window of hrn runs: pass rate, cost, batch attempts and the largest tool output shape.
# effects: read, exec
# parameters: --home:path=HRN_HOME; --days:int=7; --hrn:path=hrn
# example: go test -count=1 ./scripts/report-digest && out=$(bash scripts/report-digest.sh --home scripts/testdata/tool-shapes --days 36500 --hrn scripts/testdata/report-digest/fake-hrn.sh) && [[ $out == *'"runs":{"runs":3,'* ]] && [[ $out == *'"purpose":"build"'* ]] && [[ $out == *'"top_shape":"'* ]] && err=$(bash scripts/report-digest.sh --days 0 2>&1 >/dev/null; true) && [[ $err == '{"error":"days must be a positive integer"}' ]]
# hash: 80b64be1ef9e61e860327cb36ce2dd2fdcc054653f19c3c2be6733b6d7d70094

set -euo pipefail

fail() {
  printf '{"error":"%s"}\n' "$1" >&2
  exit 1
}

cd "$(dirname "${BASH_SOURCE[0]}")/.." 2>/dev/null || fail 'cannot enter repository'

home=${HRN_HOME:-${HOME:-}/.hrn}
days=7
hrn=hrn
while (($#)); do
  case "$1" in
    --home|--days|--hrn)
      (($# >= 2)) || fail 'option requires a value'
      case "$1" in
        --home) home=$2 ;;
        --days) days=$2 ;;
        --hrn) hrn=$2 ;;
      esac
      shift 2
      ;;
    *) fail 'unknown option' ;;
  esac
done

[[ $days =~ ^[0-9]+$ && $days =~ [1-9] ]] || fail 'days must be a positive integer'
command -v "$hrn" >/dev/null 2>&1 || fail 'hrn binary not found'
command -v go >/dev/null 2>&1 || fail 'go not found'
tmp=$(mktemp -d 2>/dev/null) || fail 'cannot create temporary directory'
trap 'rm -rf -- "$tmp"' EXIT

HRN_HOME="$home" "$hrn" report --since "${days}d" --by purpose --json > "$tmp/report.json" 2>/dev/null || fail 'hrn report failed'
bash scripts/tool-shapes.sh --home "$home" --days "$days" --json > "$tmp/shapes.json" 2>/dev/null || fail 'tool shapes failed'
if ! go run ./scripts/report-digest --report "$tmp/report.json" --shapes "$tmp/shapes.json" --days "$days" 2> "$tmp/error.json"; then
  # Keep the program's error object; go run adds an exit status line after it.
  line=$(head -n 1 "$tmp/error.json")
  [[ $line == '{"error":'* ]] && { printf '%s\n' "$line" >&2; exit 1; }
  fail 'digest failed'
fi
