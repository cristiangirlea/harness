#!/bin/sh
# name: install
# summary: Install a checksum-verified hrn release on Linux or macOS.
# effects: read, exec, network
# parameters: HRN_INSTALL_DIR:path=$HOME/.local/bin; HRN_INSTALL_VERSION:tag=latest; HRN_INSTALL_API:test-url=github; HRN_INSTALL_OS:test-os=uname; HRN_INSTALL_ARCH:test-arch=uname
# example: out=$(HRN_INSTALL_API=ftp://example.invalid sh scripts/install.sh 2>&1) && exit 1; test "$out" = '{"error":"HRN_INSTALL_API must use https or loopback http"}'
# hash: 6d67a791a6be17ea4b906a77751f5289b52f3237822c3573530610212638abb7

json_string() {
	HRN_JSON_STRING=$1 LC_ALL=C awk 'BEGIN {
		for (i = 1; i < 32; i++) escape[sprintf("%c", i)] = sprintf("\\u%04x", i)
		escape["\\"] = "\\\\"
		escape["\""] = "\\\""
		s = ENVIRON["HRN_JSON_STRING"]
		printf "\""
		for (i = 1; i <= length(s); i++) {
			c = substr(s, i, 1)
			printf "%s", (c in escape ? escape[c] : c)
		}
		printf "\""
	}'
}

fail() {
	printf '{"error":%s}\n' "$(json_string "$1")" >&2
	exit 1
}

parse_url() {
	case $1 in
		https://*|http://*) ;;
		*) return 1 ;;
	esac
	scheme=${1%%:*}
	authority=${1#*://}
	authority=${authority%%[/?#]*}
	case $authority in
		\[*\]*)
			host=${authority%%]*}]
			port=${authority#"$host"}
			literal=${host#\[}
			literal=${literal%\]}
			case $literal in *[!a-fA-F0-9:.]*) return 1 ;; *:*) ;; *) return 1 ;; esac ;;
		*) host=${authority%%:*}; port=${authority#"$host"}
			case $host in ''|*[!a-zA-Z0-9._-]*) return 1 ;; esac ;;
	esac
	case $port in
		'') ;;
		:*) case ${port#:} in ''|*[!0-9]*) return 1 ;; esac ;;
		*) return 1 ;;
	esac
	case $1 in *\\*|*[[:space:][:cntrl:]]*) return 1 ;; esac
}

asset_allowed() {
	parse_url "$1" || return 1
	# Match the same origin and GitHub suffixes used by internal/update/release.go's assetURLAllowed.
	[ "$scheme://$authority" != "$api_origin" ] || return 0
	[ "$scheme" = https ] || return 1
	case $(printf '%s' "$host" | tr '[:upper:]' '[:lower:]') in
		github.com|*.github.com|githubusercontent.com|*.githubusercontent.com) return 0 ;;
		*) return 1 ;;
	esac
}

plain_tag() {
	case $1 in ''|v|*..*|*[!a-zA-Z0-9._+-]*) return 1 ;; esac
}

fetch() {
	if [ "$downloader" = curl ]; then
		protocol=https
		case $1 in http://*) protocol=http,https ;; esac
		curl -q -g -fsSL --proto "=$protocol" --proto-redir "=$protocol" -H "Accept: $2" "$1"
	else
		wget --no-config --no-netrc -qO- --header="Accept: $2" "$1"
	fi
}

release_fields() {
	LC_ALL=C awk -v platform="$platform" -v arch="$arch" '
		function token(    ch, size) {
			sub(/^[[:space:]]+/, "", rest)
			ch = substr(rest, 1, 1)
			if (ch == "\"") {
				if (!match(rest, /^"([^"\\[:cntrl:]]|\\(["\\\/bfnrt]|u[0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F]))*"/))
					exit 1
				value = substr(rest, 2, RLENGTH - 2)
				size = RLENGTH
				gsub(/\\\//, "/", value)
				rest = substr(rest, size + 1)
				return "string"
			}
			if (match(rest, /^(true|false|null|-?[0-9]+([.][0-9]+)?([eE][+-]?[0-9]+)?)/)) {
				rest = substr(rest, RLENGTH + 1)
				return "literal"
			}
			rest = substr(rest, 2)
			return ch
		}
		function read_value(kind, role,    next_kind, key, name, url) {
			if (kind == "{") {
				next_kind = token()
				while (next_kind == "string") {
					key = value
					if (token() != ":") exit 1
					next_kind = token()
					if (next_kind == "string") {
						if (role == "root" && key == "tag_name") tag = value
						if (role == "asset" && key == "name") name = value
						if (role == "asset" && key == "browser_download_url") url = value
					}
					read_value(next_kind, role == "root" && key == "assets" ? "assets" : "")
					next_kind = token()
					if (next_kind != ",") break
					next_kind = token()
				}
				if (next_kind != "}") exit 1
				if (role == "asset" && name != "") urls[name] = url
			} else if (kind == "[") {
				next_kind = token()
				if (next_kind == "]") return
				while (1) {
					read_value(next_kind, role == "assets" ? "asset" : "")
					next_kind = token()
					if (next_kind == "]") break
					if (next_kind != ",") exit 1
					next_kind = token()
				}
			} else if (kind != "string" && kind != "literal") exit 1
		}
		{ rest = rest $0 "\n" }
		END {
			if (token() != "{") exit 1
			read_value("{", "root")
			if (token() != "") exit 1
			version = tag
			sub(/^v/, "", version)
			print tag
			print urls["hrn_" version "_" platform "_" arch]
			print urls["SHA256SUMS"]
			print "."
		}'
}

api=${HRN_INSTALL_API:-https://api.github.com/repos/cristiangirlea/harness/releases/latest}
parse_url "$api" || fail 'HRN_INSTALL_API must use https or loopback http'
if [ "$scheme" = http ]; then
	case $host in
		127.0.0.1|'[::1]') ;;
		*) fail 'HRN_INSTALL_API must use https or loopback http' ;;
	esac
fi
api_origin=$scheme://$authority
pin=${HRN_INSTALL_VERSION:-}
if [ -n "$pin" ]; then
	plain_tag "$pin" || fail 'HRN_INSTALL_VERSION must be a plain version tag'
	api_path=${api%%[?#]*}
	case $api_path in
		*/releases/latest) api=${api_path%/latest}/tags/$pin${api#"$api_path"} ;;
		*) fail 'HRN_INSTALL_API must end in /releases/latest to pin a version' ;;
	esac
fi

system=${HRN_INSTALL_OS:-$(uname -s 2>/dev/null)}
case $system in
	Linux|linux) platform=linux ;;
	Darwin|darwin) platform=darwin ;;
	*) fail "unsupported OS: $system" ;;
