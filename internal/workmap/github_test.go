package workmap

import (
	"context"
	"errors"
	"fmt"
	"github.com/cristiangirlea/harness/internal/ws"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGitHubJoinsBranchesAndKeepsOnlySelectedFields(t *testing.T) {
	home := t.TempDir()
	old := ws.GH
	t.Cleanup(func() { ws.GH = old })
	calls := 0
	ws.GH = func(_ context.Context, args ...string) ([]byte, error) {
		calls++
		switch strings.Join(args, " ") {
		case "issue list --repo=owner/repo --state open --limit 100 --json number,title,url,updatedAt":
			return []byte(`[{"number":1,"title":"idea","url":"issue","body":"secret body","author":{"login":"secret author"}}]`), nil
		case "pr list --repo=owner/repo --state open --limit 100 --json number,title,url,headRefName,isCrossRepository,reviewDecision,statusCheckRollup,updatedAt":
			return []byte(`[{"number":2,"title":"pull","url":"pr","headRefName":"feature","statusCheckRollup":[{"name":"test","conclusion":"FAILURE"}]}]`), nil
		default:
			return []byte(`[]`), nil
		}
	}
	pieces := map[string]*Piece{"repo\x00branch:feature": {Repo: "repo", Key: "branch:feature", Stage: Branch, Links: Links{Branch: "feature"}}}
	if err := githubRepo(context.Background(), home, "repo", "owner/repo", pieces); err != nil {
		t.Fatal(err)
	}
	if pieces["repo\x00branch:feature"].Stuck != "CI red: test" || len(pieces) != 2 {
		t.Fatalf("pieces: %+v", pieces)
	}
	if err := githubRepo(context.Background(), home, "repo", "owner/repo", pieces); err != nil || calls != 3 {
		t.Fatalf("cache: %d %v", calls, err)
	}
	data, err := os.ReadFile(filepath.Join(home, "workmap", "github", "owner__repo.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret") {
		t.Fatal("private fields cached")
	}
	oldTTL := githubTTL
	t.Cleanup(func() { githubTTL = oldTTL })
	githubTTL = 0
	if err := githubRepo(context.Background(), home, "repo", "owner/repo", pieces); err != nil || calls != 6 {
		t.Fatalf("refresh: %d %v", calls, err)
	}
}

func TestGitHubRefreshFallsBackAndRaisesMergedBranches(t *testing.T) {
	home := t.TempDir()
	old := ws.GH
	ttl := githubTTL
	t.Cleanup(func() { ws.GH = old; githubTTL = ttl })
	calls := 0
	fail := false
	ws.GH = func(_ context.Context, args ...string) ([]byte, error) {
		calls++
		if fail {
			return nil, fmt.Errorf("unavailable")
		}
		if args[0] == "issue" {
			return []byte(`[]`), nil
		}
		if strings.Contains(strings.Join(args, " "), "--state open") {
			return []byte(`[{"number":2,"title":"new","url":"open","headRefName":"new","reviewDecision":"CHANGES_REQUESTED"}]`), nil
		}
		return []byte(`[{"number":3,"title":"merged","url":"merged","headRefName":"feature"}]`), nil
	}
	makePieces := func() map[string]*Piece {
		return map[string]*Piece{"repo\x00branch:feature": {Repo: "repo", Key: "branch:feature", Stage: Review, Stuck: "local", Links: Links{Branch: "feature"}}}
	}
	pieces := makePieces()
	if err := githubRepo(context.Background(), home, "repo", "owner/repo", pieces); err != nil {
		t.Fatal(err)
	}
	if p := pieces["repo\x00branch:feature"]; p.Stage != Merged || p.Stuck != "" {
		t.Fatalf("merged: %+v", p)
	}
	if p := pieces["repo\x00pr#2"]; p == nil || p.Stuck != "review: changes requested" {
		t.Fatalf("open: %+v", p)
	}
	githubTTL = 0
	fail = true
	pieces = makePieces()
	if err := githubRepo(context.Background(), home, "repo", "owner/repo", pieces); err == nil || pieces["repo\x00pr#2"] == nil {
		t.Fatal("failed refresh did not return cached data and error")
	}
	if calls != 4 {
		t.Fatalf("calls: %d", calls)
	}
}

func TestGitHubSkipsOtherRemotesAndDisabledSource(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	old := ws.GH
	t.Cleanup(func() { ws.GH = old })
	calls := 0
	ws.GH = func(_ context.Context, args ...string) ([]byte, error) { calls++; return []byte(`[]`), nil }
	run := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git: %s: %v", out, err)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@example.invalid")
	run("config", "user.name", "Test")
	run("commit", "--allow-empty", "-m", "initial")
	run("remote", "add", "origin", "https://example.com/owner/repo")
	if _, err := Build(context.Background(), home, []string{repo}); err != nil || calls != 0 {
		t.Fatalf("non-GitHub: %v %d", err, calls)
	}
	run("remote", "set-url", "origin", "git@github.com:owner/repo.git")
	if err := os.WriteFile(filepath.Join(home, "backends.json"), []byte(`{"work_map_github":false}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(context.Background(), home, []string{repo}); err != nil || calls != 0 {
		t.Fatalf("disabled: %v %d", err, calls)
	}
}

func TestGitHubSeparatesForksAndPreservesOpenPullRequests(t *testing.T) {
	old := ws.GH
	t.Cleanup(func() { ws.GH = old })
	ws.GH = func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "issue" {
			return []byte(`[]`), nil
		}
		if strings.Contains(strings.Join(args, " "), "--state open") {
			return []byte(`[{"number":1,"url":"open","headRefName":"feature","statusCheckRollup":[{"name":"ci","conclusion":"FAILURE"}]},{"number":2,"url":"fork","headRefName":"feature","isCrossRepository":true}]`), nil
		}
		return []byte(`[{"number":3,"url":"old","headRefName":"feature"},{"number":4,"url":"fork-merged","headRefName":"other","isCrossRepository":true}]`), nil
	}
	pieces := map[string]*Piece{"r\x00branch:feature": {Repo: "r", Stage: Branch}, "r\x00branch:other": {Repo: "r", Stage: Branch, Stuck: "local"}}
	if err := githubRepo(context.Background(), t.TempDir(), "r", "o/r", pieces); err != nil {
		t.Fatal(err)
	}
	if p := pieces["r\x00branch:feature"]; p.Stage != CI || p.Links.PR != "open" || p.Stuck != "CI red: ci" {
		t.Fatalf("open: %+v", p)
	}
	if p := pieces["r\x00branch:other"]; p.Stage != Branch || p.Stuck != "local" {
		t.Fatalf("fork affected local: %+v", p)
	}
	if pieces["r\x00pr#2"] == nil || pieces["r\x00pr#4"] == nil {
		t.Fatalf("fork pieces: %+v", pieces)
	}
}

func TestGitHubReportsCheckStates(t *testing.T) {
	for _, tc := range []struct{ check, want string }{
		{`{"name":"ci/x","conclusion":"FAILURE"}`, "CI red: ci/x"}, {`{"name":"ci/x","conclusion":"ERROR"}`, "CI red: ci/x"}, {`{"name":"ci/x","conclusion":"CANCELLED"}`, "CI red: ci/x"}, {`{"name":"ci/x","conclusion":"TIMED_OUT"}`, "CI red: ci/x"}, {`{"name":"ci/x","conclusion":"ACTION_REQUIRED"}`, "CI red: ci/x"}, {`{"name":"ci/x","conclusion":"STARTUP_FAILURE"}`, "CI red: ci/x"}, {`{"context":"ci/x","state":"FAILURE"}`, "CI red: ci/x"}, {`{"context":"ci/x","state":"PENDING"}`, "pending"}, {`{"name":"ci/x","status":"IN_PROGRESS"}`, "pending"},
	} {
		t.Run(tc.check, func(t *testing.T) {
			old := ws.GH
			t.Cleanup(func() { ws.GH = old })
			ws.GH = func(_ context.Context, args ...string) ([]byte, error) {
				if args[0] == "pr" && strings.Contains(strings.Join(args, " "), "--state open") {
					return []byte(`[{"number":1,"statusCheckRollup":[` + tc.check + `]}]`), nil
				}
				return []byte(`[]`), nil
			}
			pieces := map[string]*Piece{}
			if err := githubRepo(context.Background(), t.TempDir(), "r", "o/r", pieces); err != nil {
				t.Fatal(err)
			}
			p := pieces["r\x00pr#1"]
			got := p.Stuck
			if got == "" {
				got = p.Links.Checks
			}
			if got != tc.want {
				t.Fatalf("got %q", got)
			}
		})
	}
}

func TestGitHubThrottlesFailedRefresh(t *testing.T) {
	old := ws.GH
	t.Cleanup(func() { ws.GH = old })
	calls := 0
	ws.GH = func(context.Context, ...string) ([]byte, error) { calls++; return nil, fmt.Errorf("unavailable") }
	home := t.TempDir()
	for i := 0; i < 2; i++ {
		err := githubRepo(context.Background(), home, "r", "o/r", map[string]*Piece{})
		if err == nil || !strings.Contains(err.Error(), "unavailable") {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if calls != 1 {
		t.Fatalf("calls: %d", calls)
	}
}

func TestGitHubRejectsFutureCacheTimes(t *testing.T) {
	old := ws.GH
	t.Cleanup(func() { ws.GH = old })
	calls := 0
	ws.GH = func(context.Context, ...string) ([]byte, error) { calls++; return []byte(`[]`), nil }
	home := t.TempDir()
	dir := filepath.Join(home, "workmap", "github")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "o__r.json"), []byte(`{"fetched":"2099-01-01T00:00:00Z","tried":"2099-01-01T00:00:00Z"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := githubRepo(context.Background(), home, "r", "o/r", map[string]*Piece{}); err != nil || calls != 3 {
		t.Fatalf("calls %d error %v", calls, err)
	}
}

func TestGitHubParsesRemoteNames(t *testing.T) {
	for _, tc := range []struct{ url, want string }{{"https://github.com/-R/x", ""}, {"git@github.com:o/-x.git", ""}, {"https://github.com/o/..", ""}, {"https://user:tok@github.com/o/r.git", "o/r"}, {"ssh://git@github.com/o/r", "o/r"}, {"https://github.com/Owner/repo.git", "owner/repo"}, {"git@github.com:Owner/repo.git", "owner/repo"}} {
		t.Run(tc.url, func(t *testing.T) {
			if got := parseGitHubRepo(tc.url); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestGitHubKeepsHigherLocalStages(t *testing.T) {
	old := ws.GH
	t.Cleanup(func() { ws.GH = old })
	ws.GH = func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "pr" && strings.Contains(strings.Join(args, " "), "--state open") {
			return []byte(`[{"number":1,"headRefName":"feature","statusCheckRollup":[{"name":"ci","conclusion":"FAILURE"}]}]`), nil
		}
		return []byte(`[]`), nil
	}
	for _, stage := range []Stage{Merged, Released} {
		t.Run(string(stage), func(t *testing.T) {
			p := &Piece{Repo: "r", Stage: stage}
			pieces := map[string]*Piece{"r\x00branch:feature": p}
			if err := githubRepo(context.Background(), t.TempDir(), "r", "o/r", pieces); err != nil {
				t.Fatal(err)
			}
			if p.Stage != stage || p.Stuck != "" {
				t.Fatalf("piece: %+v", p)
			}
		})
	}
}

func TestGitHubHandlesCorruptAndMissingCaches(t *testing.T) {
	for _, tc := range []struct {
		name          string
		corrupt, fail bool
	}{{"corrupt success", true, false}, {"corrupt failure", true, true}, {"missing failure", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			dir := filepath.Join(home, "workmap", "github")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if tc.corrupt {
				if err := os.WriteFile(filepath.Join(dir, "o__r.json"), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			old := ws.GH
			t.Cleanup(func() { ws.GH = old })
			ws.GH = func(_ context.Context, args ...string) ([]byte, error) {
				if tc.fail {
					return nil, errors.New("failed")
				}
				if args[0] == "issue" {
					return []byte(`[{"number":1}]`), nil
				}
				return []byte(`[]`), nil
			}
			pieces := map[string]*Piece{}
			err := githubRepo(context.Background(), home, "r", "o/r", pieces)
			if (err != nil) != tc.fail || (len(pieces) == 0) != tc.fail {
				t.Fatalf("pieces %v error %v", pieces, err)
			}
		})
	}
}

func TestGitHubCacheCleansTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")
	if err := writeGitHubCache(path, githubCache{}); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("mode: %v %v", info, err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "blocked"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeGitHubCache(filepath.Join(dir, "blocked"), githubCache{}); err == nil {
		t.Fatal("expected rename failure")
	}
	files, err := filepath.Glob(filepath.Join(dir, ".github-*.json"))
	if err != nil || len(files) != 0 {
		t.Fatalf("temporary files: %v %v", files, err)
	}
}

func githubTestRepo(t *testing.T, name string) string {
	t.Helper()
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", repo}, args...)...)
		c.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git: %s %v", out, err)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@example.invalid")
	run("config", "user.name", "Test")
	run("commit", "--allow-empty", "-m", "initial")
	run("branch", "feature")
	run("remote", "add", "origin", "https://github.com/o/"+name)
	return repo
}

func TestBuildCallsGitHubByDefaultAndDeduplicatesErrors(t *testing.T) {
	repos := []string{githubTestRepo(t, "one"), githubTestRepo(t, "two")}
	old := ws.GH
	t.Cleanup(func() { ws.GH = old })
	var calls atomic.Int32
	ws.GH = func(_ context.Context, args ...string) ([]byte, error) {
		calls.Add(1)
		if !strings.HasPrefix(args[2], "--repo=o/") {
			t.Errorf("repo argument: %v", args)
		}
		return nil, errors.New("not logged in")
	}
	home := t.TempDir()
	pieces, err := Build(context.Background(), home, repos)
	if calls.Load() != 2 || err == nil || strings.Count(err.Error(), "not logged in") != 1 || len(pieces) != 2 {
		t.Fatalf("calls %d pieces %+v error %v", calls.Load(), pieces, err)
	}
	pieces, err = Build(context.Background(), home, repos)
	if calls.Load() != 2 || err == nil || strings.Count(err.Error(), "not logged in") != 1 || len(pieces) != 2 {
		t.Fatalf("retry calls %d pieces %+v error %v", calls.Load(), pieces, err)
	}
}

func TestGitHubLimitsConcurrentRepositoriesAndCancels(t *testing.T) {
	roots := map[string]string{}
	for i := 0; i < 6; i++ {
		r := githubTestRepo(t, fmt.Sprintf("repo%d", i))
		roots[canonical(r)] = r
	}
	old := ws.GH
	t.Cleanup(func() { ws.GH = old })
	var active, max atomic.Int32
	var once sync.Once
	reached := make(chan struct{})
	release := make(chan struct{})
	ws.GH = func(ctx context.Context, _ ...string) ([]byte, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			m := max.Load()
			if n <= m || max.CompareAndSwap(m, n) {
				break
			}
		}
		if n == 4 {
			once.Do(func() { close(reached) })
		}
		select {
		case <-release:
			return []byte(`[]`), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	pieces := map[string]*Piece{}
	for r := range roots {
		pieces[r+"\x00branch:feature"] = &Piece{Repo: r, Key: "branch:feature", Stage: Branch}
	}
	go func() { done <- githubSources(ctx, t.TempDir(), roots, pieces) }()
	select {
	case <-reached:
	case <-time.After(30 * time.Second):
		t.Fatal("workers did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || max.Load() != 4 || len(pieces) != 6 {
			t.Fatalf("max %d pieces %d error %v", max.Load(), len(pieces), err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not return")
	}
	close(release)
}

func TestBuildKeepsLocalPiecesWhenGitHubIsCancelled(t *testing.T) {
	repo := githubTestRepo(t, "cancel")
	old := ws.GH
	t.Cleanup(func() { ws.GH = old })
	started := make(chan struct{})
	ws.GH = func(ctx context.Context, _ ...string) ([]byte, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		pieces []Piece
		err    error
	}
	done := make(chan outcome, 1)
	home := t.TempDir()
	go func() { p, e := Build(ctx, home, []string{repo}); done <- outcome{p, e} }()
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("GitHub did not start")
	}
	cancel()
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) || len(got.pieces) != 1 || got.pieces[0].Key != "branch:feature" {
			t.Fatalf("pieces %+v error %v", got.pieces, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not return")
	}
}

func TestPassingChecksClearOlderBatchFailure(t *testing.T) {
	old := ws.GH
	t.Cleanup(func() { ws.GH = old })
	ws.GH = func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "pr" && args[1] == "list" && args[3] == "--state" && args[4] == "open" {
			return []byte(`[ {"number":1,"headRefName":"feature","updatedAt":"2025-01-02T00:00:00Z","statusCheckRollup":[{"conclusion":"SUCCESS"}]} ]`), nil
		}
		return []byte(`[]`), nil
	}
	at := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	p := &Piece{Repo: "r", Key: "branch:feature", Stuck: "gate failed", stuckAt: at}
	pieces := map[string]*Piece{"r\x00branch:feature": p}
	if err := githubRepo(context.Background(), t.TempDir(), "r", "o/r", pieces); err != nil {
		t.Fatal(err)
	}
	if p.Stuck != "" {
		t.Fatalf("stuck: %s", p.Stuck)
	}
}
