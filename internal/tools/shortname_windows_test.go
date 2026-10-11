//go:build windows

package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// shortName is long's 8.3 name. The test skips on a volume that makes none.
func shortName(t *testing.T, long string) string {
	t.Helper()
	p, err := syscall.UTF16PtrFromString(long)
	must(t, err)
	buf := make([]uint16, 1024)
	n, err := syscall.GetShortPathName(p, &buf[0], uint32(len(buf)))
	if err != nil || n == 0 || int(n) > len(buf) {
		t.Skipf("no short name for %s: %v", long, err)
	}
	short := syscall.UTF16ToString(buf[:n])
	if strings.EqualFold(filepath.Base(short), filepath.Base(long)) {
		t.Skipf("this volume makes no 8.3 names (%s)", short)
	}
	return short
}

// A path spelt by its short name is judged where it really is, and so is
// what grep finds under it: CODEX~1/auth.json is .codex/auth.json.
func TestAShortNamedPathIsJudgedWhereItReallyIs(t *testing.T) {
	ws, tl := setup(t)
	codex := filepath.Join(ws.Root, ".codex")
	must(t, os.MkdirAll(codex, 0o755))
	must(t, os.WriteFile(filepath.Join(codex, "auth.json"), []byte(`{"token":"hunter2-codex"}`), 0o644))
	short := filepath.Base(shortName(t, codex))

	for _, path := range []string{short, short + "/auth.json"} {
		if out, err := call(t, ws, tl["grep"], map[string]any{"pattern": "hunter2", "path": path}); strings.Contains(out, "hunter2-codex") {
			t.Errorf("grep in %q = %q, %v; want codex's login passed over", path, out, err)
		}
	}
	raw, _ := json.Marshal(map[string]any{"path": short + "/auth.json"})
	if rule := tl["read_file"].Refuse(ws, raw); rule != "secret-file" {
		t.Errorf("read_file %s/auth.json: Refuse = %q, want secret-file", short, rule)
	}
}
