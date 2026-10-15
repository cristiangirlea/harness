package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAssetNameFollowsTheReleaseScript(t *testing.T) {
	for _, tc := range []struct{ version, goos, goarch, want string }{
		{"0.2.0", "linux", "amd64", "hrn_0.2.0_linux_amd64"},
		{"0.2.0", "darwin", "arm64", "hrn_0.2.0_darwin_arm64"},
		{"0.2.0", "windows", "amd64", "hrn_0.2.0_windows_amd64.exe"},
		{"0.2.0", "windows", "arm64", "hrn_0.2.0_windows_arm64.exe"},
		// A tag carries a v the file name does not.
		{"v0.2.0", "linux", "arm64", "hrn_0.2.0_linux_arm64"},
	} {
		if got := AssetName(tc.version, tc.goos, tc.goarch); got != tc.want {
			t.Errorf("AssetName(%q, %q, %q) = %q; want %q", tc.version, tc.goos, tc.goarch, got, tc.want)
		}
	}
}

const sumsFixture = `0b7e9d3f7f7d2f0a0e1b4a7d3f5e2a1c9b8d7e6f5a4b3c2d1e0f9a8b7c6d5e4f  hrn_0.2.0_darwin_amd64
e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855  hrn_0.2.0_linux_amd64
%s  hrn_0.2.0_windows_amd64.exe
`

func TestVerifyChecksumAcceptsTheListedDigestOnly(t *testing.T) {
	data := []byte("MZ this is not really a binary")
	sum := sha256.Sum256(data)
	sums := fmt.Appendf(nil, sumsFixture, hex.EncodeToString(sum[:]))

	if err := VerifyChecksum(sums, "hrn_0.2.0_windows_amd64.exe", data); err != nil {
		t.Errorf("the listed digest: %v", err)
	}
	if err := VerifyChecksum(sums, "hrn_0.2.0_windows_amd64.exe", append(data, '!')); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Errorf("a changed file: err = %v; want a checksum mismatch", err)
	}
	if err := VerifyChecksum(sums, "hrn_0.2.0_linux_arm64", data); err == nil || !strings.Contains(err.Error(), "hrn_0.2.0_linux_arm64") {
		t.Errorf("a file SHA256SUMS does not list: err = %v; want it named", err)
	}
	if err := VerifyChecksum([]byte("garbage\n"), "hrn_0.2.0_windows_amd64.exe", data); err == nil {
		t.Error("an unreadable SHA256SUMS was accepted")
	}
	// The GNU "binary" marker is tolerated.
	if err := VerifyChecksum([]byte(hex.EncodeToString(sum[:])+" *hrn_0.2.0_windows_amd64.exe\n"), "hrn_0.2.0_windows_amd64.exe", data); err != nil {
		t.Errorf("the binary marker: %v", err)
	}
}

// fakeGitHub serves the releases/latest document and the two files hrn
// update downloads, and records the Authorization headers it saw.
func fakeGitHub(t *testing.T, binary []byte, sums string) (*httptest.Server, *[]string) {
	t.Helper()
	var auth []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/repos/cristiangirlea/harness/releases/latest":
			if r.Header.Get("Accept") != "application/vnd.github+json" {
				t.Errorf("Accept = %q", r.Header.Get("Accept"))
			}
			json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v0.2.0",
				"assets": []map[string]any{
					{"name": "hrn_0.2.0_linux_amd64", "browser_download_url": srv.URL + "/dl/hrn_0.2.0_linux_amd64"},
					{"name": "hrn_0.2.0_windows_amd64.exe", "browser_download_url": srv.URL + "/dl/hrn_0.2.0_windows_amd64.exe"},
					{"name": "SHA256SUMS", "browser_download_url": srv.URL + "/dl/SHA256SUMS"},
				},
			})
		case "/dl/hrn_0.2.0_windows_amd64.exe", "/dl/hrn_0.2.0_linux_amd64":
			w.Write(binary)
		case "/dl/SHA256SUMS":
			w.Write([]byte(sums))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &auth
}

