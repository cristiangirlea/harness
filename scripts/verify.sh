#!/usr/bin/env bash
# name: verify
# summary: Run kept examples and synchronize script hashes and the AGENTS index.
# effects: read, write-workspace, exec
# parameters: --check:bool=false; --root:path=repository
# example: none (the runner)
# hash: none
set -euo pipefail
check=false
root=$(cd "$(dirname "$0")/.." && pwd)
error() { printf '{"error":"%s"}\n' "$1" >&2; }
while (( $# )); do
  case "$1" in
    --check) check=true; shift ;;
    --root) [[ $# -ge 2 ]] || { error 'missing root'; exit 2; }; root=$2; shift 2 ;;
    *) error 'usage: verify.sh [--check] [--root path]'; exit 2 ;;
  esac
done
cd "$root"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
start='<!-- scripts index: start -->'
end='<!-- scripts index: end -->'
if [[ ! -f AGENTS.md ]] || ! awk -v s="$start" -v e="$end" '
  $0==s {a++; position=NR} $0==e {b++; if (NR<=position || !a) bad=1}
  END {exit !(a==1 && b==1 && !bad)}' AGENTS.md; then
  error 'invalid index markers in AGENTS.md'; exit 1
fi
if command -v sha256sum >/dev/null; then hash_command=(sha256sum)
elif command -v shasum >/dev/null; then hash_command=(shasum -a 256)
else error 'no SHA-256 command'; exit 1; fi
status=0
: > "$tmp/index"
shopt -s nullglob
for file in scripts/*.go scripts/*.sh; do
  header=$(awk '/^#!/ {next} /^# / {sub(/^# /, ""); print; next}
    /^\/\/ / {sub(/^\/\/ /, ""); print; next} {exit}' "$file")
  valid=true
  unset metadata
  declare -A metadata=()
  while IFS= read -r line; do
    [[ $line == *': '* ]] || continue
    key=${line%%: *}
    value=${line#*: }
    [[ ! ${metadata[$key]+set} ]] || valid=false
    metadata[$key]=$value
  done <<< "$header"
  for key in name summary effects parameters example hash; do
    value=${metadata[$key]:-}
    [[ -n $value && $value != *$'\n'* ]] || valid=false
  done
  effects=${metadata[effects]:-}
  [[ $effects =~ ^(read|write-workspace|exec|network)(,[[:space:]]*(read|write-workspace|exec|network))*$ ]] || valid=false
  if ! $valid; then error "invalid header: $file"; status=1; continue; fi
  summary=${metadata[summary]}
  example=${metadata[example]}
  if [[ $example =~ ^none\ \((.+)\)$ ]]; then
    reason=${BASH_REMATCH[1]}
    printf -- '- `%s`: %s (not verified: %s)\n' "$file" "$summary" "$reason" >> "$tmp/index"
    continue
  fi
  if ! bash -c "$example" > "$tmp/output" 2>&1; then
    error "example failed: $file"
    tail -c 2000 "$tmp/output" >&2
    status=1
    continue
  fi
  hash=$(sed '/^# hash: /d; /^\/\/ hash: /d' "$file" | "${hash_command[@]}" | awk '{print $1}')
  if [[ ${metadata[hash]} != "$hash" ]]; then
    if $check; then error "stale hash: $file"; status=1
    else
      content=$(awk -v hash="$hash" '/^# hash: / {$0="# hash: " hash}
        /^\/\/ hash: / {$0="// hash: " hash} {print}' "$file")
      printf '%s\n' "$content" > "$file"
    fi
  fi
  printf -- '- `%s`: %s\n' "$file" "$summary" >> "$tmp/index"
done
LC_ALL=C sort "$tmp/index" > "$tmp/sorted"
awk -v s="$start" -v e="$end" -v indexfile="$tmp/sorted" '
  $0==s {print; while ((getline line < indexfile)>0) print line; inside=1; next}
  $0==e {inside=0} !inside {print}' AGENTS.md > "$tmp/agents"
if ! cmp -s AGENTS.md "$tmp/agents"; then
  if $check; then error 'index differs: AGENTS.md'; status=1
  else cat "$tmp/agents" > AGENTS.md; fi
fi
if (( status == 0 )); then printf '{"verified":true}\n'; fi
exit "$status"
