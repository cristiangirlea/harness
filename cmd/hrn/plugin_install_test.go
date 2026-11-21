package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/prefs/prefstest"
)

func fakePluginRelease(t *testing.T, tag, endpoint, problem string) (*atomic.Int32, string) {
	t.Helper()
	var binary []byte
	if endpoint != "" && problem != "missing asset" && problem != "missing sums" {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		binary, err = os.ReadFile(exe)
		if err != nil {
			t.Fatal(err)
		}
	}
	sum := fmt.Sprintf("%x", sha256.Sum256(binary))
	asset := "hrn-echo_" + strings.TrimPrefix(tag, "v") + "_" + runtime.GOOS + "_" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		asset += ".exe"
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("plugin release request sent credentials")
		}
		switch {
		case endpoint != "" && r.URL.Path == endpoint:
			origin := "http://" + r.Host
			var assets []map[string]string
			if problem != "missing asset" {
				assets = append(assets, map[string]string{"name": asset, "browser_download_url": origin + "/binary"})
			}
			if problem != "missing sums" {
				assets = append(assets, map[string]string{"name": "SHA256SUMS", "browser_download_url": origin + "/sums"})
			}
			json.NewEncoder(w).Encode(map[string]any{"tag_name": tag, "assets": assets})
		case r.URL.Path == "/sums":
			digest := sum
			if problem == "checksum" {
				digest = strings.Repeat("0", 64)
			}
			fmt.Fprintf(w, "%s  %s\n", digest, asset)
		case r.URL.Path == "/binary":
			w.Write(binary)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	previous := pluginReleaseAPIBase
	pluginReleaseAPIBase = server.URL
	t.Cleanup(func() { pluginReleaseAPIBase = previous })
	return &requests, sum
}

func TestPluginInstallMakesTheVerifiedPluginAvailableForCallsAndListing(t *testing.T) {
	home := useEmptyPluginHome(t)
	prefstest.OrgPath(t)
	requests, sum := fakePluginRelease(t, "v1.2.3", "/repos/owner/hrn-echo/releases/latest", "")
	out, _, err := runPluginCommand(t, "", 0, "install", "owner/hrn-echo", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Name, Version, Tag, Source, Path, SHA256 string
		Capabilities                             []struct{ Name string }
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	file := "hrn-echo"
	if runtime.GOOS == "windows" {
		file += ".exe"
	}
	dir := filepath.Join(home, "plugins", "echo")
	got, gotErr := filepath.EvalSymlinks(result.Path)
	want, wantErr := filepath.EvalSymlinks(filepath.Join(dir, file))
	if gotErr != nil || wantErr != nil || got != want || result.Name != "echo" ||
		result.Version != fixturePluginVersion || result.Tag != "v1.2.3" || result.Source != "owner/hrn-echo" ||
		result.SHA256 != sum || len(result.Capabilities) == 0 || result.Capabilities[0].Name != "echo" || requests.Load() != 3 {
		t.Fatalf("install = %s, resolved path = %q (%v), want %q (%v), requests = %d", out, got, gotErr, want, wantErr, requests.Load())
	}
	requireNoPluginRuns(t)
	recordPath := filepath.Join(dir, "install.json")
	raw, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]string
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	installedAt, err := time.Parse(time.RFC3339, record["installed_at"])
	if err != nil || installedAt.Location() != time.UTC || record["source"] != result.Source ||
		record["tag"] != result.Tag || record["sha256"] != sum ||
		record["asset"] != strings.Replace(file, "echo", "echo_1.2.3_"+runtime.GOOS+"_"+runtime.GOARCH, 1) {
		t.Fatalf("install record = %s", raw)
	}
	out, _, err = runPluginCommand(t, "", 0, "call", "echo", "echo", "--input", `{"hello":"world"}`)
	if err != nil || !jsonEqual([]byte(out), `{"hello":"world"}`) {
		t.Fatalf("call = %s, %v", out, err)
	}
	for _, flags := range [][]string{nil, {"--json"}} {
		out, _, err = runPluginCommand(t, "", 0, append([]string{"list"}, flags...)...)
		if err != nil || !strings.Contains(out, result.Source) {
			t.Fatalf("list %v = %s, %v", flags, out, err)
		}
	}
	prefstest.Write(t, recordPath, "{")
	out, _, err = runPluginCommand(t, "", 0, "list", "--json")
	if err != nil || !strings.Contains(out, `"name":"echo"`) || strings.Contains(out, `"source"`) {
		t.Fatalf("list with broken metadata = %s, %v", out, err)
	}
}

