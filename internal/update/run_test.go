package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// site is a fake installation: two hrn copies, a candidate, a watch running
// from the first copy and an MCP server from the second.
type site struct {
	t          *testing.T
	a, b       string
	candidate  string
	version    VersionFunc
	starter    *fakeStarter
	procs      fakeLister
	out        bytes.Buffer
	home       string
	versionMap map[string]string
}

func newSite(t *testing.T) *site {
	t.Helper()
	s := &site{t: t, home: t.TempDir()}
	s.a = filepath.Join(t.TempDir(), exe("hrn"))
	s.b = filepath.Join(t.TempDir(), exe("hrn"))
	s.candidate = filepath.Join(t.TempDir(), exe("hrn"))
	fakeHrn(t, s.a, "old")
	fakeHrn(t, s.b, "old")
	fakeHrn(t, s.candidate, "new build")
	s.versionMap = map[string]string{s.a: oldLine, s.b: oldLine, s.candidate: newLine}
	s.version = fakeVersion(t, s.versionMap)
	s.starter = &fakeStarter{}
	s.procs = fakeLister{
		{PID: 11, Exe: s.a, Args: []string{"watch", "--lan", "--addr", "127.0.0.1:9000"}},
		{PID: 12, Exe: s.b, Args: []string{"mcp", "--dir", "/w"}, PPID: 5, ParentName: "claude" + exe("")},
		{PID: 13, Exe: s.a, Args: []string{"update", "--dry-run"}},
		{PID: 14, Exe: "/elsewhere/hrn", Args: []string{"watch"}},
	}
	return s
}

func (s *site) options() Options {
	return Options{
		Source:    Source{File: s.candidate},
		Targets:   []string{s.a, s.b},
		Version:   s.version,
		Lister:    s.procs,
		Restarter: s.starter.restarter(s.home, nil),
		Home:      s.home,
		Now:       func() time.Time { return stamp },
	}
}

func (s *site) run(o Options) (int, error) {
	s.out.Reset()
	return Run(context.Background(), &s.out, o)
}

func (s *site) contents(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		s.t.Fatal(err)
	}
	return string(b)
}

func wantLines(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
}

func TestRunUpdatesEveryTargetRestartsTheWatchAndListsTheMCPServers(t *testing.T) {
	s := newSite(t)
	code, err := s.run(s.options())
	if err != nil || code != 0 {
		t.Fatalf("code %d, err %v\n%s", code, err, s.out.String())
	}
	for _, target := range []string{s.a, s.b} {
		if s.contents(target) != s.contents(s.candidate) {
			t.Errorf("%s was not replaced", target)
		}
		backups, _ := Backups(target)
		if len(backups) != 1 || filepath.Base(backups[0]) != "hrn-a0e5d2f12345-20261015-0907.prev"+filepath.Ext(target) {
			t.Errorf("backups beside %s: %v", target, backups)
		}
	}
	wantLines(t, s.out.String(),
		"installed: "+oldLine,
		"candidate: "+newLine,
		s.a+"  "+oldLine,
		s.b+"  "+oldLine,
		"updated "+s.a,
		"updated "+s.b,
		"watch restarted on 127.0.0.1:9000 (pid 4242",
		"still on the old build until their host restarts them:",
		"pid 12 hrn mcp --dir /w (parent 5 claude",
	)
	if len(s.starter.stopped) != 1 || s.starter.stopped[0] != 11 {
		t.Errorf("stopped %v; want the watch only", s.starter.stopped)
	}
	if s.starter.exe != s.a || strings.Join(s.starter.args, " ") != "watch --lan --addr 127.0.0.1:9000" {
		t.Errorf("started %s %v", s.starter.exe, s.starter.args)
	}
	if s.starter.stdout != filepath.Join(s.home, "watch-9000.log") {
		t.Errorf("log %q", s.starter.stdout)
	}
}

func TestRunStopsWhenTheCandidateIsAlreadyCurrent(t *testing.T) {
	s := newSite(t)
	s.versionMap[s.candidate] = "hrn 0.2.0 (go1.26.5, a0e5d2f12345)"
	s.version = fakeVersion(t, s.versionMap)
	before := s.contents(s.a)
	code, err := s.run(s.options())
	if err != nil || code != 0 {
		t.Fatalf("code %d, err %v", code, err)
	}
	wantLines(t, s.out.String(), "already current")
	if s.contents(s.a) != before || len(s.starter.stopped) != 0 {
		t.Error("something changed")
	}
	if strings.Contains(s.out.String(), "updated") {
		t.Errorf("the targets were touched:\n%s", s.out.String())
	}
}

