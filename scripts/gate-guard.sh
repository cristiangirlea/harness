#!/usr/bin/env bash
# name: gate-guard
# summary: Run hrn's guard tests against a real adanext-exec built at the pinned commit.
# effects: read, exec, network
# parameters: --pin:bool=false
# example: bash scripts/gate-guard.sh --pin | grep -Eq '^[0-9a-f]{40}$'
# hash: 5be8f5d8d89958cc05828091e34e83fd1e2d8198215156923e1e10a292ea14ea
set -euo pipefail
cd "$(dirname "$0")/.."
pin=eb6714faa442eecc59e76582f3b326ba89b36632
if (( $# > 1 )) || [[ ${1:-} != '' && ${1:-} != --pin ]]; then
  printf '{"error":"usage: gate-guard.sh [--pin]"}\n' >&2
  exit 2
fi
if [[ ${1:-} == --pin ]]; then printf '%s\n' "$pin"; exit 0; fi
bin=$(mktemp -d)
trap 'rm -rf "$bin"' EXIT
# Go on Windows takes only its own path form.
native=$(cd "$bin" && { pwd -W 2> /dev/null || pwd; })
if ! built=$(GOBIN=$native go install "github.com/cristiangirlea/adanext-exec/cmd/adanext-exec@$pin" 2>&1); then
  printf '%s\n' "$built" | tail -n 20 >&2
  printf '{"error":"could not build the gate","pin":"%s"}\n' "$pin" >&2
  exit 1
fi
ext=''
[[ -f $bin/adanext-exec.exe ]] && ext=.exe
if ! log=$(HRN_TEST_GATE="$native/adanext-exec$ext" go test -v -count=1 -timeout 15m -run RealGate ./internal/tools ./internal/agent 2>&1); then
  printf '%s\n' "$log" | tail -n 40 >&2
  printf '{"error":"a guard test failed against the real gate"}\n' >&2
  exit 1
fi
passed=$(grep -c '^--- PASS: TestRealGate' <<< "$log" || true)
want=6
if (( passed != want )); then
  printf '{"error":"the real gate tests did not all run","passed":%d,"want":%d}\n' "$passed" "$want" >&2
  exit 1
fi
printf '{"passed":%d,"pin":"%s"}\n' "$passed" "$pin"
