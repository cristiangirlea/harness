package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
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
	"github.com/cristiangirlea/harness/internal/mcp"
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
	for _, name := range []string{"git", "sh", "bash"} {
		source, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("find system tool %s: %v", name, err)
		}
		if runtime.GOOS == "windows" && name == "git" {
			core, err := exec.Command(source, "--exec-path").Output()
			if err != nil {
				t.Fatal(err)
			}
			coreDir := strings.TrimSpace(string(core))
			dlls, err := filepath.Glob(filepath.Join(coreDir, "..", "..", "bin", "*.dll"))
			if err != nil || len(dlls) == 0 {
				t.Fatalf("find git DLLs: %v", err)
			}
			for _, dll := range dlls {
				data, err := os.ReadFile(dll)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, filepath.Base(dll)), data, 0755); err != nil {
					t.Fatal(err)
				}
			}
			source = filepath.Join(coreDir, "git.exe")
		}
		if runtime.GOOS == "windows" && name == "bash" {
			sh, err := exec.LookPath("sh")
			if err != nil {
				t.Fatal(err)
			}
			source = filepath.Join(filepath.Dir(sh), "bash.exe")
		}
		target := filepath.Join(path, name)
		if runtime.GOOS == "windows" {
			target += ".exe"
		}
		data, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, data, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if runtime.GOOS == "windows" {
		sh, _ := exec.LookPath("sh")
		dll, err := os.ReadFile(filepath.Join(filepath.Dir(sh), "msys-2.0.dll"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "msys-2.0.dll"), dll, 0755); err != nil {
			t.Fatal(err)
		}
	}
	repo := filepath.Join(home, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	git := exec.Command(filepath.Join(path, "git"+func() string {
		if runtime.GOOS == "windows" {
			return ".exe"
		}
		return ""
	}()), "init", "-q", repo)
	if out, err := git.CombinedOutput(); err != nil {
		t.Fatalf("init fixture repository: %v\n%s", err, out)
	}
	for _, name := range []string{"claude", "codex", "gemini", "opencode", "agy"} {
		if _, err := exec.LookPath(filepath.Join(path, name)); err == nil {
			t.Fatalf("provider %s resolves", name)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/knowledge/active" && !strings.HasPrefix(r.URL.Path, "/v1/jobs/j_"+strings.Repeat("1", 32)) {
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
	for _, args := range [][]string{{"config", "user.name", "Fixture"}, {"config", "user.email", "fixture@example.invalid"}, {"commit", "--allow-empty", "-qm", "base"}} {
		cmd := exec.Command(filepath.Join(path, "git"+func() string {
			if runtime.GOOS == "windows" {
				return ".exe"
			}
			return ""
		}()), args...)
		cmd.Dir, cmd.Env = repo, env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	reads := []struct {
		args []string
		want string
	}{
		{[]string{"gc", repo}, "nothing to remove; 0 worktrees and 0 caches stay"},
		{[]string{"runs"}, "fixture prompt"},
		{[]string{"show", "--json", w.ID()}, "fixture prompt"},
		{[]string{"flow", w.ID()}, "run codex"},
		{[]string{"report"}, "codex"},
		{[]string{"batch", "show", "--json", "fixture-batch"}, "fixture-batch"},
		{[]string{"eval", "list"}, "fixture-eval"},
		{[]string{"runs", "prune", "--older-than", "9999d", "--dry-run"}, "would remove 0 runs, 0 batch records and 1 eval records"},
		{[]string{"knowledge", "active", request}, `"generation": 1`},
	}
	checkReads := func(t *testing.T, env []string) {
		for _, tc := range reads {
			t.Run(strings.Join(tc.args, "_"), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, bin, tc.args...)
				prepareZeroProviderCommand(cmd)
				cmd.Env = env
				out, err := cmd.CombinedOutput()
				if err != nil || !strings.Contains(string(out), tc.want) {
					t.Fatalf("%v: %v\n%s", tc.args, err, out)
				}
			})
		}
		t.Run("batch fixture", func(t *testing.T) {
			fixture := t.TempDir()
			git := func(dir string, args ...string) string {
				t.Helper()
				cmd := exec.Command(filepath.Join(path, "git"+func() string {
					if runtime.GOOS == "windows" {
						return ".exe"
					}
					return ""
				}()), args...)
				cmd.Dir, cmd.Env = dir, env
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			git(fixture, "init", "-q")
			git(fixture, "config", "user.name", "Fixture")
			git(fixture, "config", "user.email", "fixture@example.invalid")
			if err := os.WriteFile(filepath.Join(fixture, "file"), []byte("base\n"), 0600); err != nil {
				t.Fatal(err)
			}
			git(fixture, "add", "file")
			git(fixture, "commit", "-qm", "base")
			base := git(fixture, "rev-parse", "HEAD")
			git(fixture, "checkout", "-qb", "scratch")
			worktree := filepath.Join(t.TempDir(), "worktree")
			git(fixture, "worktree", "add", "-qb", "hrn/fixture", worktree)
			if err := os.WriteFile(filepath.Join(worktree, "file"), []byte("changed\n"), 0600); err != nil {
				t.Fatal(err)
			}
			git(worktree, "commit", "-qam", "change")
			commit := git(worktree, "rev-parse", "HEAD")
			res := batch.Result{ID: "fixture-ops", Dir: fixture, Base: base, Started: time.Now(), Tasks: []batch.TaskResult{{ID: "task", Status: batch.StatusPass, Branch: "hrn/fixture", Worktree: worktree, Commit: commit}}}
			if err := batch.SaveResult(res); err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				args []string
				want string
			}{
				{[]string{"batch", "diff", res.ID, "task"}, "+changed"},
				{[]string{"batch", "merge", res.ID, "task"}, "merged"},
				{[]string{"batch", "rm", res.ID}, "removed"},
			} {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				cmd := exec.CommandContext(ctx, bin, tc.args...)
				prepareZeroProviderCommand(cmd)
				cmd.Env = env
				out, err := cmd.CombinedOutput()
				cancel()
				if err != nil || !strings.Contains(string(out), tc.want) {
					t.Fatalf("%v: %v\n%s", tc.args, err, out)
				}
			}
			data, err := os.ReadFile(filepath.Join(fixture, "file"))
			if err != nil || string(data) != "changed\n" {
				t.Fatalf("merge: %q %v", data, err)
			}
			if _, err := os.Stat(worktree); !os.IsNotExist(err) {
				t.Fatalf("worktree remains: %v", err)
			}
		})
		t.Run("mcp", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bin, "mcp")
			prepareZeroProviderCommand(cmd)
			started := time.Now()
			cmd.Env = env
			cmd.Dir = repo
			in, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = in.Close(); _ = cmd.Wait(); assertNoZeroProviderChildren(t, cmd.Process.Pid, started) }()
			var client mcp.Client
			if err := client.Connect(ctx, out, in); err != nil {
				t.Fatal(err)
			}
			remoteArgs := json.RawMessage(`{"tenant_id":"t_` + strings.Repeat("1", 32) + `","project_id":"p_` + strings.Repeat("1", 32) + `","job_id":"j_` + strings.Repeat("1", 32) + `"}`)
			cases := map[string]struct {
				args json.RawMessage
				want string
			}{
				"runs":          {json.RawMessage(`{}`), "fixture prompt"},
				"show":          {json.RawMessage(`{"run_id":"` + w.ID() + `"}`), "fixture prompt"},
				"flow":          {json.RawMessage(`{"id":"` + w.ID() + `"}`), "run codex"},
				"batch_show":    {json.RawMessage(`{"batch_id":"fixture-batch"}`), "fixture-batch"},
				"backends":      {json.RawMessage(`{}`), "claude"},
				"remote_status": {remoteArgs, "generation"},
				"remote_events": {remoteArgs, "generation"},
			}
			for _, tool := range client.Tools() {
				if !tool.Annotations.ReadOnly() {
					continue
				}
				tc, ok := cases[tool.Name]
				if !ok {
					t.Fatalf("no fixture for read-only tool %s", tool.Name)
				}
				result, err := client.CallTool(ctx, tool.Name, tc.args)
				if err != nil || result.IsError || !strings.Contains(result.Text(), tc.want) {
					t.Fatalf("%s: %v %+v", tool.Name, err, result)
				}
				delete(cases, tool.Name)
			}
			if len(cases) != 0 {
				t.Fatalf("tools/list lacks read-only tools: %v", cases)
			}

			if env[0] == "PATH="+path {
				args, err := json.Marshal(map[string]string{"prompt": "fixture prompt", "backend": "claude", "dir": repo})
				if err != nil {
					t.Fatal(err)
				}
				result, err := client.CallTool(ctx, "run", args)
				if err != nil || !result.IsError || !strings.Contains(result.Text(), "claude not found on PATH") {
					t.Fatalf("run: %v %+v", err, result)
				}
			}
		})
		t.Run("watch", func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addr := ln.Addr().String()
			if err := ln.Close(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bin, "watch", "--addr", addr)
			prepareZeroProviderCommand(cmd)
			cmd.Env = env
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
			client := &http.Client{Timeout: time.Second}
			for {
				resp, err := client.Get("http://" + addr + "/")
				if err == nil {
					resp.Body.Close()
					if resp.StatusCode != http.StatusOK {
						t.Fatalf("watch: HTTP %d", resp.StatusCode)
					}
					break
				}
				select {
				case <-ctx.Done():
					t.Fatalf("watch did not start: %v", err)
				case <-time.After(50 * time.Millisecond):
				}
			}
		})
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
	shimEnv[0] = "PATH=" + shimPath + string(os.PathListSeparator) + path
	shimEnv = append(shimEnv, "HRN_ZERO_PROVIDER_SHIM_LOG="+log)
	probe := filepath.Join(shimPath, "claude")
	if runtime.GOOS == "windows" {
		probe += ".exe"
	}
	cmd := exec.Command(probe)
	cmd.Env = env
	if err := cmd.Run(); err == nil {
		t.Fatal("shim without a log unexpectedly succeeded")
	}
	cmd = exec.Command(probe)
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
	spec := filepath.Join(home, "tasks.json")
	specData, err := json.Marshal(map[string]any{"dir": repo, "backend": "claude", "tasks": []any{map[string]string{"id": "task", "prompt": "fixture prompt"}}, "matrix": []any{map[string]string{"backend": "claude"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spec, specData, 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"batch", "eval"} {
		t.Run("execute_"+name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bin, name, spec)
			prepareZeroProviderCommand(cmd)
			started := time.Now()
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			if cmd.Process == nil {
				t.Fatalf("start: %v", err)
			}
			assertNoZeroProviderChildren(t, cmd.Process.Pid, started)
			// Eval currently reports a failed task but exits successfully.
			if ctx.Err() != nil || (name == "batch" && err == nil) || (name == "eval" && err != nil) || !strings.Contains(string(out), "claude not found on PATH") {
				t.Fatalf("%s: %v (%v)\n%s", name, err, ctx.Err(), out)
			}
		})
	}
	t.Run("exec_start", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		frame, err := json.Marshal(map[string]any{"type": "start", "attempt_id": "fixture", "task": map[string]string{"prompt": "fixture prompt", "dir": repo, "backend": "claude", "mode": "auto"}})
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(ctx, bin, "exec", "--protocol", "v1")
		prepareZeroProviderCommand(cmd)
		started := time.Now()
		cmd.Env = env
		cmd.Stdin = strings.NewReader(string(frame) + "\n")
		out, err := cmd.CombinedOutput()
		if cmd.Process == nil {
			t.Fatalf("start: %v", err)
		}
		assertNoZeroProviderChildren(t, cmd.Process.Pid, started)
		var rejected bool
		for _, line := range strings.Split(string(out), "\n") {
			var frame struct {
				Type  string `json:"type"`
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal([]byte(line), &frame) == nil && frame.Type == "rejected" && frame.Error.Code == "unavailable" && strings.Contains(frame.Error.Message, "claude not found on PATH") {
				rejected = true
			}
		}
		if ctx.Err() != nil || err == nil || !rejected {
			t.Fatalf("exec: %v (%v)\n%s", err, ctx.Err(), out)
		}
	})
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
			prepareZeroProviderCommand(cmd)
			started := time.Now()
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			if cmd.Process == nil {
				t.Fatalf("start: %v", err)
			}
			assertNoZeroProviderChildren(t, cmd.Process.Pid, started)
			want := name + " not found on PATH"
			if name == "" {
				want = "claude not found on PATH"
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
