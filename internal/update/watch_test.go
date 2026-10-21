package update

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeLister []Process

func (f fakeLister) Processes() ([]Process, error) { return []Process(f), nil }

func TestWatchesOfFindsTheWatchProcessesRunningFromATarget(t *testing.T) {
	target := filepath.Join(t.TempDir(), exe("hrn"))
	other := filepath.Join(t.TempDir(), exe("hrn"))
	procs := []Process{
		{PID: 11, Exe: target, Args: []string{"watch", "--lan", "--addr", "127.0.0.1:9000"}},
		{PID: 12, Exe: target, Args: []string{"mcp", "--dir", "/w"}, PPID: 5, ParentName: "claude.exe"},
		{PID: 13, Exe: other, Args: []string{"watch"}},
		{PID: 14, Exe: target, Args: []string{"run", "watch", "the", "film"}},
		{PID: 15, Exe: target, Args: []string{"watch"}},
		{PID: 16, Exe: "hrn.exe", Args: nil},
		{PID: 17, Exe: target, Args: []string{"update"}},
	}
	got := WatchesOf(procs, []string{target})
	want := []Watch{
		{PID: 11, Exe: target, Args: []string{"watch", "--lan", "--addr", "127.0.0.1:9000"}, Addr: "127.0.0.1:9000"},
		{PID: 15, Exe: target, Args: []string{"watch"}, Addr: "127.0.0.1:8089"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
	mcp := MCPServersOf(procs, []string{target})
	if len(mcp) != 1 || mcp[0].PID != 12 {
		t.Errorf("mcp servers %+v; want pid 12 only", mcp)
	}
}

func TestWatchAddrReadsEverySpellingOfTheFlag(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"watch"}, "127.0.0.1:8089"},
		{[]string{"watch", "--addr", "127.0.0.1:9000"}, "127.0.0.1:9000"},
		{[]string{"watch", "--addr=127.0.0.1:9001"}, "127.0.0.1:9001"},
		{[]string{"watch", "-addr", "localhost:9002", "--lan"}, "localhost:9002"},
		{[]string{"watch", "--lan=Local Area Connection", "--addr", ":9003"}, ":9003"},
	} {
		if got := watchAddr(tc.args); got != tc.want {
			t.Errorf("%v: %q; want %q", tc.args, got, tc.want)
		}
	}
	if got := watchLog("/home/u/.hrn", "127.0.0.1:9000"); got != filepath.Join("/home/u/.hrn", "watch-9000.log") {
		t.Errorf("log %q", got)
	}
	if got := watchLog("/home/u/.hrn", "nonsense"); got != filepath.Join("/home/u/.hrn", "watch.log") {
		t.Errorf("log without a port %q", got)
	}
}

type fakeStarter struct {
	stopped []int
	exe     string
	args    []string
	stdout  string
	stderr  string
	stopErr error
}

func (f *fakeStarter) restarter(home string, logs func(int) (string, string)) Restarter {
	return Restarter{
		Stop: func(ctx context.Context, pid int) error {
			f.stopped = append(f.stopped, pid)
			return f.stopErr
		},
		Start: func(exe string, args []string, stdout, stderr string) (int, error) {
			f.exe, f.args, f.stdout, f.stderr = exe, args, stdout, stderr
			return 4242, nil
		},
		Logs: logs,
		Home: home,
	}
}

func TestRestartStopsTheOldWatchAndStartsTheNewBinaryWithTheSameArguments(t *testing.T) {
	newExe := filepath.Join(t.TempDir(), exe("hrn"))
	w := Watch{PID: 11, Exe: "/old/hrn", Args: []string{"watch", "--lan", "--addr", "127.0.0.1:9000"}, Addr: "127.0.0.1:9000"}
	f := &fakeStarter{}
	home := t.TempDir()

	pid, log, err := f.restarter(home, nil).Restart(context.Background(), w, newExe)
	if err != nil {
		t.Fatal(err)
	}
	if pid != 4242 {
		t.Errorf("pid %d", pid)
	}
	if !reflect.DeepEqual(f.stopped, []int{11}) {
		t.Errorf("stopped %v; want the old watch only", f.stopped)
	}
	if f.exe != newExe || !reflect.DeepEqual(f.args, w.Args) {
		t.Errorf("started %s %v; want the new binary with the old arguments", f.exe, f.args)
	}
	want := filepath.Join(home, "watch-9000.log")
	if f.stdout != want || f.stderr != want || log != want {
		t.Errorf("logs %q %q (%q); want %q", f.stdout, f.stderr, log, want)
	}
}

func TestRestartKeepsTheLogFilesTheOldWatchWroteTo(t *testing.T) {
	w := Watch{PID: 11, Exe: "/old/hrn", Args: []string{"watch"}, Addr: "127.0.0.1:8089"}
	f := &fakeStarter{}
	logs := func(pid int) (string, string) {
		if pid != 11 {
			t.Errorf("logs asked for pid %d", pid)
		}
		return "/var/log/hrn/out.log", "/var/log/hrn/err.log"
	}
	_, log, err := f.restarter(t.TempDir(), logs).Restart(context.Background(), w, "/new/hrn")
	if err != nil {
		t.Fatal(err)
	}
	if f.stdout != "/var/log/hrn/out.log" || f.stderr != "/var/log/hrn/err.log" || log != "/var/log/hrn/out.log" {
		t.Errorf("logs %q %q (%q)", f.stdout, f.stderr, log)
	}
}

func TestRestartDoesNotStartWhenTheOldWatchCouldNotBeStopped(t *testing.T) {
	w := Watch{PID: 11, Exe: "/old/hrn", Args: []string{"watch"}, Addr: "127.0.0.1:8089"}
	f := &fakeStarter{stopErr: errors.New("access denied")}
	_, _, err := f.restarter(t.TempDir(), nil).Restart(context.Background(), w, "/new/hrn")
	if err == nil || f.exe != "" {
		t.Fatalf("err = %v, started %q; want the stop error and no start", err, f.exe)
	}
}

func TestDescribeMCPServerNamesPidParentAndStart(t *testing.T) {
	start := time.Date(2026, 10, 15, 9, 7, 0, 0, time.Local)
	p := Process{PID: 12, PPID: 5, ParentName: "claude.exe", Exe: "/x/hrn", Args: []string{"mcp", "--dir", "/w"}, Start: start}
	got := DescribeProcess(p)
	for _, want := range []string{"pid 12", "hrn mcp --dir /w", "parent 5 claude.exe", "since 2026-10-15 09:07"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q lacks %q", got, want)
		}
	}
	bare := DescribeProcess(Process{PID: 13, Exe: "/x/hrn", Args: []string{"mcp"}})
	if strings.Contains(bare, "parent") || strings.Contains(bare, "since") {
		t.Errorf("%q says what it does not know", bare)
	}
}
