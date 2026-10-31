#!/usr/bin/env bash
# name: archtest-baseline-test
# summary: Test baseline comparison and layer map reporting in fixture repositories.
# effects: read, exec
# parameters: none
# example: bash scripts/archtest-baseline_test.sh
# hash: 9d0e46a7533fa5816b2b6f090f30b0b8cec4768f6c0360769158ce1a14465ff9
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
git checkout -q -- internal/archtest/layers.go
mkdir -p docs/adr
printf 'Status: proposed, 2026-10-31, fixture\n' > docs/adr/0001-capability-kernel-boundaries.md
git rm -q -f internal/archtest/baseline.txt
git add docs/adr/0001-capability-kernel-boundaries.md
git commit -qm proposed
printf 'agent -> prefs\n' > internal/archtest/baseline.txt
bash "$script" HEAD > out
grep -q '"ok":true' out
printf 'new -> edge\n' >> internal/archtest/baseline.txt
if bash "$script" HEAD > out 2>&1; then exit 1; fi
grep -q 'baseline grew' out
git rm -q docs/adr/0001-capability-kernel-boundaries.md
git commit -qm no-adr
if bash "$script" HEAD > out 2>&1; then exit 1; fi
grep -q 'base has no architecture baseline' out
printf '{"ok":true}\n'
