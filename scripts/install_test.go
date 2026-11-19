// name: install-tests
// summary: Exercise both release installers against a loopback server.
// effects: read, exec, network
// parameters: none
// example: go test -count=1 ./scripts -run '^TestInstall$/./^invalid_api$'
// hash: 0a62184da79216fc037611ca3bcb8b85ca1a72db118841a4ce47c4bea4f49cb3
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

func TestInstall(t *testing.T) {
	for _, windows := range []bool{false, true} {
		script, platform, binary := "install.sh", "linux", "hrn"
		if windows {
			script, platform, binary = "install.ps1", "windows", "hrn.exe"
		}
		t.Run(script, func(t *testing.T) {
			shell, reason := installShell()
			args := []string{script}
			if windows {
				if runtime.GOOS != "windows" {
					t.Skip("requires Windows PowerShell")
				}
				var err error
				shell, err = exec.LookPath("powershell.exe")
				if err != nil {
					t.Skip("powershell.exe is unavailable")
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
			for _, tc := range []struct {
				name, failure string
				onPath        bool
			}{
				{name: "compact"},
				{name: "pretty and replace", onPath: true},
				{name: "pin"},
				{name: "bad digest", failure: "checksum mismatch for " + asset},
				{name: "unlisted", failure: "SHA256SUMS does not list " + asset},
				{name: "missing binary", failure: "release v0.2.0 has no asset " + asset},
				{name: "missing sums", failure: "release v0.2.0 has no SHA256SUMS"},
				{name: "other host", failure: "asset URL is not allowed"},
				{name: "plain http", failure: "asset URL is not allowed"},
				{name: "other port", failure: "asset URL is not allowed"},
				{name: "invalid api", failure: apiError},
				{name: "plain http api", failure: apiError},
				{name: "in use", failure: "could not replace hrn.exe; close hrn and run again"},
			} {
				if tc.name == "in use" && !windows {
					continue
				}
				t.Run(tc.name, func(t *testing.T) {
					parent := installTemp(t)
					home := installTemp(t)
					dir := filepath.Join(parent, "install space")
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
					type releaseAsset struct {
						Name string `json:"name"`
						URL  string `json:"browser_download_url"`
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
						case "/releases/latest", "/releases/tags/v0.2.0":
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
							}
							assets := []releaseAsset{{"unrelated", base + "/unused"}}
							if tc.name != "missing binary" {
								assets = append(assets, releaseAsset{asset, binURL})
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
							_, _ = w.Write(data)
						case "/sums":
							digest := fmt.Sprintf("%x", sha256.Sum256(data))
							switch tc.name {
							case "bad digest":
								digest = strings.Repeat("0", 64)
							case "unlisted":
								fmt.Fprintf(w, "%s  unrelated\n", digest)
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
					case "invalid api":
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
						cmd := exec.CommandContext(ctx, shell, args...)
						cmd.Env = []string{"PATH=" + path, "HOME=" + home, "LOCALAPPDATA=" + home,
							"HRN_INSTALL_DIR=" + dir, "HRN_INSTALL_API=" + api, "HRN_INSTALL_VERSION=" + version,
							"HRN_INSTALL_OS=linux", "HRN_INSTALL_ARCH=amd64"}
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
						if tc.failure != "" {
							var result map[string]string
							if _, ok := err.(*exec.ExitError); !ok || ctx.Err() != nil || len(stdout) != 0 ||
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
						if tc.failure == "" || tc.name == "bad digest" || tc.name == "unlisted" || tc.name == "in use" {
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
									folder == dir && (tc.failure == "" || tc.name == "in use") && entry.Name() == binary {
									continue
								}
								t.Errorf("unexpected file after install: %s", filepath.Join(folder, entry.Name()))
							}
						}
					}
					run()
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
