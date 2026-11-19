package prefs

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestOrgPath(t *testing.T) {
	before, err := OrgPath()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && before != "/etc/hrn/backends.json" {
		t.Fatalf("organisation path = %q", before)
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ProgramData", "ALLUSERSPROFILE", "SystemDrive"} {
		t.Setenv(name, dir)
	}
	if got, err := OrgPath(); err != nil || got != before || strings.HasPrefix(strings.ToLower(got), strings.ToLower(dir)) {
		t.Fatalf("organisation path = %q, before = %q, error = %v", got, before, err)
	}
}

func TestLoadOrg(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	useOrgPath(t, filepath.Join(dir, "backends.json"), true)
	if got, err := LoadOrg(); err != nil || !reflect.DeepEqual(got, Org{}) {
		t.Fatalf("missing: %+v, %v", got, err)
	}
	for _, tc := range []struct{ name, raw string }{
		{"malformed", "{"},
		{"trailing", "{}{}"},
		{"null", "null"},
		{"unknown field", `{"xyz":true}`},
		{"unknown nested field", `{"providers":{"claude":{"xyz":true}}}`},
		{"invalid content", `{"billing":"private-value"}`},
		{"invalid ceiling", `{"billing":"metered","billing_max_cost_usd":0}`},
		{"unknown lock", `{"locked":["private-value"]}`},
		{"duplicate lock", `{"locked":["enabled","enabled"]}`},
		{"personal sessions", `{"work_map_sessions":null}`},
		{"personal github", `{"work_map_github":false}`},
		{"personal repos", `{"work_map_repos":[]}`},
		{"over cap", "{}" + strings.Repeat(" ", 1<<20)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(orgPathOverride, []byte(tc.raw), 0600); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.Remove(orgPathOverride); err != nil {
					t.Error(err)
				}
			})
			_, err := LoadOrg()
			if err == nil || !strings.Contains(err.Error(), "the organisation's backends.json") || strings.Contains(err.Error(), dir) || strings.Contains(err.Error(), "private-value") {
				t.Fatalf("organisation error = %v", err)
			}
		})
	}
	if err := os.WriteFile(orgPathOverride, []byte(`{"enabled":["claude"],"locked":["enabled"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadOrg(); err != nil || !slices.Equal(got.EnabledNames, []string{"claude"}) || !slices.Equal(got.Locked, []string{"enabled"}) {
		t.Fatalf("valid: %+v, %v", got, err)
	}
}

func useOrgPath(t *testing.T, path string, trusted bool) {
	t.Helper()
	old, oldTrusted := orgPathOverride, orgPathTrusted
	orgPathOverride, orgPathTrusted = path, trusted
	t.Cleanup(func() { orgPathOverride, orgPathTrusted = old, oldTrusted })
}

func requireOrgError(t *testing.T, message string) {
	t.Helper()
	if _, err := LoadOrg(); !errors.Is(err, ErrOrganisation) || err.Error() != "the organisation's backends.json: "+message {
		t.Fatalf("organisation policy error = %v, want %q", err, message)
	}
}

func TestOrgOpenErrorKeepsOnlyMissingApartFromCouldNotBeRead(t *testing.T) {
	if err := orgOpenError(fs.ErrNotExist); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing file must stay missing, got %v", err)
	}
	for _, err := range []error{fs.ErrPermission, errors.New("sharing violation"), io.ErrUnexpectedEOF} {
		got := orgOpenError(err)
		if !errors.Is(got, ErrOrganisation) || got.Error() != "the organisation's backends.json: could not be read" {
			t.Errorf("open error %v became %v", err, got)
		}
	}
}

func TestLoadOrgPathChecks(t *testing.T) {
	for _, name := range []string{"hard link", "symlink file", "symlink folder", "directory as file", "missing file", "missing folder", "unreadable file"} {
		t.Run(name, func(t *testing.T) {
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "backends.json")
			target := filepath.Join(dir, "policy.json")
			write := func(name string) {
				t.Helper()
				if err := os.WriteFile(name, []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			symlink := func(old, new string) {
				t.Helper()
				if err := os.Symlink(old, new); err != nil {
					t.Skipf("cannot create symlink: %v", err)
				}
			}
			switch name {
			case "hard link":
				write(path)
				if err := os.Link(path, target); err != nil {
					t.Fatal(err)
				}
			case "symlink file":
				write(target)
				symlink(target, path)
			case "symlink folder":
				write(path)
				link := filepath.Join(dir, "linked")
				symlink(dir, link)
				path = filepath.Join(link, "backends.json")
			case "directory as file":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "missing folder":
				path = filepath.Join(dir, "missing", "backends.json")
			case "unreadable file":
				write(path)
				makeOrgUnreadable(t, path)
			}
			useOrgPath(t, path, true)
			switch name {
			case "missing file", "missing folder":
				if got, err := LoadOrg(); err != nil || !reflect.DeepEqual(got, Org{}) {
					t.Fatalf("missing policy = %+v, %v", got, err)
				}
			case "unreadable file":
				requireOrgError(t, "could not be read")
			default:
				requireOrgError(t, "not owned by an administrator account")
			}
		})
	}
}

func TestLoadUserRejectsLockedKey(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"locked":[]}`, `{"locked":null}`, `{"locked":false}`} {
		if err := os.WriteFile(filepath.Join(dir, "backends.json"), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "locked: only the organisation's file may lock fields") {
			t.Fatalf("user locked error = %v", err)
		}
	}
}