func TestFetchReleaseDownloadsTheAssetForThisPlatformAndChecksIt(t *testing.T) {
	binary := []byte("MZ fake hrn for windows")
	sum := sha256.Sum256(binary)
	srv, auth := fakeGitHub(t, binary, fmt.Sprintf(sumsFixture, hex.EncodeToString(sum[:])))
	dir := t.TempDir()

	src := ReleaseSource{API: srv.URL + "/repos/cristiangirlea/harness/releases/latest", Client: srv.Client(), GOOS: "windows", GOARCH: "amd64"}
	path, tag, err := src.Fetch(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if tag != "v0.2.0" {
		t.Errorf("tag = %q", tag)
	}
	if filepath.Dir(path) != dir || filepath.Base(path) != "hrn_0.2.0_windows_amd64.exe" {
		t.Errorf("path = %q; want the asset under %s", path, dir)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(binary) {
		t.Errorf("downloaded %q, %v", got, err)
	}
	if info, _ := os.Stat(path); info != nil && info.Mode().Perm()&0o100 == 0 && !isWindows() {
		t.Errorf("mode %v; want executable", info.Mode())
	}
	for _, a := range *auth {
		if a != "" {
			t.Errorf("Authorization %q sent without a token", a)
		}
	}
}

func TestFetchReleaseRefusesAChecksumMismatch(t *testing.T) {
	srv, _ := fakeGitHub(t, []byte("MZ tampered"), fmt.Sprintf(sumsFixture, strings.Repeat("00", 32)))
	dir := t.TempDir()
	src := ReleaseSource{API: srv.URL + "/repos/cristiangirlea/harness/releases/latest", Client: srv.Client(), GOOS: "windows", GOARCH: "amd64"}
	_, _, err := src.Fetch(context.Background(), dir)
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("err = %v; want a checksum refusal", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "hrn_") {
			t.Errorf("%s left behind after the refusal", e.Name())
		}
	}
}

func TestFetchReleaseNamesAMissingAssetForThisPlatform(t *testing.T) {
	srv, _ := fakeGitHub(t, nil, "")
	src := ReleaseSource{API: srv.URL + "/repos/cristiangirlea/harness/releases/latest", Client: srv.Client(), GOOS: "darwin", GOARCH: "arm64"}
	_, _, err := src.Fetch(context.Background(), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "hrn_0.2.0_darwin_arm64") {
		t.Fatalf("err = %v; want the missing asset named", err)
	}
}

func TestFetchReleaseSendsTheTokenAsABearerAndNeverInErrors(t *testing.T) {
	srv, auth := fakeGitHub(t, nil, "")
	src := ReleaseSource{API: srv.URL + "/repos/cristiangirlea/harness/releases/latest", Client: srv.Client(), GOOS: "darwin", GOARCH: "arm64", Token: "ghp_secret123"}
	_, _, err := src.Fetch(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("want the missing-asset error")
	}
	if strings.Contains(err.Error(), "ghp_secret123") {
		t.Errorf("the token leaked into %q", err)
	}
	if len(*auth) == 0 || (*auth)[0] != "Bearer ghp_secret123" {
		t.Errorf("Authorization headers %q; want the bearer token on the API call", *auth)
	}
}

func TestFetchReleaseReportsAnAPIFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
	}))
	defer srv.Close()
	src := ReleaseSource{API: srv.URL + "/latest", Client: srv.Client(), GOOS: "linux", GOARCH: "amd64"}
	_, _, err := src.Fetch(context.Background(), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v; want the status", err)
	}
}

func TestFetchReleaseRefusesAssetsOffGitHubAndKeepsTheTokenForTheAPI(t *testing.T) {
	for _, u := range []string{"http://203.0.113.9/hrn_0.2.0_windows_amd64.exe", "https://evil.example/hrn_0.2.0_windows_amd64.exe", "ftp://github.com/x"} {
		var auth []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth = append(auth, r.Header.Get("Authorization"))
			json.NewEncoder(w).Encode(map[string]any{"tag_name": "v0.2.0", "assets": []map[string]any{
				{"name": "hrn_0.2.0_windows_amd64.exe", "browser_download_url": u},
				{"name": "SHA256SUMS", "browser_download_url": u + ".sums"},
			}})
		}))
		src := ReleaseSource{API: srv.URL + "/latest", Client: srv.Client(), GOOS: "windows", GOARCH: "amd64", Token: "ghp_secret123"}
		_, _, err := src.Fetch(context.Background(), t.TempDir())
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), "not a GitHub") {
			t.Fatalf("%s: err = %v; want a refusal naming GitHub", u, err)
		}
		if len(auth) != 1 || auth[0] != "Bearer ghp_secret123" {
			t.Fatalf("%s: Authorization headers %q; want one, on the API call only", u, auth)
		}
	}
}

func TestFetchReleaseSendsNoTokenToTheAssetHost(t *testing.T) {
	binary := []byte("binary bytes")
	sum := sha256.Sum256(binary)
	srv, auth := fakeGitHub(t, binary, hex.EncodeToString(sum[:])+"  hrn_0.2.0_windows_amd64.exe\n")
	src := ReleaseSource{API: srv.URL + "/repos/cristiangirlea/harness/releases/latest", Client: srv.Client(), GOOS: "windows", GOARCH: "amd64", Token: "ghp_secret123"}
	if _, _, err := src.Fetch(context.Background(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if len(*auth) != 3 || (*auth)[0] != "Bearer ghp_secret123" || (*auth)[1] != "" || (*auth)[2] != "" {
		t.Fatalf("Authorization headers %q; want the bearer on the API call and none on the two downloads", *auth)
	}
}

func TestFetchReleaseRefusesATagThatLeavesTheDirectory(t *testing.T) {
	for _, tag := range []string{"../../evil", "v..", "a/b", `a\b`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			name := AssetName(tag, "windows", "amd64")
			json.NewEncoder(w).Encode(map[string]any{"tag_name": tag, "assets": []map[string]any{
				{"name": name, "browser_download_url": "http://" + r.Host + "/dl/x"},
				{"name": "SHA256SUMS", "browser_download_url": "http://" + r.Host + "/dl/SHA256SUMS"},
			}})
		}))
		src := ReleaseSource{API: srv.URL + "/latest", Client: srv.Client(), GOOS: "windows", GOARCH: "amd64"}
		dir := t.TempDir()
		_, _, err := src.Fetch(context.Background(), dir)
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), "tag") {
			t.Fatalf("%q: err = %v; want a refusal naming the tag", tag, err)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Fatalf("%q: something was written into the download directory: %v", tag, entries)
		}
	}
}

func TestAssetURLAllowedAcceptsGitHubHostsOverHTTPSOnly(t *testing.T) {
	api := "https://api.github.com/repos/cristiangirlea/harness/releases/latest"
	for _, u := range []string{
		"https://github.com/cristiangirlea/harness/releases/download/v0.2.0/hrn_0.2.0_linux_amd64",
		"https://objects.githubusercontent.com/x",
		"https://githubusercontent.com/x",
		"https://release-assets.github.com/x",
	} {
		if err := assetURLAllowed(api, u); err != nil {
			t.Errorf("%s refused: %v", u, err)
		}
	}
	for _, u := range []string{"http://github.com/x", "https://github.com.evil.example/x", "https://notgithub.com/x", "x"} {
		if err := assetURLAllowed(api, u); err == nil {
			t.Errorf("%s accepted", u)
		}
	}
}
