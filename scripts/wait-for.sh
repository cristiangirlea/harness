#!/usr/bin/env bash
# name: wait-for
# summary: Wait for a TCP port with a bounded timeout.
# effects: exec, network
# parameters: host:string=required; port:int=required; timeout:seconds=10
# example: out=$(bash scripts/wait-for.sh 127.0.0.1 1 1 2>&1) && exit 1; test "$out" = '{"error":"timeout"}'$'\nexit status 1'
# hash: pending
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
exec go run "$root/scripts/answers.go" wait "$@"