func TestPluginInstallUsesTheConfiguredSourceAndReplacesAnExistingCopyAtATag(t *testing.T) {
	home := useEmptyPluginHome(t)
	prefstest.OrgPath(t)
	prefstest.Write(t, filepath.Join(home, "backends.json"), `{"plugins":{"sources":{"echo":"Owner/custom"},"allow":["owner/*"]}}`)
	path, _ := installCommandPlugin(t, home, "echo")
	prefstest.Write(t, path, "old binary")
	recordPath := filepath.Join(filepath.Dir(path), "install.json")
	prefstest.Write(t, recordPath, `{"source":"previous/hrn-echo"}`)
	requests, sum := fakePluginRelease(t, "v1.2.3", "/repos/Owner/custom/releases/tags/v1.2.3", "")
	out, _, err := runPluginCommand(t, "", 0, "install", "echo@v1.2.3")
	if err != nil || !strings.Contains(out, "installed echo v1.2.3 from Owner/custom") || requests.Load() != 3 {
		t.Fatalf("install = %s, %v, requests = %d", out, err, requests.Load())
	}
	binary, err := os.ReadFile(path)
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(binary)) != sum {
		t.Fatalf("installed copy was not replaced: %v", err)
	}
	record, err := os.ReadFile(recordPath)
	if err != nil || !strings.Contains(string(record), `"source":"Owner/custom"`) {
		t.Fatalf("install record was not replaced: %s, %v", record, err)
	}
	backups, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".previous-*"))
	if err != nil || len(backups) != 0 {
		t.Fatalf("backups after replacement = %v, %v", backups, err)
	}
	entries, err := os.ReadDir(filepath.Join(home, "plugins"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".staging-") {
			t.Fatalf("staging directory remains after installation: %s", entry.Name())
		}
	}
}

func TestPluginInstallRefusesInvalidArgumentsAndDisallowedSourcesBeforeNetworking(t *testing.T) {
	for _, tc := range []struct {
		name, arg, user, org, want string
		code                       int
		flags                      []string
	}{
		{name: "unknown", arg: "echo", want: "hrn plugin install OWNER/REPO", code: 2},
		{name: "user off", arg: "owner/hrn-echo", user: `{"plugins":{"install":"off"}}`, want: "your backends.json", code: 1},
		{name: "org off", arg: "owner/hrn-echo", user: `{"plugins":{"install":"on"}}`, org: `{"plugins":{"install":"off"},"locked":["plugins"]}`, want: "organisation's backends.json", code: 1},
		{name: "org off inherited", arg: "owner/hrn-echo", user: `{"enabled":["claude"]}`, org: `{"plugins":{"install":"off"}}`, want: "organisation's backends.json", code: 1},
		{name: "allowlist", arg: "owner/hrn-echo", user: `{"plugins":{"allow":["other/*"]}}`, want: "owner/hrn-echo", code: 1},
		{name: "tag", arg: "owner/hrn-echo@.bad", want: "invalid release tag", code: 2},
		{name: "reserved", arg: "owner/hrn-server", want: "server", code: 2},
		{name: "short name override", arg: "echo", flags: []string{"--name", "other"}, want: "--name requires OWNER/REPO", code: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := useEmptyPluginHome(t)
			orgPath := prefstest.OrgPath(t)
			if tc.org != "" {
				prefstest.Write(t, orgPath, tc.org)
			}
			if tc.user != "" {
				prefstest.Write(t, filepath.Join(home, "backends.json"), tc.user)
			}
			requests, _ := fakePluginRelease(t, "", "", "")
			_, _, err := runPluginCommand(t, "", tc.code, append([]string{"install", tc.arg}, tc.flags...)...)
			if err == nil || !strings.Contains(err.Error(), tc.want) || requests.Load() != 0 {
				t.Fatalf("error = %v, want %q, requests = %d", err, tc.want, requests.Load())
			}
		})
	}
}

