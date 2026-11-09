package archtest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSpawnFixtures(t *testing.T) {
	const head = `package a; import ("os/exec"; "example.com/root/internal/nowindow"); `
	const bare = `package a; import "os/exec"; func f() { _ = exec.Command("git") }`
	const unset = " is not passed to nowindow.Set before it is used"
	const inF = "internal/a/a.go:1: the command created in f" + unset
	one := func(src string) map[string]string { return map[string]string{"internal/a/a.go": src} }
	cases := []struct {
		name   string
		files  map[string]string
		exempt []string
		want   []string
	}{
		{"hidden", one(head + `func f() { c := exec.Command("git"); nowindow.Set(c); _ = c.Run() }`), nil, nil},
		{"hidden after its fields are set", one(head + `func f() { c := exec.Command("git"); c.Dir = "x"; c.Env = c.Environ(); nowindow.Set(c); _ = c.Run() }`), nil, nil},
		{"hidden in a switch case", one(head + `func f(n int) { switch n { case 1: c := exec.Command("git"); nowindow.Set(c); _ = c.Run() } }`), nil, nil},
		{"hidden in its closure", one(head + `var run = func() error { c := exec.Command("git"); nowindow.Set(c); return c.Run() }`), nil, nil},
		{"hidden then returned", one(head + `func f() *exec.Cmd { c := exec.Command("git"); nowindow.Set(c); return c }`), nil, nil},
		{"never hidden", one(`package a; import "os/exec"; func f() { c := exec.Command("git"); _ = c.Run() }`), nil, []string{inF}},
		{"with a context", map[string]string{"cmd/hrn/a.go": `package main; import ("context"; "os/exec"); func f() { c := exec.CommandContext(context.Background(), "git"); _ = c.Run() }`}, nil, []string{"cmd/hrn/a.go:1: the command created in f" + unset}},
		{"not kept in a variable", one(head + `func f() { _ = exec.Command("git").Run(); nowindow.Set(nil) }`), nil, []string{inF}},
		{"returned unhidden", one(head + `func f() *exec.Cmd { return exec.Command("git") }; func g(c *exec.Cmd) { nowindow.Set(c) }`), nil, []string{inF}},
		{"another command hidden", one(head + `func f() { c := exec.Command("git"); d := exec.Command("gh"); nowindow.Set(d); _, _ = c, d }`), nil, []string{inF}},
		{"replaced after it was hidden", one(head + `func f() { c := exec.Command("git"); nowindow.Set(c); c = exec.Command("gh"); _ = c.Run() }`), nil, []string{inF}},
		{"hidden after it ran", one(head + `func f() { c := exec.Command("git"); _ = c.Run(); nowindow.Set(c) }`), nil, []string{inF}},
		{"hidden after it was handed on", one(head + `func f(start func(*exec.Cmd)) { c := exec.Command("git"); start(c); nowindow.Set(c) }`), nil, []string{inF}},
		{"started under another name", one(head + `func f() { c := exec.Command("git"); other := c; _ = other.Run(); nowindow.Set(c) }`), nil, []string{inF}},
		{"started through a method value", one(head + `func f() { c := exec.Command("git"); run := c.Run; _ = run(); nowindow.Set(c) }`), nil, []string{inF}},
		{"only the inner command hidden", one(head + `func f() { c := exec.Command("git"); { c := exec.Command("gh"); nowindow.Set(c); _ = c.Run() }; _ = c.Run() }`), nil, []string{inF}},
		{"hidden only when deferred", one(head + `func f() { c := exec.Command("git"); defer nowindow.Set(c); _ = c.Run() }`), nil, []string{inF}},
		{"hidden only on a branch", one(head + `func f(ok bool) { c := exec.Command("git"); if ok { nowindow.Set(c) }; _ = c.Run() }`), nil, []string{inF}},
		{"hidden only inside a closure", one(head + `func f() { c := exec.Command("git"); _ = func() { nowindow.Set(c) }; _ = c.Run() }`), nil, []string{inF}},
		{"hidden only outside its closure", one(head + `func f(c *exec.Cmd) { nowindow.Set(c); _ = func() error { c := exec.Command("git"); return c.Run() } }`), nil, []string{inF}},
		{"constructor in parentheses", one(head + `func f() { c := (exec.Command)("git"); nowindow.Set(c); _ = c.Run() }`), nil, []string{inF}},
		{"constructor kept as a value", one(head + `func f() { mk := exec.Command; c := mk("git"); nowindow.Set(c); _ = c.Run() }`), nil, []string{inF}},
		{"dot import", one(`package a; import . "os/exec"; func f() { _ = Command("git") }`), nil, []string{"internal/a/a.go:1: os/exec is dot-imported, which hides its spawn sites from this check"}},
		{"renamed imports", one(`package a; import (ex "os/exec"; hide "example.com/root/internal/nowindow"); func f() { c := ex.Command("git"); hide.Set(c); d := ex.Command("gh"); _ = d }`), nil, []string{inF}},
		{"the offending line", one("package a\n\nimport \"os/exec\"\n\nfunc f() {\n\tc := exec.Command(\"git\")\n\t_ = c.Run()\n}\n"), nil, []string{"internal/a/a.go:6: the command created in f" + unset}},
		{"tests and other trees ignored", map[string]string{"internal/a/a_test.go": bare, "scripts/x/main.go": bare, "server/a/a.go": bare, "internal/a/testdata/a.go": bare}, nil, nil},
		{"exempt function", one(`package a; import "os/exec"; func f() { _ = exec.Command("browser") }; func g() { _ = exec.Command("git") }`), []string{"internal/a/a.go:f"}, []string{"internal/a/a.go:1: the command created in g" + unset}},
		{"second site in an exempt function", one("package a\nimport \"os/exec\"\nfunc f() {\n\t_ = exec.Command(\"browser\")\n\t_ = exec.Command(\"git\")\n}\n"), []string{"internal/a/a.go:f"}, []string{"exemption internal/a/a.go:f covers 2 spawn sites, want one: lines 4, 5"}},
		{"nested site in an exempt function", one(`package a; import "os/exec"; func f() { _ = exec.Command("browser"); _ = func() { _ = exec.Command("git") } }`), []string{"internal/a/a.go:f"}, []string{"exemption internal/a/a.go:f covers 2 spawn sites, want one: lines 1, 1"}},
		{"stale exemption", one(`package a`), []string{"internal/a/a.go:f"}, []string{"stale exemption: internal/a/a.go:f"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for k, v := range tc.files {
				p := filepath.Join(root, filepath.FromSlash(k))
				if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(v), 0644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := unhiddenSpawns(root, tc.exempt)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEverySpawnSiteHidesItsConsoleWindow(t *testing.T) {
	problems, err := unhiddenSpawns(filepath.Join("..", ".."), spawnExempt)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) > 0 {
		t.Fatalf("write `x := exec.Command(...)` then `nowindow.Set(x)` before x is used, or exempt the function in spawnExempt:\n%s", strings.Join(problems, "\n"))
	}
}
