package update

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSplitCommandLineFollowsTheWindowsRules(t *testing.T) {
	for _, tc := range []struct {
		line string
		want []string
	}{
		{`hrn.exe watch --lan`, []string{"hrn.exe", "watch", "--lan"}},
		{`"C:\Program Files\hrn\hrn.exe" watch "--lan=Ethernet 3"`, []string{`C:\Program Files\hrn\hrn.exe`, "watch", "--lan=Ethernet 3"}},
		{`hrn mcp --dir "C:\w\my repo\"`, []string{"hrn", "mcp", "--dir", `C:\w\my repo"`}},
		{`hrn mcp --dir "C:\w\my repo\\"`, []string{"hrn", "mcp", "--dir", `C:\w\my repo\`}},
		{`hrn run "say \"hi\""`, []string{"hrn", "run", `say "hi"`}},
		{`  hrn   watch  `, []string{"hrn", "watch"}},
		{`hrn`, []string{"hrn"}},
		{``, nil},
		{`"C:\x y\hrn.exe"`, []string{`C:\x y\hrn.exe`}},
	} {
		if got := splitCommandLine(tc.line); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q: %q; want %q", tc.line, got, tc.want)
		}
	}
}

// The real process list has to find this test process and a child it
// starts, with their arguments, on each system the lister supports.
func TestSystemProcessesListsThisProcessAndItsChildWithTheirArguments(t *testing.T) {
	procs, err := SystemProcesses{}.Processes()
	if err != nil {
		if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
			t.Skip(err)
		}
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	self = mustEval(t, self)

	var me *Process
	for i := range procs {
		if procs[i].PID == os.Getpid() {
			me = &procs[i]
		}
	}
	if me == nil {
		t.Fatalf("pid %d is not in the %d processes listed", os.Getpid(), len(procs))
	}
	if !samePath(mustEval(t, me.Exe), self) {
		t.Errorf("exe %q; want %q", me.Exe, self)
	}
	if !reflect.DeepEqual(me.Args, os.Args[1:]) {
		t.Errorf("args %q; want %q", me.Args, os.Args[1:])
	}
	if me.PPID <= 0 {
		t.Errorf("ppid %d", me.PPID)
	}
	if me.Start.IsZero() || me.Start.After(time.Now().Add(time.Minute)) {
		t.Errorf("start %v", me.Start)
	}

	// A child with the arguments a watch would have, started from a copy of
	// the test binary so its exe is a path of our own.
	copyDir := t.TempDir()
	child := filepath.Join(copyDir, exe("hrn"))
	fakeHrn(t, child, "child")
	cmd := exec.Command(child, "-test.run=^TestHelperSleep$", "--", "watch", "--lan=Ethernet 3", "--addr", "127.0.0.1:1")
	cmd.Env = append(os.Environ(), "HRN_UPDATE_HELPER_SLEEP=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stdin.Close()
		cmd.Wait()
	}()
	procs, err = SystemProcesses{}.Processes()
	if err != nil {
		t.Fatal(err)
	}
	var kid *Process
	for i := range procs {
		if procs[i].PID == cmd.Process.Pid {
			kid = &procs[i]
		}
	}
	if kid == nil {
		t.Fatalf("child pid %d is not listed", cmd.Process.Pid)
	}
	if !samePath(mustEval(t, kid.Exe), mustEval(t, child)) {
		t.Errorf("child exe %q; want %q", kid.Exe, child)
	}
	if want := cmd.Args[1:]; !reflect.DeepEqual(kid.Args, want) {
		t.Errorf("child args %q; want %q", kid.Args, want)
	}
	if kid.PPID != os.Getpid() {
		t.Errorf("child ppid %d; want %d", kid.PPID, os.Getpid())
	}
	if kid.ParentName == "" || !strings.Contains(strings.ToLower(filepath.Base(self)), strings.ToLower(strings.TrimSuffix(kid.ParentName, ".exe"))) {
		t.Errorf("child's parent name %q; want this test binary %q", kid.ParentName, filepath.Base(self))
	}
	// The whole point: a watch is matched by its real exe path. The helper
	// flags come before the hrn arguments, so they are dropped first.
	asHrn := *kid
	asHrn.Args = kid.Args[slices.Index(kid.Args, "--")+1:]
	ws := WatchesOf([]Process{asHrn}, []string{mustEval(t, child)})
	if len(ws) != 1 || ws[0].PID != cmd.Process.Pid || ws[0].Addr != "127.0.0.1:1" {
		t.Errorf("watches %+v; want the child on 127.0.0.1:1", ws)
	}
}

func TestStartDetachedOutlivesItsParentAndStopEndsIt(t *testing.T) {
	if _, err := (SystemProcesses{}).Processes(); err != nil {
		t.Skip(err)
	}
	child := filepath.Join(t.TempDir(), exe("hrn"))
	fakeHrn(t, child, "detached")
	log := filepath.Join(t.TempDir(), "watch-1.log")
	t.Setenv("HRN_UPDATE_HELPER_SLEEP", "1")

	pid, err := StartDetached(child, []string{"-test.run=^TestHelperSleep$", "--", "sleep"}, log, log)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// It is running on its own: cmd.Wait was never called and the handle
	// released, yet the system lists it, writing to the log.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if b, _ := os.ReadFile(log); strings.Contains(string(b), "sleeping") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the detached child never wrote to %s", log)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := Stop(SystemProcesses{})(ctx, pid); err != nil {
		t.Fatalf("stop: %v", err)
	}
	procs, _ := (SystemProcesses{}).Processes()
	for _, p := range procs {
		if p.PID == pid {
			t.Errorf("pid %d still listed after Stop", pid)
		}
	}
}

// ps prints argv[0] as typed: a watch started through PATH shows a bare
// hrn, which must still match the target it runs from.
func TestParsePSResolvesABareProgramNameThroughPATH(t *testing.T) {
	target := filepath.Join(t.TempDir(), exe("hrn"))
	out := []byte(`    1     0 Thu Oct 15 09:00:00 2026     /sbin/launchd
  200     1 Thu Oct 15 09:01:00 2026     -zsh
  300   200 Thu Oct 15 09:02:00 2026     hrn watch --lan
  301   200 Thu Oct 15 09:03:00 2026     ./hrn watch
`)
	lookPath := func(name string) (string, error) {
		if name == "hrn" {
			return target, nil
		}
		return "", exec.ErrNotFound
	}
	procs := parsePS(out, lookPath)
	if len(procs) != 4 {
		t.Fatalf("%d processes: %+v", len(procs), procs)
	}
	w := procs[2]
	if w.PID != 300 || w.Exe != target || !reflect.DeepEqual(w.Args, []string{"watch", "--lan"}) || w.ParentName != "-zsh" {
		t.Errorf("watch %+v; want pid 300 running %s", w, target)
	}
	if procs[3].Exe != "./hrn" {
		t.Errorf("a path relative to the other process's folder was resolved: %q", procs[3].Exe)
	}
	if ws := WatchesOf(procs, []string{target}); len(ws) != 1 || ws[0].PID != 300 {
		t.Errorf("watches %+v; want pid 300", ws)
	}
}
