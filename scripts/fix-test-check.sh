#!/usr/bin/env bash
# name: fix-test-check
# summary: Require a changed test for closing fixes that change Go code.
# effects: read, exec
# parameters: file:path=required
# example: go test -count=1 ./scripts/fix-test-check && root=$PWD && dir=$(mktemp -d) && printf '%s' '{"body":"Update"}' > "$dir/input file.json" && out=$(cd "$dir" && bash "$root/scripts/fix-test-check.sh" 'input file.json') && rm -rf "$dir" && [[ $out == *'"ok":true'* ]]
# hash: ad8ea7e941de8338d68e127edbc88c4b7839bbbb3e115097b241485b7aae59ae
set -euo pipefail
if [[ $# == 1 && $1 != "" && $1 != /* && $1 != [[:alpha:]]:[\\/]* && $1 != \\\\* ]]; then
  set -- "$PWD/$1"
fi
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
# Building preserves the distinction between a failed check and an input error.
dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
go build -o "$dir/fix-test-check$(go env GOEXE)" ./scripts/fix-test-check
"$dir/fix-test-check$(go env GOEXE)" "$@"
