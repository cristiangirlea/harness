package workmap

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/cristiangirlea/harness/internal/prefs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSessionsJoinAndRespectActivity(t *testing.T) {
	home, repo := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	dir := filepath.Join(home, "projects", "fixture")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	id := "12345678-1234-1234-1234-123456789abc"
	path := filepath.Join(dir, id+".jsonl")
	data := `{"cwd":` + strconv.Quote(repo) + `,"gitBranch":"feature","sessionId":"` + id + `","message":"secret-value"}` + "\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	pieces := map[string]*Piece{}
	if err := sessionSources(map[string]string{repo: repo}, map[string]map[string]string{repo: {}}, map[string]string{repo: "main"}, pieces); err != nil {
		t.Fatal(err)
	}
	p := pieces[repo+"\x00branch:feature"]
	if p == nil || p.Stage != Agent || !p.ActiveSession || len(p.Links.Sessions) != 1 || p.Links.Sessions[0] != id {
		t.Fatalf("piece: %+v", p)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	pieces = map[string]*Piece{}
	if err := sessionSources(map[string]string{repo: repo}, map[string]map[string]string{repo: {}}, map[string]string{repo: "main"}, pieces); err != nil {
		t.Fatal(err)
	}
	p = pieces[repo+"\x00branch:feature"]
	if p == nil || p.Stage != Branch || p.ActiveSession || len(p.Links.Sessions) != 1 {
		t.Fatalf("piece: %+v", p)
	}
}

func TestSessionsFilterAndReadOnlyTheTail(t *testing.T) {
	home, repo := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	dir := filepath.Join(home, "projects", "fixture")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	id := "12345678-1234-1234-1234-123456789abc"
	line := func(cwd, branch string) string {
		return fmt.Sprintf(`{"cwd":%s,"gitBranch":%s,"message":"secret-shaped-value"}`, strconv.Quote(cwd), strconv.Quote(branch)) + "\n"
	}
	data := line(repo, "early") + strings.Repeat("x", 70000) + "\n" + line(repo, "feature") + "{broken\n\xff\n"
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	for i, item := range []struct{ cwd, branch string }{{repo, "main"}, {t.TempDir(), "outside"}} {
		name := fmt.Sprintf("00000000-0000-0000-0000-%012d.jsonl", i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(line(item.cwd, item.branch)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	pieces := map[string]*Piece{}
	if err := sessionSources(map[string]string{repo: repo}, nil, map[string]string{repo: "main"}, pieces); err != nil {
		t.Fatal(err)
	}
	if len(pieces) != 1 || pieces[repo+"\x00branch:feature"] == nil {
		t.Fatalf("pieces: %+v", pieces)
	}
	encoded, err := json.Marshal(pieces)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "secret-shaped-value") || strings.Contains(string(encoded), strings.Repeat("x", 100)) {
		t.Fatal("private content escaped")
	}
}

func TestSessionsLimitFilesToTheNewestTwoHundred(t *testing.T) {
	home, repo := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	dir := filepath.Join(home, "projects", "fixture")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 201; i++ {
		path := filepath.Join(dir, fmt.Sprintf("00000000-0000-0000-0000-%012d.jsonl", i))
		data := fmt.Sprintf(`{"cwd":%s,"gitBranch":"feature"}`, strconv.Quote(repo))
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		tm := time.Now().Add(-time.Duration(i) * time.Second)
		if err := os.Chtimes(path, tm, tm); err != nil {
			t.Fatal(err)
		}
	}
	pieces := map[string]*Piece{}
	if err := sessionSources(map[string]string{repo: repo}, nil, map[string]string{repo: "main"}, pieces); err != nil {
		t.Fatal(err)
	}
	p := pieces[repo+"\x00branch:feature"]
	if p == nil || len(p.Links.Sessions) != 200 {
		t.Fatalf("piece: %+v", p)
	}
	for _, id := range p.Links.Sessions {
		if strings.HasSuffix(id, "000000000200") {
			t.Fatal("oldest included")
		}
	}
}

func TestBuildSkipsDisabledSessions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "not-a-directory"))
	if err := os.WriteFile(filepath.Join(home, "not-a-directory"), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	off := false
	if err := prefs.Save(home, prefs.Prefs{WorkMapSessions: &off}); err != nil {
		t.Fatal(err)
	}
	pieces, err := Build(context.Background(), home, []string{})
	if err != nil || len(pieces) != 0 {
		t.Fatalf("pieces: %+v, error: %v", pieces, err)
	}
}

func TestSessionsDoNotLowerTheExistingStage(t *testing.T) {
	home, repo := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	dir := filepath.Join(home, "projects", "fixture")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "12345678-1234-1234-1234-123456789abc.jsonl")
	if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"cwd":%s,"gitBranch":"feature"}`, strconv.Quote(repo))), 0600); err != nil {
		t.Fatal(err)
	}
	key := repo + "\x00branch:feature"
	pieces := map[string]*Piece{key: {Stage: CI}}
	if err := sessionSources(map[string]string{repo: repo}, nil, map[string]string{repo: "main"}, pieces); err != nil {
		t.Fatal(err)
	}
	if pieces[key].Stage != CI || !pieces[key].ActiveSession {
		t.Fatalf("piece: %+v", pieces[key])
	}
}

func TestSessionsRejectInvalidBranchesAndOutsidePaths(t *testing.T) {
	home, repo := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	dir := filepath.Join(home, "projects", "fixture")
	for i, item := range []struct{ cwd, branch string }{{repo, "HEAD"}, {repo, ""}, {repo, "bad..name"}, {repo, "-option"}, {repo, "bad\nname"}, {repo + "-other", "outside"}} {
		reviewWrite(t, filepath.Join(dir, fmt.Sprintf("00000000-0000-0000-0000-%012d.jsonl", i)), fmt.Sprintf("{\"cwd\":%s,\"gitBranch\":%s}", strconv.Quote(item.cwd), strconv.Quote(item.branch)))
	}
	pieces := map[string]*Piece{}
	if err := sessionSources(map[string]string{repo: repo}, nil, nil, pieces); err != nil || len(pieces) != 0 {
		t.Fatalf("pieces: %+v, error: %v", pieces, err)
	}
}
func TestSessionsMatchWindowsPathCase(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows path semantics")
	}
	home, repo := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	reviewWrite(t, filepath.Join(home, "projects", "fixture", "12345678-1234-1234-1234-123456789abc.jsonl"), fmt.Sprintf("{\"cwd\":%s,\"gitBranch\":\"feature\"}", strconv.Quote(strings.ToUpper(repo))))
	pieces := map[string]*Piece{}
	if err := sessionSources(map[string]string{repo: repo}, nil, nil, pieces); err != nil || len(pieces) != 1 {
		t.Fatalf("pieces: %+v, error: %v", pieces, err)
	}
}
func TestSessionsSkipLinksLongLinesAndNonUUIDNames(t *testing.T) {
	home, repo, outside := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	dir := filepath.Join(home, "projects", "fixture")
	line := fmt.Sprintf("{\"cwd\":%s,\"gitBranch\":\"feature\"}", strconv.Quote(repo))
	reviewWrite(t, filepath.Join(dir, "not-a-uuid.jsonl"), line)
	reviewWrite(t, filepath.Join(dir, "00000000-0000-0000-0000-000000000001.jsonl"), strings.Repeat("x", 65537))
	reviewWrite(t, filepath.Join(outside, "12345678-1234-1234-1234-123456789abc.jsonl"), line)
	before := map[string]*Piece{}
	if err := sessionSources(map[string]string{repo: repo}, nil, nil, before); err != nil || len(before) != 0 {
		t.Fatalf("pieces: %+v, error: %v", before, err)
	}
	if err := os.Symlink(filepath.Join(outside, "12345678-1234-1234-1234-123456789abc.jsonl"), filepath.Join(dir, "00000000-0000-0000-0000-000000000002.jsonl")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(home, "projects", "linked")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	pieces := map[string]*Piece{}
	if err := sessionSources(map[string]string{repo: repo}, nil, nil, pieces); err != nil || len(pieces) != 0 {
		t.Fatalf("pieces: %+v, error: %v", pieces, err)
	}
}
func TestBuildJoinsWorktreeAndKeepsTranscriptPrivateByDefault(t *testing.T) {
	repo, git := reviewRepo(t)
	wt := filepath.Join(t.TempDir(), "linked")
	git("worktree", "add", "-b", "feature", wt)
	home, config := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	reviewWrite(t, filepath.Join(home, "backends.json"), "{}")
	reviewWrite(t, filepath.Join(config, "projects", "fixture", "12345678-1234-1234-1234-123456789abc.jsonl"), fmt.Sprintf("{\"cwd\":%s,\"gitBranch\":\"feature\",\"message\":\"sk-secret-fixture-123456\"}", strconv.Quote(wt)))
	pieces, err := Build(context.Background(), home, []string{repo})
	if err != nil {
		t.Fatal(err)
	}
	p := reviewFind(t, pieces, "branch:feature")
	if len(p.Links.Sessions) != 1 || !p.ActiveSession {
		t.Fatalf("piece: %+v", p)
	}
	data, e := json.Marshal(pieces)
	if e != nil || strings.Contains(string(data), "sk-secret-fixture-123456") || strings.Contains(fmt.Sprint(err), "sk-secret-fixture-123456") {
		t.Fatal("transcript content escaped")
	}
}
