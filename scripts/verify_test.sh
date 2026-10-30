#!/usr/bin/env bash
# name: verify-test
# summary: Test verifier stamping, exclusions, errors and read-only checks in fixtures.
# effects: read, exec
# parameters: none
# example: bash scripts/verify_test.sh
# hash: 8b6e458257761791e4e66587f93a7d097201ab154abedba1776b1ed12846ed92
set -euo pipefail
runner=$(cd "$(dirname "$0")" && pwd)/verify.sh
tmp=$(mktemp -d)
trap 'if [[ -n ${listener:-} ]]; then kill "$listener" 2>/dev/null; wait "$listener" 2>/dev/null; fi; rm -rf "$tmp"' EXIT
mkdir "$tmp/scripts"
printf '<!-- scripts index: start -->\n<!-- scripts index: end -->\n' > "$tmp/AGENTS.md"
fixture() {
  printf '#!/usr/bin/env bash\n# name: %s\n# summary: %s script.\n# effects: read\n# parameters: none\n# example: %s\n# hash: %s\ntrue\n' "$1" "$1" "$2" "${4:-pending}" > "${3:-$tmp}/scripts/$1.sh"
}
fail() { printf '{"error":"%s"}\n' "$1" >&2; exit 1; }
check_fails() {
  if bash "$runner" --root "${2:-$tmp}" --check > "$tmp/out" 2>&1; then fail expected-check-failure; fi
  grep -qF "$1" "$tmp/out" || fail "missing diagnostic: $1"
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
clean=$tmp/clean
mkdir -p "$clean/scripts"
printf '<!-- scripts index: start -->\n<!-- scripts index: end -->\n' > "$clean/AGENTS.md"
printf '// name: tool\n// summary: tool program.\n// effects: read\n// parameters: none\n// example: true\n// hash: pending\npackage main\n' > "$clean/scripts/tool.go"
fixture unstamped true "$clean" none
fixture prose true "$clean"
sed -i 's/^true$/# Prose without a key ends the header.\n# example: false\ntrue/' "$clean/scripts/prose.sh"
printf 'data\n' > "$clean/scripts/baseline.txt"
bash "$runner" --root "$clean" > "$tmp/out"
grep -Eq '^// hash: [0-9a-f]{64}$' "$clean/scripts/tool.go" || fail go-header-stamp
grep -q 'scripts/tool.go`: tool program.' "$clean/AGENTS.md" || fail go-header-index
grep -Eq '^# hash: [0-9a-f]{64}$' "$clean/scripts/unstamped.sh" || fail hash-none-stamp
grep -q 'scripts/prose.sh' "$clean/AGENTS.md" || fail prose-header
[[ $(bash "$runner" --root "$clean" --check) == '{"verified":true}' ]] || fail clean-check
sed -i 's/^# effects:/# name: again\n# effects:/' "$clean/scripts/prose.sh"
check_fails 'invalid header: scripts/prose.sh' "$clean"
rm "$clean/scripts/prose.sh"
bash "$runner" --root "$clean" > "$tmp/out"
for extra in tool.py nested/tool.ps1 .hidden.sh .hidden/tool.go nested/deep/tool.go; do
  mkdir -p "$(dirname "$clean/scripts/$extra")"
  printf 'print(1)\n' > "$clean/scripts/$extra"
  check_fails "unsupported file: scripts/$extra" "$clean"
  rm "$clean/scripts/$extra"
done
printf '// name: nested\n// summary: nested program.\n// effects: read\n// parameters: none\n// example: touch ran-main; false\n// hash: pending\npackage main\n' > "$clean/scripts/nested/main.go"
if bash "$runner" --root "$clean" > "$tmp/out" 2>&1; then fail nested-without-entry-example-failure; fi
grep -q 'example failed: scripts/nested/main.go' "$tmp/out" || fail nested-without-entry-diagnostic
[[ -f $clean/ran-main ]] || fail nested-without-entry-example-ran
rm "$clean/ran-main"
fixture nested true "$clean"
for name in defs main_test; do
  sed "s/ran-main/ran-$name/" "$clean/scripts/nested/main.go" > "$clean/scripts/nested/$name.go"
done
mkdir -p "$clean/scripts/testdata/case"
printf 'fixture\n' > "$clean/scripts/testdata/case/input.json"
bash "$runner" --root "$clean" > "$tmp/out" 2>&1 || fail nested-example-skipped
for name in main defs main_test; do
  [[ ! -e $clean/ran-$name ]] || fail "nested-$name-example-not-run"
  grep -Eq '^// hash: [0-9a-f]{64}$' "$clean/scripts/nested/$name.go" || fail "nested-$name-stamp"
  grep -qF "scripts/nested/$name.go"'`: nested program.' "$clean/AGENTS.md" || fail "nested-$name-index"
done
bash "$runner" --root "$clean" --check > "$tmp/out"
printf '// changed\n' >> "$clean/scripts/nested/main.go"
cp "$clean/scripts/nested/main.go" "$tmp/nested"
check_fails 'stale hash: scripts/nested/main.go' "$clean"
cmp "$clean/scripts/nested/main.go" "$tmp/nested" || fail nested-check-wrote-hash
bash "$runner" --root "$clean" > "$tmp/out"
bash "$runner" --root "$clean" --check > "$tmp/out"
for name in main defs main_test; do
  [[ ! -e $clean/ran-$name ]] || fail "nested-$name-check-example-not-run"
done
rm -r "$clean/scripts/nested"
# A quote in a file name must not break the error object.
printf 'true\n' > "$clean/scripts/q\"x.sh"
check_fails 'invalid header: scripts/q\"x.sh"}' "$clean"
rm "$clean/scripts/q\"x.sh"
# Nor may a control character.
printf 'true\n' > "$clean/scripts/n"$'\n'"l.sh"
check_fails 'invalid header: scripts/n\u000al.sh"}' "$clean"
rm "$clean/scripts/n"$'\n'"l.sh"
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
long=$(printf '%0250d' 0)
printf '{"Action":"fail","Test":"%s"}\n' "$long" > "$tmp/long"
[[ $(bash scripts/test-summary.sh "$tmp/long") == '{"passed":0,"failed":1,"skipped":0,"failures":["'"${long:0:200}"'"],"truncated":true}' ]] || fail long-name
for args in '127.0.0.1 0 1' '127.0.0.1 65536 1' '127.0.0.1 1 -1' '127.0.0.1 1 bad' ''; do
  if bash scripts/wait-for.sh $args > "$tmp/out" 2>&1; then fail invalid-wait; fi
  grep -q '"error":' "$tmp/out" || fail wait-error
done
# The helper's self-test owns its ephemeral listener, avoiding port races.
go run scripts/answers.go self-test > "$tmp/out"
[[ $(cat "$tmp/out") == '{"ready":true}' ]] || fail ready
if bash scripts/wait-for.sh 127.0.0.1 1 1 > "$tmp/out" 2>&1; then fail timeout; fi
[[ $(cat "$tmp/out") == '{"error":"timeout"}' ]] || fail timeout-shape
# A listener of the test's own reports its ephemeral port, so the wrapper is probed without races.
mkdir "$tmp/listen"
cat > "$tmp/listen/main.go" <<'EOF'
package main

import (
	"net"
	"os"
	"time"
)

func main() {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		os.Exit(1)
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	os.WriteFile(os.Args[1]+".part", []byte(port), 0600)
	os.Rename(os.Args[1]+".part", os.Args[1])
	l.(*net.TCPListener).SetDeadline(time.Now().Add(60 * time.Second))
	for range 2 {
		c, err := l.Accept()
		if err != nil {
			os.Exit(1)
		}
		c.Close()
	}
}
EOF
printf 'module listen\n\ngo 1.26\n' > "$tmp/listen/go.mod"
(cd "$tmp/listen" && go build -o listen.exe .)
"$tmp/listen/listen.exe" "$tmp/port" &
listener=$!
for ((i=0; i<600; i++)); do [[ -s $tmp/port ]] && break; sleep 0.1; done
port=$(cat "$tmp/port")
[[ $(bash scripts/wait-for.sh 127.0.0.1 "$port" 5) == '{"ready":true}' ]] || fail wait-ready
[[ $(bash scripts/wait-for.sh 127.0.0.1 "$port" 0) == '{"ready":true}' ]] || fail wait-zero
wait "$listener" || fail listener
unset listener
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
# Only the skill's first block counts, blank lines are not steps, and no step reads the others.
skill=$tmp/skill
mkdir -p "$skill/scripts" "$skill/.claude/skills/check"
cp scripts/check.sh "$skill/scripts/check.sh"
printf '%s\n' '```bash' 'cat > /dev/null' '' 'printf "two\n" >> "$TRACE"' '```' 'Prose.' '```bash' 'printf "bogus\n" >> "$TRACE"' '```' > "$skill/.claude/skills/check/SKILL.md"
[[ $("$real_bash" "$skill/scripts/check.sh" --list) == $'cat > /dev/null\nprintf "two\\n" >> "$TRACE"' ]] || fail check-blocks
TRACE="$tmp/skill-trace" "$real_bash" "$skill/scripts/check.sh" > "$tmp/out"
[[ $(cat "$tmp/out") == $'{"step":1,"result":"passed"}\n{"step":2,"result":"passed"}' ]] || fail check-steps
[[ $(cat "$tmp/skill-trace") == two ]] || fail check-stdin
grep -A3 'name: kept scripts verify$' .github/workflows/ci.yml | grep -q 'timeout-minutes: 5$' || fail ci-timeout
if git ls-files -s 'scripts/*.sh' | grep -v '^100755 '; then fail script-mode; fi
fixture invalidhint true "$clean"
sed -i 's/# parameters: none/# parameters: words without grammar\n# matches: x/' "$clean/scripts/invalidhint.sh"
check_fails 'invalid header: scripts/invalidhint.sh' "$clean"
rm "$clean/scripts/invalidhint.sh"
fixture 'unsafe name' true "$clean"
sed -i '/# parameters: none/a # matches: x' "$clean/scripts/unsafe name.sh"
check_fails 'invalid header: scripts/unsafe name.sh' "$clean"
rm "$clean/scripts/unsafe name.sh"
printf '{"passed":true}\n'