func TestPluginInstallRefusesWrongAndInvalidReleaseTags(t *testing.T) {
	for _, tc := range []struct{ name, ref, endpoint, tag, want string }{
		{"wrong requested tag", "owner/hrn-echo@v1.2.3", "tags/v1.2.3", "v9", `release returned tag "v9", want "v1.2.3"`},
		{"invalid latest tag", "owner/hrn-echo", "latest", "v1!2", `invalid asset name "" for tag "v1!2"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := useEmptyPluginHome(t)
			prefstest.OrgPath(t)
			fakePluginRelease(t, tc.tag, "/repos/owner/hrn-echo/releases/"+tc.endpoint, "")
			_, _, err := runPluginCommand(t, "", 1, "install", tc.ref)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			entries, err := os.ReadDir(filepath.Join(home, "plugins"))
			if err != nil || len(entries) != 0 {
				t.Fatalf("plugins after failure = %v, %v", entries, err)
			}
		})
	}
}

func TestPluginInstallCleansUpAfterDownloadAndHandshakeFailures(t *testing.T) {
	for _, tc := range []struct{ problem, want string }{
		{"checksum", "checksum mismatch"},
		{"missing asset", "has no asset"},
		{"missing sums", "has no SHA256SUMS"},
		{"mismatch", `ready named plugin "other", want "echo"`},
	} {
		t.Run(tc.problem, func(t *testing.T) {
			home := useEmptyPluginHome(t)
			prefstest.OrgPath(t)
			if tc.problem == "mismatch" {
				prefstest.Write(t, filepath.Join(".", ".plugin-fixture-name"), "other")
			}
			fakePluginRelease(t, "v1", "/repos/owner/custom/releases/latest", tc.problem)
			_, _, err := runPluginCommand(t, "", 1, "install", "owner/custom", "--name", "echo")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			entries, err := os.ReadDir(filepath.Join(home, "plugins"))
			if err != nil || len(entries) != 0 {
				t.Fatalf("plugins after failure = %v, %v", entries, err)
			}
			requireNoPluginRuns(t)
		})
	}
}

func TestMissingPluginHintsFollowEffectiveInstallPreferences(t *testing.T) {
	home := useEmptyPluginHome(t)
	orgPath := prefstest.OrgPath(t)
	prefstest.Write(t, orgPath, `{"plugins":{"sources":{"echo":"owner/hrn-echo"}},"locked":["plugins"]}`)
	path := filepath.Join(home, "backends.json")
	prefstest.Write(t, path, `{"plugins":{"install":"off"}}`)
	_, _, err := runPluginCommand(t, "", 1, "info", "echo")
	if err == nil || !strings.Contains(err.Error(), "(install it with: hrn plugin install echo)") {
		t.Fatalf("configured hint = %v", err)
	}
	prefstest.Write(t, orgPath, `{"plugins":{"install":"off"},"locked":["plugins"]}`)
	_, _, err = runPluginCommand(t, "", 1, "call", "echo", "echo")
	if err == nil || !strings.Contains(err.Error(), "in $HRN_HOME/plugins/echo/ or on PATH") {
		t.Fatalf("disabled hint = %v", err)
	}
	prefstest.Write(t, path, "{")
	_, _, err = runPluginCommand(t, "", 1, "info", "echo")
	if err == nil || !strings.Contains(err.Error(), "hrn plugin install OWNER/hrn-echo") {
		t.Fatalf("unavailable preferences hint = %v", err)
	}
}
