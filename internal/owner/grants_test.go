package owner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestViewerProjectGrants(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:me")
	project := filepath.Join(t.TempDir(), "repo")
	v := Viewer{ID: Email("reader@example.com"), Grants: []Grant{{Viewer: Email("reader@example.com"), Project: project}}}
	for _, tc := range []struct {
		name, recorded, dir string
		want                bool
	}{
		{"exact", "local:me", project, true},
		{"child", "local:me", filepath.Join(project, "sub"), true},
		{"clean", "", filepath.Join(project, "sub", ".."), true},
		{"prefix sibling", "local:me", project + "2", false},
		{"other owner", "local:other", project, false},
		{"no directory", "local:me", "", false},
		{"relative", "local:me", "repo", false},
		{"case", "local:me", strings.ToUpper(project), runtime.GOOS == "windows"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := v.CanReadRecord(tc.recorded, tc.dir); got != tc.want {
				t.Fatalf("CanReadRecord(%q, %q) = %v", tc.recorded, tc.dir, got)
			}
		})
	}
	if v.CanRead("local:me") || (Viewer{ID: Email("other@example.com"), Grants: v.Grants}).CanReadRecord("local:me", project) {
		t.Fatal("a grant must match both the viewer and directory")
	}
	v.Grants[0].Owner = "user:team"
	if !v.CanReadRecord("user:team", project) || v.CanReadRecord("local:me", project) {
		t.Fatal("an explicit owner must replace the default")
	}
}

func TestReadGrantsFailsClosed(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:me")
	path := filepath.Join(os.Getenv("HRN_HOME"), "grants.json")
	if grants, err := ReadGrants(path); err != nil || len(grants) != 0 {
		t.Fatalf("absent file = %v, %v", grants, err)
	}
	valid, _ := json.Marshal([]Grant{{Viewer: "user:reader", Project: t.TempDir()}})
	for _, data := range []string{`{`, `null`, `{}`, `[{"viewer":"user:reader","project":"relative"}]`, `[{"viewer":"local:reader","project":"/repo"}]`, string(valid[:len(valid)-1]) + `,{}]`} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if grants, err := ReadGrants(path); err == nil || len(grants) != 0 {
			t.Fatalf("invalid grants %q = %v, %v", data, grants, err)
		}
	}
	if err := os.WriteFile(path, valid, 0600); err != nil {
		t.Fatal(err)
	}
	if grants, err := ReadGrants(path); err != nil || len(grants) != 1 {
		t.Fatalf("valid grants = %v, %v", grants, err)
	}
}

func TestViewerReadDecisionBasis(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_OWNER", "local:me")
	project := t.TempDir()
	for _, tc := range []struct {
		viewer Viewer
		basis  string
	}{
		{Viewer{ID: "local:me"}, "owner"},
		{Viewer{Admin: true}, "admin"},
		{Viewer{ID: "user:reader", Grants: []Grant{{Viewer: "user:reader", Project: project}}}, "grant:" + project},
		{Viewer{ID: "user:other"}, "none"},
	} {
		calls := 0
		tc.viewer.OnRead = func(id, basis string) {
			calls++
			if id != "record" || basis != tc.basis {
				t.Fatalf("audit = %q, %q; want %q", id, basis, tc.basis)
			}
		}
		if got := tc.viewer.ReadRecord("record", "", project); got != (tc.basis != "none") || calls != 1 {
			t.Fatalf("basis %q: allowed=%v, calls=%d", tc.basis, got, calls)
		}
	}
}
