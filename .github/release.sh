#!/usr/bin/env bash
# Builds the release binaries for one version into dist/: one static binary per
# platform, named hrn_<version>_<os>_<arch>[.exe], and SHA256SUMS. The release
# workflow runs it on a v* tag; run it by hand to check a release before tagging:
#
#   .github/release.sh v0.2.0
#
# The tag must match agent.Version, so `hrn version` and the release agree. The
# commit is not passed in: go build stamps it from the checkout, and hrn
# version reads it back from the build info.
set -euo pipefail

tag="${1:?usage: release.sh vX.Y.Z}"
version="${tag#v}"
declared="$(sed -n 's/^const Version = "\(.*\)"$/\1/p' internal/agent/agent.go)"
if [ "$version" != "$declared" ]; then
	echo "tag $tag does not match agent.Version $declared; bump internal/agent/agent.go first" >&2
	exit 1
fi

rm -rf dist
mkdir -p dist
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do
	os="${target%/*}"
	arch="${target#*/}"
	out="dist/hrn_${version}_${os}_${arch}"
	[ "$os" = windows ] && out="$out.exe"
	echo "building $out"
	CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags="-s -w" -o "$out" ./cmd/hrn
done
(cd dist && sha256sum hrn_* > SHA256SUMS)
cat dist/SHA256SUMS
