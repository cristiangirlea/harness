// name: install-tests
// summary: Exercise both release installers against a loopback server.
// effects: read, exec, network
// parameters: none
// example: go test -count=1 ./scripts -run '^TestInstall$/./^invalid_api$'
// hash: 74ad4188e4a169f47550e1bebb757855aabd276b26322ae14405f2c7df7143c0
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func installShell() (string, string) {
	if dash, err := exec.LookPath("dash"); err == nil {
		return dash, ""
	}
	if sh, err := exec.LookPath("sh"); err == nil {
		return sh, ""
	}
	if runtime.GOOS == "windows" {
		if git, err := exec.LookPath("git"); err == nil {
			root := filepath.Dir(filepath.Dir(git))
			for _, dir := range []string{"bin", "usr/bin"} {
				sh := filepath.Join(root, dir, "sh.exe")
				if _, err := os.Stat(sh); err == nil {
					return sh, ""
				}
			}
		}
		if bash, err := exec.LookPath("bash"); err == nil {
			system := filepath.Join(os.Getenv("SystemRoot"), "System32")
			if !strings.EqualFold(filepath.Dir(bash), system) {
				return bash, ""
			}
			return "", "only the Windows system bash launcher is available"
		}
	}
	return "", "no POSIX shell found"
}

func installTemp(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestInstallDownloader(t *testing.T) {
	shell, reason := installShell()
	if reason != "" {
		t.Skip(reason)
	}
	for _, tc := range []struct {
		name, api, failure string
	}{
		{"busybox", "https://api.github.com/releases/latest", "curl or GNU wget is required"},
		{"curl flags", "https://api.github.com/releases/latest", "release has no plain version tag"},
		{"gnu wget", "https://api.github.com/releases/latest", "release has no plain version tag"},
		{"wget downgrade", "https://api.github.com/releases/latest", "could not fetch release"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := installTemp(t)
			fake := map[string]string{
				"sha256sum": "exit 99",
				"wget":      `[ "$1" = --version ] && printf 'BusyBox wget\n'`,
			}
			if tc.name == "curl flags" {
				fake["curl"] = `[ "$1" = -q ] || exit 99
flags=0
while [ "$#" -gt 0 ]; do
	case $1 in
		--proto|--proto-redir) [ "$2" = =https ] || exit 99; flags=$((flags + 1)); shift ;;
		--max-redirs) [ "$2" = 5 ] || exit 99; flags=$((flags + 1)); shift ;;
		--fail) flags=$((flags + 1)) ;;
		--output) output=$2; shift ;;
	esac
	shift
done
[ "$flags" -eq 4 ] || exit 99
printf '{}' >"$output"
printf '200 '`
			}
			if tc.name == "gnu wget" || tc.name == "wget downgrade" {
				location := "/release"
				if tc.name == "wget downgrade" {
					location = "http://api.github.com/release"
				}
				fake["wget"] = `if [ "$1" = --version ]; then printf 'GNU Wget 1.21\n'; exit; fi
case " $* " in
	*" --https-only --max-redirect=0 "*) ;;
	*) exit 99 ;;
esac
while [ "$#" -gt 1 ]; do
	if [ "$1" = -O ]; then output=$2; shift; fi
	shift
done
case $1 in
	*/latest) printf '  HTTP/1.1 302 Found\n  Location: %s\n' ' ` + location + `' >&2; exit 8 ;;
	*/release) printf '{}' >"$output" ;;
esac`
			}
			for name, contents := range fake {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+contents+"\n"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(shell, "-c", `
bin=$(CDPATH='' cd "$1" && pwd -P)
for tool in awk cat mktemp rm tr; do
	printf '#!/bin/sh\nexec "%s" "$@"\n' "$(command -v "$tool")" > "$bin/$tool"
	chmod +x "$bin/$tool"
done
PATH=$bin
export PATH
. ./install.sh`, "sh", dir)
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "TMPDIR=" + dir,
				"HRN_INSTALL_API=" + tc.api, "HRN_INSTALL_VERSION=", "HRN_INSTALL_OS=linux", "HRN_INSTALL_ARCH=amd64"}
			if runtime.GOOS == "windows" {
				for _, key := range []string{"SystemRoot", "WINDIR", "ComSpec", "PATHEXT"} {
					cmd.Env = append(cmd.Env, key+"="+os.Getenv(key))
				}
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			stdout, err := cmd.Output()
			var result map[string]string
			if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 1 || len(stdout) != 0 ||
				json.Unmarshal(stderr.Bytes(), &result) != nil ||
				!reflect.DeepEqual(result, map[string]string{"error": tc.failure}) {
				t.Fatalf("expected %q, got %v stdout %q stderr %q", tc.failure, err, stdout, &stderr)
			}
			assertNoFetchTemp(t, dir)
		})
	}
}

func assertNoFetchTemp(t *testing.T, dir string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "hrn-fetch.*"))
	if err != nil || len(files) != 0 {
		t.Fatalf("leftover download files: %v (%v)", files, err)
	}
}