func TestMergeGroups(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	five, six := 5, 6
	for _, tc := range []struct {
		group           string
		base, org, user Prefs
	}{
		{"enabled", Prefs{}, Prefs{EnabledNames: []string{"claude", "codex"}}, Prefs{EnabledNames: []string{"codex", "claude"}}},
		{"default", Prefs{EnabledNames: []string{"claude", "codex"}}, Prefs{DefaultName: "claude"}, Prefs{DefaultName: "codex"}},
		{"billing", Prefs{}, Prefs{Billing: "metered", BillingMaxCostUSD: 5}, Prefs{Billing: "metered", BillingMaxCostUSD: 6}},
		{"providers", Prefs{Billing: "metered"}, Prefs{Providers: map[string]Provider{"claude": {Target: "bedrock"}}}, Prefs{Providers: map[string]Provider{"claude": {Target: "vertex"}}}},
		{"accounts", Prefs{}, Prefs{Accounts: map[string][]Account{"claude": {{Name: "a", Home: dir}}}, AccountReservePercent: &five}, Prefs{Accounts: map[string][]Account{"codex": {{Name: "b", Home: dir}}}, AccountReservePercent: &six}},
		{"local_api", Prefs{}, Prefs{LocalAPI: "http://localhost:5000"}, Prefs{LocalAPI: "http://localhost:5001"}},
	} {
		t.Run(tc.group, func(t *testing.T) {
			org := Org{Prefs: tc.org, Locked: []string{tc.group}}
			for _, c := range []struct {
				name            string
				user, want      Prefs
				locked, ignored bool
			}{
				{"locked different", tc.user, tc.org, true, true},
				{"locked equal", tc.org, tc.org, true, false},
				{"locked unset", Prefs{}, tc.org, true, false},
				{"unlocked set", tc.user, tc.user, false, false},
				{"unlocked unset", Prefs{}, tc.org, false, false},
			} {
				t.Run(c.name, func(t *testing.T) {
					org.Prefs = overlayTestPrefs(tc.base, tc.org)
					org.Locked = nil
					if c.locked {
						org.Locked = []string{tc.group}
					}
					got, ignored, err := Merge(org, overlayTestPrefs(tc.base, c.user))
					if err != nil || !reflect.DeepEqual(got, overlayTestPrefs(tc.base, c.want)) || (c.ignored && !slices.Equal(ignored, []string{tc.group})) || (!c.ignored && len(ignored) != 0) {
						t.Fatalf("merge = %+v, %v, %v", got, ignored, err)
					}
				})
			}
		})
	}
}

func overlayTestPrefs(base, group Prefs) Prefs {
	if len(base.EnabledNames) > 0 {
		group.EnabledNames = base.EnabledNames
	}
	if base.Billing != "" {
		group.Billing = base.Billing
	}
	return group
}

func TestMergeEmptyLocksPairsAndPersonalFields(t *testing.T) {
	yes, no := true, false
	org := Org{Prefs: Prefs{WorkMapGitHub: &yes, WorkMapRepos: []string{"org"}, Billing: "metered", BillingMaxCostUSD: 9}, Locked: []string{"providers", "local_api"}}
	user := Prefs{WorkMapSessions: &yes, WorkMapGitHub: &no, WorkMapRepos: []string{"user"}, Billing: "metered",
		Providers: map[string]Provider{"claude": {Target: "vertex"}}, LocalAPI: "http://localhost"}
	got, ignored, err := Merge(org, user)
	want := user
	want.Providers, want.LocalAPI = nil, ""
	if err != nil || !reflect.DeepEqual(got, want) || !slices.Equal(ignored, []string{"local_api", "providers"}) {
		t.Fatalf("merge = %+v, %v, %v", got, ignored, err)
	}
	org = Org{Prefs: Prefs{Billing: "plan"}, Locked: []string{"billing"}}
	_, _, err = Merge(org, user)
	if err == nil || !strings.HasPrefix(err.Error(), "backend preferences: providers:") || !strings.HasSuffix(err.Error(), " (after the organisation's settings)") || strings.Contains(err.Error(), "vertex") {
		t.Fatalf("merged validation = %v", err)
	}
}

func TestLoadOrgRefusesScratchOwner(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "backends.json")
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	skipTrustedScratchOwner(t, path)
	useOrgPath(t, path, false)
	requireOrgError(t, "not owned by an administrator account")
}
