package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestReadsWorkWithoutAProvider(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the hrn binary")
	}
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	w, err := runlog.Create("codex", agent.Task{Prompt: "fixture prompt"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(agent.Result{Status: agent.StatusOK}); err != nil {
		t.Fatal(err)
	}
	if err := batch.SaveResult(batch.Result{ID: "fixture-batch", Started: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "evals"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "evals", "fixture-eval.json"), []byte(`{"id":"fixture-eval","batch_id":"fixture-batch"}`), 0600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "hrn")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	path := t.TempDir()
	for _, name := range []string{"claude", "codex", "gemini", "opencode", "agy"} {
		if _, err := exec.LookPath(filepath.Join(path, name)); err == nil {
			t.Fatalf("provider %s resolves", name)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/knowledge/active" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"generation":1}`))
	}))
	defer srv.Close()
	request := filepath.Join(home, "request.json")
	if err := os.WriteFile(request, []byte(`{"tenant_id":"t_fixture","project_id":"p_fixture","backend":"codex","model":"fixture","revision":"abc"}`), 0600); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + path, "HOME=" + home, "USERPROFILE=" + home, "APPDATA=" + home, "HRN_HOME=" + home, "HRN_SERVER_URL=" + srv.URL, "HRN_TENANT_TOKEN=" + strings.Repeat("r", 64)}
	if runtime.GOOS == "windows" {
		env = append(env, "SYSTEMROOT="+os.Getenv("SYSTEMROOT"), "WINDIR="+os.Getenv("WINDIR"))
	}
	reads := []struct {
		args []string
		want string
	}{
		{[]string{"runs"}, "fixture prompt"},
		{[]string{"show", "--json", w.ID()}, "fixture prompt"},
		{[]string{"flow", w.ID()}, w.ID()},
		{[]string{"report"}, "codex"},
		{[]string{"batch", "show", "--json", "fixture-batch"}, "fixture-batch"},
		{[]string{"eval", "list"}, "fixture-eval"},
		{[]string{"runs", "prune", "--older-than", "9999d", "--dry-run"}, "run"},
		{[]string{"knowledge", "active", request}, `"generation": 1`},
	}
	checkReads := func(t *testing.T, env []string) {
		for _, tc := range reads {
			t.Run(strings.Join(tc.args, "_"), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, bin, tc.args...)
				cmd.Env = env
				out, err := cmd.CombinedOutput()
				if err != nil || !strings.Contains(string(out), tc.want) {
					t.Fatalf("%v: %v\n%s", tc.args, err, out)
				}
			})
		}
	}
	t.Run("without providers", func(t *testing.T) { checkReads(t, env) })
	shimPath := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"claude", "codex", "gemini", "opencode", "agy"} {
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		in, err := os.Open(self)
		if err != nil {
			t.Fatal(err)
		}
		out, err := os.OpenFile(filepath.Join(shimPath, name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.Copy(out, in)
		if err != nil {
			t.Fatal(err)
		}
		if err := out.Close(); err != nil {
			t.Fatal(err)
		}
		if err := in.Close(); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(home, "provider-calls")
	shimEnv := append([]string(nil), env...)
	shimEnv[0] = "PATH=" + shimPath
	shimEnv = append(shimEnv, "HRN_ZERO_PROVIDER_SHIM_LOG="+log)
	probe := filepath.Join(shimPath, "claude")
	if runtime.GOOS == "windows" {
		probe += ".exe"
	}
	cmd := exec.Command(probe)
	cmd.Env = shimEnv
	if err := cmd.Run(); err == nil {
		t.Fatal("recording shim unexpectedly succeeded")
	}
	if b, err := os.ReadFile(log); err != nil || !strings.Contains(string(b), "claude") {
		t.Fatalf("recording shim: %q, %v", b, err)
	}
	if err := os.Remove(log); err != nil {
		t.Fatal(err)
	}
	t.Run("with recording providers", func(t *testing.T) { checkReads(t, shimEnv) })
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatalf("provider call log exists or cannot be checked: %v", err)
	}
	for _, name := range []string{"", "api", "claude", "codex", "gemini", "opencode", "agy"} {
		t.Run("run_"+name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			args := []string{"run"}
			if name != "" {
				args = append(args, "-b", name)
			}
			args = append(args, "fixture prompt")
			cmd := exec.CommandContext(ctx, bin, args...)
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			want := name
			if name == "" {
				want = "claude"
			}
			if name == "api" {
				want = "ANTHROPIC_API_KEY"
			}
			if ctx.Err() != nil || err == nil || !strings.Contains(string(out), want) {
				t.Fatalf("run %s: %v (context %v)\n%s", name, err, ctx.Err(), out)
			}
		})
	}
}
