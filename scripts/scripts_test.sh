#!/usr/bin/env bash
# name: scripts-test
# summary: Test kept script answers and check runner ordering without real checks.
# effects: read, exec
# parameters: none
# example: bash scripts/scripts_test.sh
# hash: pending
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fail() { printf '{"error":"%s"}\n' "$1" >&2; exit 1; }
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
printf '#!/usr/bin/env bash\nprintf "%%s\\n" "$*" >> "$TRACE"\nexit 7\n' > "$tmp/bin/go"
chmod +x "$tmp/bin/go"
if PATH="$tmp/bin:$PATH" TRACE="$tmp/trace" bash scripts/check.sh > "$tmp/out" 2>&1; then fail check-failure; fi
[[ $(cat "$tmp/trace") == 'vet ./...' ]] || fail check-stop
grep -q '"error":' "$tmp/out" || fail check-error
printf '{"passed":true}\n'
