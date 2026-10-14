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
printf '\n\n' >> "$tmp/scripts/good.sh"
fixture stale 'bash scripts/stale.sh'
fixture failing 'false'
fixture paid 'none (spends money)'
printf 'exit 99\n' >> "$tmp/scripts/paid.sh"
check_fails 'stale hash: scripts/good.sh'
grep -q 'example failed: scripts/failing.sh' "$tmp/out" || fail failing-diagnostic
grep -q 'index differs' "$tmp/out" || fail index-diagnostic
[[ $(grep '^# hash:' "$tmp/scripts/good.sh") == '# hash: pending' ]] || fail check-wrote-hash
[[ $(wc -l < "$tmp/AGENTS.md") == 2 ]] || fail check-wrote-index
cp "$tmp/scripts/good.sh" "$tmp/before"
if bash "$runner" --root "$tmp" > "$tmp/out" 2>&1; then fail expected-example-failure; fi
cmp <(sed '/^# hash: /d' "$tmp/before") <(sed '/^# hash: /d' "$tmp/scripts/good.sh") || fail changed-body
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
# Entry-point behavior tests use temporary fixtures and fake check commands.
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
printf '%s\n' '{"Action":"pass","Test":"ok"}' '{"Action":"skip","Test":"skip"}' '{"Action":"fail","Test":"bad\"name"}' '{"Action":"fail","Package":"pkg"}' > "$tmp/events"
expected='{"passed":1,"failed":1,"skipped":1,"failures":["bad\"name"],"truncated":false}'
[[ $(bash scripts/test-summary.sh "$tmp/events") == "$expected" ]] || fail summary-file
[[ $(bash scripts/test-summary.sh < "$tmp/events") == "$expected" ]] || fail summary-stdin
[[ $(bash scripts/test-summary.sh < /dev/null) == '{"passed":0,"failed":0,"skipped":0,"failures":[],"truncated":false}' ]] || fail empty
if printf 'broken\n' | bash scripts/test-summary.sh > "$tmp/out" 2>&1; then fail invalid-json; fi
grep -q '"error":' "$tmp/out" || fail error-shape
if bash scripts/test-summary.sh "$tmp/missing" > "$tmp/out" 2>&1; then fail missing-file; fi
for ((i=0; i<25; i++)); do printf '{"Action":"fail","Test":"test%s"}\n' "$i"; done > "$tmp/many"
bash scripts/test-summary.sh "$tmp/many" > "$tmp/out"
grep -q '"failed":25' "$tmp/out" && grep -q '"truncated":true' "$tmp/out" || fail cap
for args in '127.0.0.1 0 1' '127.0.0.1 65536 1' '127.0.0.1 1 -1' '127.0.0.1 1 bad' ''; do
  if bash scripts/wait-for.sh $args > "$tmp/out" 2>&1; then fail invalid-wait; fi
  grep -q '"error":' "$tmp/out" || fail wait-error
done
# The helper's self-test owns its ephemeral listener, avoiding port races.
go run scripts/answers.go self-test > "$tmp/out"
[[ $(cat "$tmp/out") == '{"ready":true}' ]] || fail ready
if bash scripts/wait-for.sh 127.0.0.1 1 1 > "$tmp/out" 2>&1; then fail timeout; fi
grep -q '"error":"timeout"' "$tmp/out" || fail timeout-shape
sed -n '/^```bash$/,/^```$/p' .claude/skills/check/SKILL.md | sed '1d;$d' > "$tmp/steps"
bash scripts/check.sh --list > "$tmp/list"
cmp "$tmp/steps" "$tmp/list" || fail check-list
# Shadow commands so executing the runner tests order and short-circuiting only.
mkdir "$tmp/bin"
printf '#!/usr/bin/env bash\nprintf "%%s\\n" "$*" >> "$TRACE"\nprintf "%%5000s\\n" diagnostic\nexit 7\n' > "$tmp/bin/go"
chmod +x "$tmp/bin/go"
if PATH="$tmp/bin:$PATH" TRACE="$tmp/trace" bash scripts/check.sh > "$tmp/out" 2>&1; then fail check-failure; fi
[[ $(cat "$tmp/trace") == 'vet ./...' ]] || fail check-stop
grep -q '"error":' "$tmp/out" || fail check-error
(( $(wc -c < "$tmp/out") < 2200 )) || fail check-output-cap
real_bash=$(command -v bash)
printf '#!/bin/bash\nprintf "%%s\\n" "$2" >> "$TRACE"\n' > "$tmp/bin/bash"
chmod +x "$tmp/bin/bash"
PATH="$tmp/bin:$PATH" TRACE="$tmp/all" "$real_bash" scripts/check.sh > "$tmp/out"
cmp "$tmp/steps" "$tmp/all" || fail check-order
if "$real_bash" scripts/check.sh --unknown > "$tmp/out" 2>&1; then fail check-usage; fi
printf '{"passed":true}\n'
