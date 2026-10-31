#!/usr/bin/env bash
# name: archtest-baseline-test
# summary: Test baseline comparison and layer map reporting in fixture repositories.
# effects: read, exec
# parameters: none
# example: bash scripts/archtest-baseline_test.sh
# hash: 067b7112256bd99e7fd369cc68a650d38a52b75882d68ec56fc0c6f39ddffe7a
set -euo pipefail
script=$(cd "$(dirname "$0")" && pwd)/archtest-baseline.sh
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cd "$tmp"
git init -q
git config user.name fixture
git config user.email fixture@example.invalid
mkdir -p internal/archtest
printf 'a -> b\nc -> d\n' > internal/archtest/baseline.txt
printf 'map one\n' > internal/archtest/layers.go
git add . && git commit -qm base
printf 'a -> b\n' > internal/archtest/baseline.txt
bash "$script" HEAD > out
grep -q '"ok":true' out
printf 'new -> edge\n' >> internal/archtest/baseline.txt
if bash "$script" HEAD > out 2>&1; then exit 1; fi
grep -q 'new -> edge' out
printf 'a -> b\n' > internal/archtest/baseline.txt
printf 'map two\n' > internal/archtest/layers.go
bash "$script" HEAD > out
grep -q 'map two' out
if bash "$script" missing-ref > out 2>&1; then exit 1; fi
printf '{"ok":true}\n'