func TestRunContinuesPastAFailedTargetAndExitsOne(t *testing.T) {
	s := newSite(t)
	candidateSize, _ := os.Stat(s.candidate)
	inner := s.version
	// b's new file answers with the wrong line: its verify fails and the
	// backup goes back, while a is updated and its watch restarted.
	s.version = func(ctx context.Context, path string) (Version, error) {
		if info, err := os.Stat(path); err == nil && path == s.b && info.Size() == candidateSize.Size() {
			return parse(t, "hrn 0.2.0 (go1.26.4, bad000000000)"), nil
		}
		return inner(ctx, path)
	}
	before := s.contents(s.b)

	code, err := s.run(s.options())
	if err != nil || code != 1 {
		t.Fatalf("code %d, err %v\n%s", code, err, s.out.String())
	}
	if s.contents(s.b) != before {
		t.Error("b was not put back")
	}
	if s.contents(s.a) != s.contents(s.candidate) {
		t.Error("a was not updated")
	}
	wantLines(t, s.out.String(), "updated "+s.a, "failed "+s.b, "bad000000000", "the old binary was put back", "watch restarted on 127.0.0.1:9000")
	if got := names(t, filepath.Dir(s.b)); len(got) != 1 {
		t.Errorf("files beside b: %v", got)
	}
}

func TestRunDoesNotRestartAWatchWhoseBinaryFailedToUpdate(t *testing.T) {
	s := newSite(t)
	s.procs = fakeLister{{PID: 11, Exe: s.b, Args: []string{"watch"}}}
	candidateSize, _ := os.Stat(s.candidate)
	inner := s.version
	s.version = func(ctx context.Context, path string) (Version, error) {
		if info, err := os.Stat(path); err == nil && path == s.b && info.Size() == candidateSize.Size() {
			return Version{}, fmt.Errorf("%w: crashed", ErrNotHrn)
		}
		return inner(ctx, path)
	}
	code, _ := s.run(s.options())
	if code != 1 {
		t.Fatalf("code %d\n%s", code, s.out.String())
	}
	if len(s.starter.stopped) != 0 || s.starter.exe != "" {
		t.Errorf("the watch on the failed binary was touched: stopped %v, started %q", s.starter.stopped, s.starter.exe)
	}
	wantLines(t, s.out.String(), "watch (pid 11) left running on the old build")
}

func TestRunLeavesAloneATargetThatIsNotHrn(t *testing.T) {
	s := newSite(t)
	git := filepath.Join(t.TempDir(), exe("hrn"))
	os.WriteFile(git, []byte("#!/bin/sh\necho git version 2.47\n"), 0o755)
	o := s.options()
	o.Targets = []string{s.a, git}
	code, err := s.run(o)
	if err != nil || code != 0 {
		t.Fatalf("code %d, err %v\n%s", code, err, s.out.String())
	}
	if s.contents(git) != "#!/bin/sh\necho git version 2.47\n" {
		t.Error("the stranger was replaced")
	}
	wantLines(t, s.out.String(), git+"  not hrn, left alone", "updated "+s.a)
	if got := names(t, filepath.Dir(git)); len(got) != 1 {
		t.Errorf("files beside the stranger: %v", got)
	}
}

func TestRunDryRunPrintsThePlanAndChangesNothing(t *testing.T) {
	s := newSite(t)
	o := s.options()
	o.DryRun = true
	beforeA, beforeB := s.contents(s.a), s.contents(s.b)
	code, err := s.run(o)
	if err != nil || code != 0 {
		t.Fatalf("code %d, err %v\n%s", code, err, s.out.String())
	}
	if s.contents(s.a) != beforeA || s.contents(s.b) != beforeB {
		t.Error("a target changed")
	}
	for _, d := range []string{filepath.Dir(s.a), filepath.Dir(s.b)} {
		if got := names(t, d); len(got) != 1 {
			t.Errorf("files in %s: %v", d, got)
		}
	}
	if len(s.starter.stopped) != 0 || s.starter.exe != "" {
		t.Error("a process was stopped or started")
	}
	wantLines(t, s.out.String(),
		"installed: "+oldLine,
		"candidate: "+newLine,
		"would rename "+s.a+" to hrn-a0e5d2f12345-20261015-0907.prev"+filepath.Ext(s.a)+" and copy the candidate in",
		"would rename "+s.b,
		"would restart watch (pid 11): hrn watch --lan --addr 127.0.0.1:9000",
		"pid 12 hrn mcp --dir /w",
		"dry run: nothing changed",
	)
}

