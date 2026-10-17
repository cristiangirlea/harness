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
error() {
  local s=${1//\\/\\\\} i o c u
  s=${s//\"/\\\"}
  for ((i=1; i<32; i++)); do
    printf -v o '%03o' "$i"; printf -v c "\\$o"; printf -v u '\\u%04x' "$i"; s=${s//"$c"/"$u"}
  done
  printf '{"error":"%s"}\n' "$s" >&2
}
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
while IFS= read -r -d '' file; do
  [[ $file != scripts/testdata/* ]] || continue
  name=${file#scripts/}
  if [[ $name == .* || $name == */.* || $name == */*/* ]] ||
    [[ $name == */* && $name != *.go ]] || [[ $name != */* && ! $name =~ \.(sh|go|txt)$ ]]; then
    error "unsupported file: $file"; status=1
  fi
done < <(find scripts -type f -print0)
for file in scripts/*.go scripts/*/*.go scripts/*.sh; do
  [[ $file != scripts/testdata/* ]] || continue
  header=$(awk '/^#!/ {next} /^# / {sub(/^# /, ""); print; next}
    /^\/\/ / {sub(/^\/\/ /, ""); print; next} {exit}' "$file")
  valid=true
  unset metadata
  declare -A metadata=()
  while IFS= read -r line; do
    [[ $line =~ ^[a-z]+:\  ]] || break
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
      awk -v hash="$hash" '/^# hash: / {$0="# hash: " hash}
        /^\/\/ hash: / {$0="// hash: " hash} {print}' "$file" > "$tmp/stamped"
      cat "$tmp/stamped" > "$file"
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
