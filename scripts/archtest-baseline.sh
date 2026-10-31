#!/usr/bin/env bash
# name: archtest-baseline
# summary: Compare the architecture baseline and layer map with a base ref.
# effects: read, exec
# parameters: base-ref:string=required
# example: bash scripts/archtest-baseline_test.sh
# hash: b98833faaaeec6e8db6244edfebd31a9ae2ecab305591c80e46da7f2e30a864f
set -euo pipefail
if (( $# != 1 )); then printf '{"error":"usage: archtest-baseline.sh <base-ref>"}\n' >&2; exit 2; fi
base=$1
git rev-parse --verify --quiet "${base}^{commit}" >/dev/null || { printf '{"error":"invalid base ref"}\n' >&2; exit 1; }
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
baseline=internal/archtest/baseline.txt
map=internal/archtest/layers.go
if git cat-file -e "$base:$baseline" 2>/dev/null; then
  git show "$base:$baseline" | LC_ALL=C sort -u > "$tmp/base"
else
  git show "$base:docs/adr/0001-capability-kernel-boundaries.md" 2>/dev/null | grep -q "^Status: proposed, 2026-10-31," || { printf '{"error":"base has no architecture baseline"}\n' >&2; exit 1; }
  # The first architecture baseline is fixed by the accepted ADR.
  cat > "$tmp/base" <<'BASELINE'
agent -> prefs
agent -> prefs (test)
agent -> purpose
agent -> remote
agent -> route/table
agent -> route/table (test)
eval -> game (test)
eval -> update (test)
flow -> agent
flow -> agent (test)
limits -> agent
limits -> agent (test)
limits -> followup
mcp -> retrieval
quota -> agent
quota -> agent (test)
runlog -> agent
runlog -> agent (test)
server/cmd/hrn-worker -> internal/agent
server/platform -> internal/agent
server/platform -> internal/agent (test)
server/platform -> internal/remote (test)
server/platform -> internal/retrieval
server/platform -> internal/retrieval (test)
tools -> retrieval
update -> watch
BASELINE
  LC_ALL=C sort -u "$tmp/base" -o "$tmp/base"
fi
LC_ALL=C sort -u "$baseline" > "$tmp/head"
comm -13 "$tmp/base" "$tmp/head" > "$tmp/added"
if [[ -s $tmp/added ]]; then printf '{"error":"baseline grew"}\n' >&2; cat "$tmp/added" >&2; exit 1; fi
if git cat-file -e "$base:$map" 2>/dev/null; then git show "$base:$map" > "$tmp/map"; else : > "$tmp/map"; fi
if ! cmp -s "$tmp/map" "$map"; then
  printf '%s\n' 'Layer map changed:'
  diff -u --label "base:$map" --label "$map" "$tmp/map" "$map" || [[ $? == 1 ]]
fi
printf '{"ok":true}\n'
