#!/usr/bin/env bash
# name: check
# summary: Run the check skill commands in order, stopping on failure.
# effects: read, write-workspace, exec
# parameters: --list:bool=false
# example: bash scripts/check.sh --list
# hash: 41220986af322ba142f4ba165c2e04f1a58e0973184260c03abca8d958444aab
set -euo pipefail
cd "$(dirname "$0")/.."
if (( $# > 1 )) || [[ ${1:-} != '' && ${1:-} != --list ]]; then
  printf '{"error":"usage: check.sh [--list]"}\n' >&2
  exit 2
fi
steps=$(sed -n '/^```bash$/,/^```$/p' .claude/skills/check/SKILL.md | sed '1d;$d')
if [[ -z $steps ]]; then
  printf '{"error":"check skill has no steps"}\n' >&2
  exit 1
fi
if [[ ${1:-} == --list ]]; then printf '%s\n' "$steps"; exit 0; fi
log=$(mktemp)
trap 'rm -f "$log"' EXIT
number=0
while IFS= read -r step; do
  number=$((number + 1))
  if bash -c "$step" > "$log" 2>&1; then
    printf '{"step":%d,"result":"passed"}\n' "$number"
  else
    printf '{"error":"check step failed","step":%d}\n' "$number" >&2
    tail -c 2000 "$log" >&2
    exit 1
  fi
done <<< "$steps"