func TestInstall(t *testing.T) {
	for _, interpreter := range []string{"sh", "powershell.exe", "pwsh"} {
		windows := interpreter != "sh"
		script, platform, binary := "install.sh", "linux", "hrn"
		if windows {
			script, platform, binary = "install.ps1", "windows", "hrn.exe"
		}
		suite := script
		if interpreter == "pwsh" {
			suite += "_pwsh"
		}
		t.Run(suite, func(t *testing.T) {
			shell, reason := installShell()
			args := []string{script}
			if windows {
				if runtime.GOOS != "windows" {
					t.Skip("requires Windows PowerShell")
				}
				var err error
				shell, err = exec.LookPath(interpreter)
				if err != nil {
					t.Skip(interpreter + " is unavailable")
				}
				args = []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script}
			} else if reason != "" {
				t.Skip(reason)
			}

			asset := "hrn_0.2.0_" + platform + "_amd64"
			if windows {
				asset += ".exe"
			}
			const apiError = "HRN_INSTALL_API must use https or loopback http"
			const pinError = "HRN_INSTALL_VERSION must be a plain version tag"
			for _, tc := range []struct {
				name, failure string
				onPath        bool
			}{
				{name: "compact"},
				{name: "pretty and replace", onPath: true},
				{name: "literal directory"},
				{name: "reversed asset fields", failure: "SHA256SUMS does not list " + asset},
				{name: "duplicate asset", failure: "asset URL is not allowed"},
				{name: "pipe success"},
				{name: "pipe refusal", failure: apiError},
				{name: "pin"},
				{name: "wrong tag", failure: "release tag v0.2.0 is not the requested v0.2.1"},
				{name: "invalid pin traversal", failure: pinError},
				{name: "invalid pin v", failure: pinError},
				{name: "invalid pin space", failure: pinError},
				{name: "bad digest", failure: "checksum mismatch for " + asset},
				{name: "short digest", failure: "SHA256SUMS has no valid digest for " + asset},
				{name: "nonhex digest", failure: "SHA256SUMS has no valid digest for " + asset},
				{name: "unlisted", failure: "SHA256SUMS does not list " + asset},
				{name: "suffix only", failure: "SHA256SUMS does not list " + asset},
				{name: "empty sums", failure: "SHA256SUMS does not list " + asset},
				{name: "missing binary", failure: "release v0.2.0 has no asset " + asset},
				{name: "escaped asset name", failure: "release v0.2.0 has no asset " + asset},
				{name: "missing sums", failure: "release v0.2.0 has no SHA256SUMS"},
				{name: "other host", failure: "asset URL is not allowed"},
				{name: "plain http", failure: "asset URL is not allowed"},
				{name: "other port", failure: "asset URL is not allowed"},
				{name: "redirect other port", failure: "could not download " + asset},
				{name: "redirect same origin"},
				{name: "redirect limit", failure: "could not download " + asset},
				{name: "userinfo", failure: "asset URL is not allowed"},
				{name: "invalid api", failure: apiError},
				{name: "plain http api", failure: apiError},
				{name: "destination directory", failure: "could not replace " + binary},
				{name: "in use", failure: "could not replace hrn.exe; close hrn and run again"},
			} {
				piped := strings.HasPrefix(tc.name, "pipe ")
				if (tc.name == "in use" || piped) && !windows {
					continue
				}
				t.Run(tc.name, func(t *testing.T) {
					parent := installTemp(t)
					home := installTemp(t)
					dir := filepath.Join(parent, "install space")
					if tc.name == "literal directory" {
						dir = filepath.Join(parent, "dir [x]")
					}
					if tc.name == "destination directory" {
						if err := os.MkdirAll(filepath.Join(dir, binary), 0755); err != nil {
							t.Fatal(err)
						}
					}
					if tc.name == "in use" {
						if err := os.Mkdir(dir, 0755); err != nil {
							t.Fatal(err)
						}
						path := filepath.Join(dir, binary)
						if err := os.WriteFile(path, []byte("locked binary"), 0755); err != nil {
							t.Fatal(err)
						}
						handle, err := os.Open(path)
						if err != nil {
							t.Fatal(err)
						}
						defer handle.Close()
					}
					redirectURL := ""
					if tc.name == "redirect other port" {
						target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							t.Error("followed redirect to a different port")
							http.NotFound(w, r)
						}))
						defer target.Close()
						redirectURL = target.URL + "/asset"
					}
					var mu sync.Mutex
					var requests []string
					data := []byte("fake hrn\x00\n")
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						mu.Lock()
						defer mu.Unlock()
						requests = append(requests, r.URL.Path)
						if r.Header.Get("Authorization") != "" {
							t.Error("request carried Authorization")
						}
						base := "http://" + r.Host
						switch r.URL.Path {
						case "/releases/latest", "/releases/tags/v0.2.0", "/releases/tags/v0.2.1":
							if r.Header.Get("Accept") != "application/vnd.github+json" {
								t.Errorf("unexpected API Accept header %q", r.Header.Get("Accept"))
							}
							binURL, sumsURL := base+"/asset", base+"/sums"
							switch tc.name {
							case "other host":
								binURL = "https://github.com.example.invalid/asset"
							case "plain http":
								sumsURL = "http://assets.example.invalid/sums"
							case "other port":
								binURL = "http://127.0.0.1:1/asset"
							case "userinfo":
								binURL = "https://github.com@127.0.0.1/asset"
							case "duplicate asset":
								binURL = "https://github.com.example.invalid/asset"
							}
							type releaseAsset struct {
								Name string `json:"name"`
								URL  string `json:"browser_download_url"`
							}
							assets := []any{releaseAsset{"unrelated", base + "/unused"}}
							switch tc.name {
							case "duplicate asset":
								assets = append(assets, releaseAsset{asset, base + "/asset"})
							case "escaped asset name":
								assets = append(assets, releaseAsset{asset + `"unexpected`, binURL})
							}
							if tc.name != "missing binary" {
								switch tc.name {
								case "escaped asset name":
								case "reversed asset fields":
									assets = append(assets, map[string]string{"browser_download_url": binURL, "name": asset})
								default:
									assets = append(assets, releaseAsset{asset, binURL})
								}
							}
							if tc.name != "missing sums" {
								assets = append(assets, releaseAsset{"SHA256SUMS", sumsURL})
							}
							w.Header().Set("Content-Type", "application/json")
							enc := json.NewEncoder(w)
							if tc.onPath {
								enc.SetIndent("", "  ")
							}
							doc := map[string]any{
								"tag_name": "v0.2.0", "assets": assets,
								"body": `Notes with "name", braces {}, and a newline` + "\n",
							}
							if err := enc.Encode(doc); err != nil {
								t.Error(err)
							}
						case "/asset":
							switch tc.name {
							case "redirect other port":
								http.Redirect(w, r, redirectURL, http.StatusFound)
								return
							case "redirect same origin":
								http.Redirect(w, r, "/redirected", http.StatusFound)
								return
							case "redirect limit":
								http.Redirect(w, r, "/asset", http.StatusFound)
								return
							}
							_, _ = w.Write(data)
						case "/redirected":
							_, _ = w.Write(data)
						case "/sums":
							digest := fmt.Sprintf("%x", sha256.Sum256(data))
							switch tc.name {
							case "bad digest":
								digest = strings.Repeat("0", 64)
							case "short digest":
								digest = digest[:63]
							case "nonhex digest":
								digest = "z" + digest[1:]
							case "unlisted":
								fmt.Fprintf(w, "%s  unrelated\n", digest)
								return
							case "suffix only":
								fmt.Fprintf(w, "%s  other-%s\n", digest, asset)
								return
							case "empty sums", "reversed asset fields":
								return
							}
							if tc.onPath {
								fmt.Fprintf(w, "%s *%s\r\n", strings.ToUpper(digest), asset)
							} else {
								fmt.Fprintf(w, "%s  %s\n", digest, asset)
							}
						default:
							http.NotFound(w, r)
						}
					}))
					defer server.Close()

					api := server.URL + "/releases/latest"
					releasePath, version := "/releases/latest", ""
					switch tc.name {
					case "pin":
						releasePath, version = "/releases/tags/v0.2.0", "v0.2.0"
					case "wrong tag":
						releasePath, version = "/releases/tags/v0.2.1", "v0.2.1"
					case "invalid pin traversal":
						releasePath, version = "", "../x"
					case "invalid pin v":
						releasePath, version = "", "v"
					case "invalid pin space":
						releasePath, version = "", "v0.2.0 extra"
					case "invalid api", "pipe refusal":
						api, releasePath = "ftp://example.invalid", ""
					case "plain http api":
						api, releasePath = "http://localhost:1/releases/latest", ""
					}

					path := os.Getenv("PATH")
					if tc.onPath {
						path = dir + string(os.PathListSeparator) + path
					}
					run := func() {
						t.Helper()
						ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
						defer cancel()
						runArgs := args
						if piped {
							runArgs = []string{"-NoProfile", "-Command", `
if ($PSVersionTable.PSVersion.Major -eq 5) {
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls11
}
$global:LASTEXITCODE = 99
Get-Content -Raw -LiteralPath './install.ps1' | Invoke-Expression
$code = $LASTEXITCODE
Write-Output 'session-alive'
if ($PSVersionTable.PSVersion.Major -eq 5 -and
    [Net.ServicePointManager]::SecurityProtocol -ne ([Net.SecurityProtocolType]::Tls11 -bor [Net.SecurityProtocolType]::Tls12)) {
    exit 98
}
exit $code`}
						}
						cmd := exec.CommandContext(ctx, shell, runArgs...)
						cmd.Env = []string{"PATH=" + path, "HOME=" + home, "LOCALAPPDATA=" + home,
							"HRN_INSTALL_DIR=" + dir, "HRN_INSTALL_API=" + api, "HRN_INSTALL_VERSION=" + version,
							"HRN_INSTALL_OS=linux", "HRN_INSTALL_ARCH=amd64", "TMPDIR=" + home}
						if runtime.GOOS == "windows" {
							for _, key := range []string{"SystemRoot", "WINDIR", "ComSpec", "PATHEXT"} {
								cmd.Env = append(cmd.Env, key+"="+os.Getenv(key))
							}
							for _, key := range []string{"USERPROFILE", "APPDATA", "TEMP", "TMP"} {
								cmd.Env = append(cmd.Env, key+"="+home)
							}
						}
						var stderr bytes.Buffer
						cmd.Stderr = &stderr
						stdout, err := cmd.Output()
						if piped {
							marker := []byte("session-alive\n")
							stdout = bytes.ReplaceAll(stdout, []byte("\r\n"), []byte("\n"))
							if !bytes.HasSuffix(stdout, marker) {
								t.Fatalf("PowerShell pipeline ended the session: %v stdout %q stderr %q", err, stdout, &stderr)
							}
							stdout = bytes.TrimSuffix(stdout, marker)
						}
						if tc.failure != "" {
							var result map[string]string
							if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 1 || ctx.Err() != nil || len(stdout) != 0 ||
								json.Unmarshal(stderr.Bytes(), &result) != nil ||
								!reflect.DeepEqual(result, map[string]string{"error": tc.failure}) {
								t.Fatalf("expected %q, got %v stdout %q stderr %q", tc.failure, err, stdout, &stderr)
							}
						} else {
							var result struct {
								Installed string `json:"installed"`
								Version   string `json:"version"`
								OnPath    *bool  `json:"on_path"`
							}
							if err != nil || json.Unmarshal(stdout, &result) != nil {
								t.Fatalf("install: %v stdout %q stderr %q", err, stdout, &stderr)
							}
							want := filepath.Join(dir, binary)
							installed := result.Installed
							if runtime.GOOS == "windows" && !windows && strings.HasPrefix(installed, "/") {
								convert := exec.CommandContext(ctx, shell, "-c", `cygpath -aw "$1"`, "sh", installed)
								convert.Env = cmd.Env
								native, err := convert.Output()
								if err != nil {
									t.Fatalf("resolve installed path %q: %v", installed, err)
								}
								installed = strings.TrimSpace(string(native))
							}
							if !strings.EqualFold(filepath.Clean(installed), want) || result.Version != "v0.2.0" ||
								result.OnPath == nil || *result.OnPath != tc.onPath {
								t.Fatalf("unexpected success object %s", stdout)
							}
							hint := strings.TrimSpace(stderr.String())
							if tc.onPath && hint != "" || !tc.onPath && (!strings.Contains(hint, "PATH") || strings.Contains(hint, "\n")) {
								t.Fatalf("unexpected PATH hint %q", hint)
							}
							mu.Lock()
							wantData := bytes.Clone(data)
							mu.Unlock()
							got, err := os.ReadFile(want)
							if err != nil || !bytes.Equal(got, wantData) {
								t.Fatalf("installed bytes %q, want %q (%v)", got, wantData, err)
							}
							if runtime.GOOS != "windows" {
								info, err := os.Stat(want)
								if err != nil || info.Mode().Perm() != 0755 {
									t.Fatalf("installed executable mode: %v (%v)", info, err)
								}
							}
						}

						mu.Lock()
						got := requests
						requests = nil
						mu.Unlock()
						wantRequests := 0
						if releasePath != "" {
							wantRequests = 1
						}
						switch tc.name {
						case "redirect other port":
							wantRequests = 2
						case "redirect same origin":
							wantRequests = 4
						case "redirect limit":
							wantRequests = 7
						case "bad digest", "short digest", "nonhex digest", "unlisted", "suffix only", "empty sums",
							"reversed asset fields", "in use", "destination directory":
							wantRequests = 3
						}
						if wantRequests == 1 && tc.failure == "" {
							wantRequests = 3
						}
						if len(got) != wantRequests || wantRequests > 0 && got[0] != releasePath {
							t.Fatalf("unexpected requests %v", got)
						}
						for _, folder := range []string{parent, dir} {
							entries, err := os.ReadDir(folder)
							if folder == dir && tc.failure != "" && os.IsNotExist(err) {
								continue
							}
							if err != nil {
								t.Fatal(err)
							}
							for _, entry := range entries {
								if folder == parent && entry.Name() == filepath.Base(dir) ||
									folder == dir && entry.Name() == binary &&
										(tc.failure == "" || tc.name == "in use" || tc.name == "destination directory" && entry.IsDir()) {
									continue
								}
								t.Errorf("unexpected file after install: %s", filepath.Join(folder, entry.Name()))
							}
						}
						assertNoFetchTemp(t, home)
					}
					run()
					if tc.name == "destination directory" {
						entries, err := os.ReadDir(filepath.Join(dir, binary))
						if err != nil || len(entries) != 0 {
							t.Fatalf("destination directory changed: %v (%v)", entries, err)
						}
					}
					if tc.name == "in use" {
						got, err := os.ReadFile(filepath.Join(dir, binary))
						if err != nil || string(got) != "locked binary" {
							t.Fatalf("locked executable changed: %q (%v)", got, err)
						}
					}
					if tc.onPath {
						mu.Lock()
						data = []byte("different executable\n")
						mu.Unlock()
						run()
					}
				})
			}
		})
	}
}
