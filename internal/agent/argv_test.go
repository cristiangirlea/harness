package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestValidModel(t *testing.T) {
	for _, m := range []string{"claude-opus-5-5", "gpt-6.1-sol", "antigravity/claude-opus-5-5-high", "qwen3:32b", "claude-opus-5-5@20260601", "a+b", "x", "claude-opus-5[1m]"} {
		if !ValidModel(m) {
			t.Errorf("%q refused", m)
		}
	}
	for _, m := range []string{"", "x&calc", "%PATH%", `a"b`, "a b", "-x", "--help", ".hidden", "a|b", "a^b", "a\nb", strings.Repeat("a", 129)} {
		if ValidModel(m) {
			t.Errorf("%q accepted", m)
		}
	}
}

func TestCLIRefusesAModelThatIsNotAModelID(t *testing.T) {
	// The helper would sleep past the timeout if it were started.
	t.Setenv("HRN_CLI_HELPER", "sleep")
	res := helperCLI(parseOpencodeStream).Run(context.Background(),
		Task{Prompt: "go", Dir: t.TempDir(), Options: Options{Mode: ModeAsk, Model: "x&calc", Timeout: Duration(5 * time.Second)}}, func(Event) {})
	if res.Status != StatusError || !strings.Contains(res.Error, "is not a model id") {
		t.Fatalf("status = %s, error %q", res.Status, res.Error)
	}
}

func TestNpmShimNeverResolvesUnderTheWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "npm"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "npm", "claude.cmd"), []byte("@echo planted\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	for _, v := range []string{"", ".", "npm-home", filepath.Join("..", "x")} {
		t.Setenv("APPDATA", v)
		if got := appData("npm", "claude.cmd"); got != "" {
			t.Errorf("APPDATA=%q: appData = %q", v, got)
		}
		if got := npmShim("claude"); len(got) != 0 {
			t.Errorf("APPDATA=%q: npmShim = %v", v, got)
		}
	}
	if runtime.GOOS == "windows" {
		root := t.TempDir()
		t.Setenv("APPDATA", root)
		if got := npmShim("claude"); len(got) != 1 || got[0] != filepath.Join(root, "npm", "claude.cmd") {
			t.Errorf("absolute APPDATA: npmShim = %v", got)
		}
	}
}

func TestDetectSkipsRelativeCandidates(t *testing.T) {
	dir := t.TempDir()
	planted := filepath.Join(".local", "bin", "hrn-test-planted.exe")
	if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(planted)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, planted), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	b := &CLIBackend{name: "hrn-test-planted", candidates: func() []string { return []string{planted} }}
	if p, err := b.Detect(); err == nil {
		t.Fatalf("Detect = %q, want the relative candidate skipped", p)
	}
	b.candidates = func() []string { return []string{filepath.Join(dir, planted)} }
	if p, err := b.Detect(); err != nil || p != filepath.Join(dir, planted) {
		t.Fatalf("Detect = %q, %v, want the absolute candidate", p, err)
	}
}
