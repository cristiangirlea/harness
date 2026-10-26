#!/usr/bin/env bash
# name: privacy-check
# summary: Check tracked text files for personal folders, local work paths and narration of a person.
# effects: read, exec
# parameters: --root:path=.
# example: go test -count=1 ./scripts/privacy-check && out=$(bash scripts/privacy-check.sh) && [[ $out == *'"findings":[]'* ]]
# hash: c62d83072ccfa28738cb71df85bd17098ea2e0fa0dce8a32f0989e82f8af68fa
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
# Built, not run with go run, so its exit code passes through: 1 for findings, 2 for an error.
dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
go build -o "$dir/privacy-check$(go env GOEXE)" ./scripts/privacy-check
"$dir/privacy-check$(go env GOEXE)" "$@"
