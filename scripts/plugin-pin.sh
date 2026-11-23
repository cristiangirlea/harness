#!/usr/bin/env bash
# name: plugin-pin
# summary: Install the pinned hrn-search release with this checkout's hrn and run one call through it.
# effects: read, exec, network
# parameters: --pin:bool=false
# example: bash scripts/plugin-pin.sh --pin | grep -Eq '^cristiangirlea/hrn-search@v[0-9]+[.][0-9]+[.][0-9]+$'
# hash: 0acd0378770651455f8c7f74c844f7ba3d1b855930ca2f32d9ffb9dbf2a7ae33
set -euo pipefail
cd "$(dirname "$0")/.."
# A broken plugin release is a pin that does not move: change it only in a pull request.
source=cristiangirlea/hrn-search
tag=v0.1.0
if (( $# > 1 )) || [[ ${1:-} != '' && ${1:-} != --pin ]]; then
  printf '{"error":"usage: plugin-pin.sh [--pin]"}\n' >&2
  exit 2
fi
if [[ ${1:-} == --pin ]]; then printf '%s@%s\n' "$source" "$tag"; exit 0; fi
platform=$(go env GOOS)_$(go env GOARCH)
# A tag can move and its assets can be replaced, so the bytes are pinned too.
case $platform in
  darwin_amd64) want=5f535505e748223d493f2630d24366ccb6c547219d7b1641e5de5622871558b7 ;;
  darwin_arm64) want=cae3a525420b54ebbad721a6ee85747b041a43b3e7935ad0e8d55aabadd8c08a ;;
  linux_amd64) want=36cc1f06fe929d2abbdee9e81fcaa662dcaa771736fb30fd231ef83ab5aaa494 ;;
  linux_arm64) want=37d3bc301eb6db7b887b4ecbae7f92310c6e23f2e154f9088757e01881b51175 ;;
  windows_amd64) want=ca6f2d80005a565d0519d9f50b2596d23ec8a737fdcb8d2292ca34f1e93a7d86 ;;
  windows_arm64) want=53db5a9d8c772e5774fe3c0a7458f76828a665a3e7b52c703c1a703d1e4524a3 ;;
  *)
    printf '{"error":"no pinned hash for this platform","platform":"%s"}\n' "$platform" >&2
    exit 1
    ;;
esac
asset=hrn-search_${tag#v}_$platform
[[ $platform == windows_* ]] && asset=$asset.exe
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
# Refuse a changed release before hrn runs anything from it: install starts the plugin.
if ! sums=$(curl -fsSL --max-time 60 "https://github.com/$source/releases/download/$tag/SHA256SUMS" 2>&1); then
  printf '{"error":"could not read the release checksums","pin":"%s@%s"}\n' "$source" "$tag" >&2
  exit 1
fi
if ! grep -qE "^$want [ *]?$asset\$" <<< "$sums"; then
  printf '{"error":"the release checksum differs from the pinned hash","asset":"%s"}\n' "$asset" >&2
  exit 1
fi
# Go and hrn on Windows take only their own path form.
native=$(cd "$work" && { pwd -W 2> /dev/null || pwd; })
ext=''
[[ $platform == windows_* ]] && ext=.exe
if ! built=$(go build -o "$native/hrn$ext" ./cmd/hrn 2>&1); then
  printf '%s\n' "$built" | tail -n 20 >&2
  printf '{"error":"could not build hrn"}\n' >&2
  exit 1
fi
hrn=$work/hrn$ext
mkdir -p "$work/home"
# A scratch home: the install goes through the short-name and allow-list settings, never ~/.hrn.
printf '{"plugins":{"sources":{"search":"%s"},"allow":["%s"]}}\n' "$source" "$source" > "$work/home/backends.json"
export HRN_HOME=$native/home
if ! installed=$("$hrn" plugin install "search@$tag" --json 2>&1); then
  printf '%s\n' "$installed" | tail -n 20 >&2
  printf '{"error":"could not install the pinned plugin","pin":"%s@%s"}\n' "$source" "$tag" >&2
  exit 1
fi
sha=$(grep -oE '"sha256": ?"[0-9a-f]{64}"' <<< "$installed" | grep -oE '[0-9a-f]{64}' || true)
if [[ $sha != "$want" ]]; then
  printf '{"error":"the installed executable differs from the pinned hash","sha256":"%s"}\n' "$sha" >&2
  exit 1
fi
if ! called=$("$hrn" plugin call search search --input-file scripts/testdata/plugin-search-task.json --json 2> "$work/call.err"); then
  tail -n 20 "$work/call.err" >&2
  printf '%s\n' "$called" | tail -c 2000 >&2
  printf '{"error":"the pinned plugin call failed","pin":"%s@%s"}\n' "$source" "$tag" >&2
  exit 1
fi
verdict=$(grep -oE '"verdict": ?"[a-z_]+"' <<< "$called" | grep -oE '[a-z_]+"$' | tr -d '"' || true)
run=$(grep -oE '"run_id": ?"[^"]+"' <<< "$called" | sed -E 's/.*"([^"]+)"$/\1/' || true)
if [[ -z $run ]]; then
  printf '{"error":"the call output has no run_id"}\n' >&2
  exit 1
fi
if [[ $verdict != verified ]]; then
  printf '{"error":"the pinned plugin did not verify the task","verdict":"%s"}\n' "$verdict" >&2
  exit 1
fi
record=$("$hrn" show --json "$run" 2>&1 || true)
if ! grep -qE '"backend": ?"plugin"' <<< "$record" || ! grep -q "$sha" <<< "$record"; then
  printf '{"error":"the run record does not name the installed plugin","run":"%s"}\n' "$run" >&2
  exit 1
fi
printf '{"pin":"%s@%s","verdict":"%s","sha256":"%s"}\n' "$source" "$tag" "$verdict" "$sha"
