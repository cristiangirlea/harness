#!/usr/bin/env bash
# name: verify-test
# summary: Test verifier stamping, exclusions, errors and read-only checks in fixtures.
# effects: read, exec
# parameters: none
# example: bash scripts/verify_test.sh
# hash: pending
set -euo pipefail
runner=$(cd "$(dirname "$0")" && pwd)/verify.sh
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/scripts"
printf '<!-- scripts index: start -->\n<!-- scripts index: end -->\n' > "$tmp/AGENTS.md"
fixture() {
  printf '#!/usr/bin/env bash\n# name: %s\n# summary: %s script.\n# effects: read\n# parameters: none\n# example: %s\n# hash: pending\ntrue\n' "$1" "$1" "$2" > "$tmp/scripts/$1.sh"
}
fail() { printf '{"error":"%s"}\n' "$1" >&2; exit 1; }
check_fails() {
  if bash "$runner" --root "$tmp" --check > "$tmp/out" 2>&1; then fail expected-check-failure; fi
  grep -q "$1" "$tmp/out" || fail "missing diagnostic: $1"
}
fixture good 'bash scripts/good.sh'
fixture stale 'bash scripts/stale.sh'
fixture failing 'false'
fixture paid 'none (spends money)'
printf 'exit 99\n' >> "$tmp/scripts/paid.sh"
check_fails 'stale hash: scripts/good.sh'
grep -q 'example failed: scripts/failing.sh' "$tmp/out" || fail failing-diagnostic
grep -q 'index differs' "$tmp/out" || fail index-diagnostic
[[ $(grep '^# hash:' "$tmp/scripts/good.sh") == '# hash: pending' ]] || fail check-wrote-hash
[[ $(wc -l < "$tmp/AGENTS.md") == 2 ]] || fail check-wrote-index
if bash "$runner" --root "$tmp" > "$tmp/out" 2>&1; then fail expected-example-failure; fi
grep -q 'scripts/good.sh' "$tmp/AGENTS.md" || fail good-index
if grep -q 'scripts/failing.sh' "$tmp/AGENTS.md"; then fail failing-index; fi
grep -q '(not verified: spends money)' "$tmp/AGENTS.md" || fail excluded-index
sed -i 's/example: false/example: true/' "$tmp/scripts/failing.sh"
bash "$runner" --root "$tmp" > "$tmp/out"
bash "$runner" --root "$tmp" --check > "$tmp/out"
cp "$tmp/AGENTS.md" "$tmp/index"
cp "$tmp/scripts/good.sh" "$tmp/good"
bash "$runner" --root "$tmp" > "$tmp/out"
cmp "$tmp/AGENTS.md" "$tmp/index" && cmp "$tmp/scripts/good.sh" "$tmp/good" || fail idempotence
printf '# changed\n' >> "$tmp/scripts/stale.sh"
check_fails 'stale hash: scripts/stale.sh'
bash "$runner" --root "$tmp" > "$tmp/out"
sed -i 's/good script/incorrect summary/' "$tmp/AGENTS.md"
check_fails 'index differs'
bash "$runner" --root "$tmp" > "$tmp/out"
sed -i '/# parameters:/d' "$tmp/scripts/good.sh"
check_fails 'invalid header: scripts/good.sh'
sed -i '/scripts index: end/d' "$tmp/AGENTS.md"
check_fails 'index markers'
if bash "$runner" --unknown > "$tmp/out" 2>&1; then fail invalid-argument; fi
grep -q '"error":' "$tmp/out" || fail usage-error
printf '{"passed":true}\n'
