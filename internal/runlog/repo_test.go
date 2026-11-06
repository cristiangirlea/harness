package runlog

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/kernel"
)

func TestRepoNameKeepsOnlyTheHostAndPath(t *testing.T) {
	for remote, want := range map[string]string{
		"https://github.com/acme/tool.git":                    "github.com/acme/tool",
		"https://github.com/acme/tool":                        "github.com/acme/tool",
		"https://github.com/acme/tool/":                       "github.com/acme/tool",
		"https://user:s3cret-token@github.com/acme/tool.git":  "github.com/acme/tool",
		"https://x-access-token:abc@GitHub.com:443/Acme/Tool": "github.com/Acme/Tool",
		"ssh://git@github.com:2222/acme/tool.git":             "github.com/acme/tool",
		"git@github.com:acme/tool.git":                        "github.com/acme/tool",
		"git@github-work:acme/tool.git":                       "github-work/acme/tool",
		"gitlab.example.com:group/sub/tool":                   "gitlab.example.com/group/sub/tool",
		"https://gitlab.example.com/group/sub/tool.git":       "gitlab.example.com/group/sub/tool",
		"git://example.org/acme/.github.git":                  "example.org/acme/.github",
		" https://github.com/acme/tool.git\n":                 "github.com/acme/tool",
		"":                                                    "",
		"/srv/git/tool.git":                                   "",
		"../tool":                                             "",
		`C:\src\tool`:                                         "",
		"C:/src/tool":                                         "",
		"file:///srv/git/acme/tool.git":                       "",
		"https://github.com/tool":                             "",
		"https://github.com/acme/../tool":                     "",
		"https://github.com/acme/%2e%2e":                      "",
		"https://github.com/acme/to ol":                       "",
		"https://github.com/acme/tool?x=1#y":                  "github.com/acme/tool",
		"https://github.com/acme/<script>":                    "",
		"ssh://host/~user/tool.git":                           "",
		"https://exa_mple.com/acme/tool":                      "",
		"ext::sh -c touch% /tmp/pwned":                        "",
		"https://github.com/acme/tool.git.git":                "github.com/acme/tool.git",
	} {
		if got := RepoName(remote); got != want {
			t.Errorf("RepoName(%q) = %q, want %q", remote, got, want)
		}
	}
}

func TestPickRemotePrefersOriginThenTheOnlyOne(t *testing.T) {
	if got := PickRemote(map[string]string{"upstream": "u", "origin": "o"}); got != "o" {
		t.Fatalf("origin: %q", got)
	}
	if got := PickRemote(map[string]string{"fork": "f"}); got != "f" {
		t.Fatalf("only one: %q", got)
	}
	if got := PickRemote(map[string]string{"a": "a", "b": "b"}); got != "" {
		t.Fatalf("two without origin: %q", got)
	}
}

func TestRemotesInConfigReadsCommentsQuotesAndTabsAsGitDoes(t *testing.T) {
	cfg := "[remote\t\"origin\"] # where it came from\n\turl = \"https://github.com/acme/tool.git\" ; the fork\n" +
		"[remote \"q\\\"x\"]\n\turl = git@github.com:acme/q.git # a comment\n" +
		"[remote \"hash\"]\n\turl = \"https://github.com/acme/a#b\"\n"
	want := map[string]string{"origin": "https://github.com/acme/tool.git", `q"x`: "git@github.com:acme/q.git", "hash": "https://github.com/acme/a#b"}
	if got := RemotesInConfig(cfg); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestValidRepoTakesWhatRepoNameGives(t *testing.T) {
	for name, want := range map[string]bool{
		"github.com/acme/tool": true, "github.com/acme/tool.git": true, "gitlab.example.com/a/b/c": true,
		"github.com/tool": false, "github.com/acme/..": false, "GitHub.com/acme/tool": false, "github.com/acme/to ol": false, "": false,
	} {
		if got := ValidRepo(name); got != want {
			t.Errorf("ValidRepo(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestRemotesInConfigReadsEachRemotesFirstURL(t *testing.T) {
	cfg := "[core]\n\turl = not-a-remote\n[remote \"origin\"]\n\turl = https://github.com/acme/tool.git\n\tURL = second\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n" +
		"[Remote \"Up.Stream\"]\n  url=\"git@github.com:acme/up.git\"\n[branch \"main\"]\n\turl = nope\n[remote.old]\n\turl = old-style\n"
	want := map[string]string{"origin": "https://github.com/acme/tool.git", "Up.Stream": "git@github.com:acme/up.git"}
	if got := RemotesInConfig(cfg); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestARunRecordsItsRepositoryAndBranchWithoutCredentials(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	record := func() Record {
		t.Helper()
		w, err := Create("test", kernel.Task{Dir: dir}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Close(kernel.Result{}); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(Root(), w.ID(), "task.json"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "s3cret") {
			t.Fatalf("the remote's token reached the record: %s", raw)
		}
		r, err := Load(w.ID())
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	git("init")
	git("config", "user.email", "test@example.test")
	git("config", "user.name", "Test")
	git("checkout", "-b", "feat/remote-name")
	git("commit", "--allow-empty", "-m", "initial")

	if r := record(); r.Repo != "" || r.Branch != "feat/remote-name" {
		t.Fatalf("no remote: repo %q branch %q", r.Repo, r.Branch)
	}
	git("checkout", "--orphan", "fresh")
	git("remote", "add", "origin", "https://github.com/acme/tool.git")
	if r := record(); r.Repo != "github.com/acme/tool" || r.Branch != "fresh" || r.StartHead != "" {
		t.Fatalf("no commit on the branch yet: repo %q branch %q head %q", r.Repo, r.Branch, r.StartHead)
	}
	git("checkout", "feat/remote-name")
	git("remote", "remove", "origin")
	git("remote", "add", "upstream", "https://github.com/other/tool.git")
	git("remote", "add", "origin", "https://user:s3cret@github.com/acme/tool.git")
	if r := record(); r.Repo != "github.com/acme/tool" || r.Branch != "feat/remote-name" {
		t.Fatalf("origin: repo %q branch %q", r.Repo, r.Branch)
	}
	git("checkout", "--detach")
	if r := record(); r.Repo != "github.com/acme/tool" || r.Branch != "" || r.StartHead == "" {
		t.Fatalf("detached: repo %q branch %q head %q", r.Repo, r.Branch, r.StartHead)
	}
}

func TestARunOutsideARepositoryRecordsNoRepository(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	w, err := Create("test", kernel.Task{Dir: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(kernel.Result{}); err != nil {
		t.Fatal(err)
	}
	r, err := Load(w.ID())
	if err != nil {
		t.Fatal(err)
	}
	if r.Repo != "" || r.Branch != "" {
		t.Fatalf("repo %q branch %q", r.Repo, r.Branch)
	}
}