esac
machine=${HRN_INSTALL_ARCH:-$(uname -m 2>/dev/null)}
case $machine in
	x86_64|amd64) arch=amd64 ;;
	aarch64|arm64) arch=arm64 ;;
	*) fail "unsupported architecture: $machine" ;;
esac

if command -v curl >/dev/null 2>&1; then downloader=curl
elif command -v wget >/dev/null 2>&1; then downloader=wget
else fail 'curl or wget is required'; fi
if command -v sha256sum >/dev/null 2>&1; then
	set -- sha256sum
elif command -v shasum >/dev/null 2>&1; then
	set -- shasum -a 256
else fail 'sha256sum or shasum is required'; fi

release=$(fetch "$api" application/vnd.github+json 2>/dev/null) || fail 'could not fetch release'
fields=$(printf '%s\n' "$release" | release_fields 2>/dev/null) || fail 'invalid release response'
{
	IFS= read -r tag
	IFS= read -r binary_url
	IFS= read -r sums_url
} <<EOF
$fields
EOF
plain_tag "$tag" || fail 'release has no plain version tag'
[ -z "$pin" ] || [ "$tag" = "$pin" ] || fail 'release tag does not match HRN_INSTALL_VERSION'
asset=hrn_${tag#v}_${platform}_${arch}
[ -n "$binary_url" ] || fail "release $tag has no asset $asset"
[ -n "$sums_url" ] || fail "release $tag has no SHA256SUMS"
asset_allowed "$binary_url" && asset_allowed "$sums_url" || fail 'asset URL is not allowed'

dir=${HRN_INSTALL_DIR:-}
if [ -z "$dir" ]; then
	[ -n "${HOME:-}" ] || fail 'HOME is required'
	dir=$HOME/.local/bin
fi
case $dir in /*|[a-zA-Z]:*) ;; *) dir=./$dir ;; esac
(umask 022; mkdir -p "$dir") 2>/dev/null || fail 'could not create install directory'
dir=$(CDPATH='' cd -P "$dir" 2>/dev/null && pwd -P) || fail 'could not open install directory'
temp=$(mktemp -d "$dir/.hrn-install.XXXXXXXX" 2>/dev/null) || fail 'could not create install temp directory'
trap 'rm -rf "$temp" 2>/dev/null' 0
trap 'fail "installation interrupted"' HUP INT TERM
fetch "$binary_url" application/octet-stream >"$temp/$asset" 2>/dev/null || fail "could not download $asset"
fetch "$sums_url" application/octet-stream >"$temp/SHA256SUMS" 2>/dev/null || fail 'could not download SHA256SUMS'
want=$(LC_ALL=C awk -v name="$asset" '
	{ sub(/\r$/, ""); if (NF == 2 && ($2 == name || $2 == "*" name)) { print tolower($1); exit } }
' "$temp/SHA256SUMS" 2>/dev/null) || fail 'could not read SHA256SUMS'
[ -n "$want" ] || fail "SHA256SUMS does not list $asset"
digest=$("$@" <"$temp/$asset" 2>/dev/null) || fail "could not hash $asset"
[ "$want" = "${digest%% *}" ] || fail "checksum mismatch for $asset"

dest=$dir/hrn
[ ! -d "$dest" ] && chmod 0755 "$temp/$asset" 2>/dev/null &&
	mv -f "$temp/$asset" "$dest" 2>/dev/null || fail 'could not replace hrn'
on_path=false
remaining=${PATH:-}:
while [ -n "$remaining" ]; do
	entry=${remaining%%:*}
	remaining=${remaining#*:}
	case $entry in /*) ;; *) entry=./$entry ;; esac
	resolved=$(CDPATH='' cd -P "$entry" 2>/dev/null && pwd -P)
	if [ "$resolved" = "$dir" ]; then on_path=true; break; fi
done
if [ "$on_path" = false ]; then
	quoted=$(printf '%s' "$dir" | sed "s/'/'\\\\''/g")
	printf 'Add this line to your shell profile: export PATH=%s:"$PATH"\n' "'$quoted'" >&2
fi
printf '{"installed":%s,"version":%s,"on_path":%s}\n' "$(json_string "$dest")" "$(json_string "$tag")" "$on_path"