func TestRunRollsBackEveryTargetWithABackup(t *testing.T) {
	s := newSite(t)
	if code, err := s.run(s.options()); code != 0 || err != nil {
		t.Fatalf("update: %d %v\n%s", code, err, s.out.String())
	}
	s.starter = &fakeStarter{}
	o := s.options()
	o.Rollback = true
	o.Source = Source{}
	o.Now = func() time.Time { return stamp.Add(time.Hour) }
	// After the update the watch runs from a's new build; it is restarted
	// again after the rollback.
	code, err := s.run(o)
	if err != nil || code != 0 {
		t.Fatalf("code %d, err %v\n%s", code, err, s.out.String())
	}
	for _, target := range []string{s.a, s.b} {
		if v, err := s.version(context.Background(), target); err != nil || v.Line != oldLine {
			t.Errorf("%s says %q, %v", target, v.Line, err)
		}
	}
	wantLines(t, s.out.String(), "rolled back "+s.a+" to "+oldLine, "rolled back "+s.b, "watch restarted on 127.0.0.1:9000")
	if len(s.starter.stopped) != 1 || s.starter.exe != s.a {
		t.Errorf("stopped %v started %q", s.starter.stopped, s.starter.exe)
	}
	if strings.Contains(s.out.String(), "candidate:") {
		t.Error("a rollback has no candidate")
	}

	// A second rollback finds no backup and fails.
	code, err = s.run(o)
	if code != 1 || err != nil {
		t.Fatalf("second rollback: code %d, err %v\n%s", code, err, s.out.String())
	}
	wantLines(t, s.out.String(), "no backup beside it")
}

func TestRunRefusesAFileCandidateThatIsNotARegularFile(t *testing.T) {
	s := newSite(t)
	o := s.options()
	o.Source = Source{File: t.TempDir()}
	if _, err := s.run(o); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("err = %v", err)
	}
	o.Source = Source{File: filepath.Join(t.TempDir(), "missing")}
	if _, err := s.run(o); err == nil {
		t.Fatal("a missing file was accepted")
	}
}

func TestRunRefusesACandidateThatIsNotHrn(t *testing.T) {
	s := newSite(t)
	stranger := filepath.Join(t.TempDir(), exe("hrn"))
	os.WriteFile(stranger, []byte("not a program"), 0o755)
	o := s.options()
	o.Source = Source{File: stranger}
	before := s.contents(s.a)
	if _, err := s.run(o); err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("err = %v", err)
	}
	if s.contents(s.a) != before {
		t.Error("a target changed")
	}
}

func TestRunDownloadsTheReleaseWhenNoSourceIsGiven(t *testing.T) {
	s := newSite(t)
	binary, _ := os.ReadFile(s.candidate)
	sum := sha256.Sum256(binary)
	srv, _ := fakeGitHub(t, binary, fmt.Sprintf(sumsFixture, hex.EncodeToString(sum[:])))
	o := s.options()
	o.Source = Source{Release: &ReleaseSource{API: srv.URL + "/repos/cristiangirlea/harness/releases/latest", Client: srv.Client(), GOOS: "windows", GOARCH: "amd64"}}
	o.TempDir = t.TempDir()

	code, err := s.run(o)
	if err != nil || code != 0 {
		t.Fatalf("code %d, err %v\n%s", code, err, s.out.String())
	}
	wantLines(t, s.out.String(), "release v0.2.0", "candidate: "+newLine, "updated "+s.a)
	if s.contents(s.a) != string(binary) {
		t.Error("a is not the downloaded binary")
	}
	if got := names(t, o.TempDir); len(got) != 0 {
		t.Errorf("the download was not cleaned up: %v", got)
	}
}

func TestRunBuildsFromARefWhenAsked(t *testing.T) {
	s := newSite(t)
	o := s.options()
	o.Source = Source{Ref: "main"}
	var ref string
	o.Build = func(ctx context.Context, r, dir string) (string, error) {
		ref = r
		path := filepath.Join(dir, exe("hrn"))
		fakeHrn(t, path, "new build")
		return path, nil
	}
	code, err := s.run(o)
	if err != nil || code != 0 {
		t.Fatalf("code %d, err %v\n%s", code, err, s.out.String())
	}
	if ref != "main" {
		t.Errorf("built %q", ref)
	}
	wantLines(t, s.out.String(), "building cmd/hrn@main", "candidate: "+newLine, "updated "+s.b)
}

func TestRunRefusesTwoSources(t *testing.T) {
	s := newSite(t)
	o := s.options()
	o.Source = Source{Ref: "main", File: s.candidate}
	if _, err := s.run(o); err == nil {
		t.Fatal("two sources accepted")
	}
}

func TestRunSaysWhenThereIsNothingToUpdate(t *testing.T) {
	s := newSite(t)
	o := s.options()
	o.Targets = nil
	o.Dirs = []string{t.TempDir()}
	o.Running = filepath.Join(t.TempDir(), "gone")
	if _, err := s.run(o); err == nil || !strings.Contains(err.Error(), "no hrn") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunCarriesOnWhenTheProcessListIsUnavailable(t *testing.T) {
	s := newSite(t)
	o := s.options()
	o.Lister = failingLister{}
	code, err := s.run(o)
	if err != nil || code != 0 {
		t.Fatalf("code %d, err %v\n%s", code, err, s.out.String())
	}
	wantLines(t, s.out.String(), "updated "+s.a, "could not list processes", "restart hrn watch by hand")
}

type failingLister struct{}

func (failingLister) Processes() ([]Process, error) { return nil, fmt.Errorf("ps: not found") }
