#!/usr/bin/env bash
# name: wait-for
# summary: Wait for a TCP port with a bounded timeout.
# effects: exec, network
# parameters: host:string=required; port:int=required; timeout:seconds=10
# example: out=$(bash scripts/wait-for.sh 127.0.0.1 1 1 2>&1) && exit 1; test "$out" = '{"error":"timeout"}'
# matches: socket\.create_connection|\.connect_ex\(|net\.Dial(?:Timeout)?\(|net['"]?\)?\.createConnection\(|\bnc (?:-\w+ (?:\d+ )?)*-\w*z|/dev/tcp/|(?i:test-netconnection)
# hash: cc48c17f97d87c012cbbe5f52e6abb74ee6973357e96e1508bc53a844625a7e3
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
err=$(mktemp)
trap 'rm -f "$err"' EXIT
status=0
go run "$root/scripts/answers.go" wait "$@" 2> "$err" || status=$?
# go run adds its own "exit status N" line after the error object.
grep -v '^exit status [0-9]*$' "$err" >&2 || true
exit "$status"
