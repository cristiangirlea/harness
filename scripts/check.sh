#!/usr/bin/env bash
# name: check
# summary: Run the check skill commands in order, stopping on failure.
# effects: read, write-workspace, exec
# parameters: --list:bool=false
# example: bash scripts/check.sh --list
# hash: pending
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
while IFS= read -r step; do
  if bash -c "$step"; then
    printf '{"step":"passed"}\n'
  else
    printf '{"error":"check step failed"}\n' >&2
    exit 1
  fi
done <<< "$steps"
