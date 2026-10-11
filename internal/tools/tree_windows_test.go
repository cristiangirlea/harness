//go:build windows

package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

var treeTestKernel = syscall.NewLazyDLL("kernel32.dll")
var treeTestTimes = treeTestKernel.NewProc("GetProcessTimes")
var treeTestExit = treeTestKernel.NewProc("GetExitCodeProcess")

func treeProcessState(pid int) (string, bool, error) {
	h, err := syscall.OpenProcess(0x1000, false, uint32(pid))
	if err == syscall.Errno(87) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer syscall.CloseHandle(h)
	var created, exited, kernel, user syscall.Filetime
	if ok, _, err := treeTestTimes.Call(uintptr(h), uintptr(unsafe.Pointer(&created)), uintptr(unsafe.Pointer(&exited)), uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user))); ok == 0 {
		return "", false, err
	}
	var code uint32
	if ok, _, err := treeTestExit.Call(uintptr(h), uintptr(unsafe.Pointer(&code))); ok == 0 {
		return "", false, err
	}
	return fmt.Sprintf("%d:%d", created.HighDateTime, created.LowDateTime), code == 259, nil
}

func TestTreeStartupContainsImmediateGrandchildren(t *testing.T) {
	for i := 0; i < 20; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) { testTreeCancellation(t) })
	}
}

func TestTreeCloseKillsRemainingGrandchildren(t *testing.T) {
	file := filepath.Join(t.TempDir(), "child.json")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcessTree$")
	cmd.Env = append(os.Environ(), "HRN_TREE_HELPER=parent", "HRN_TREE_PID_FILE="+file)
	tree, err := StartTree(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	id := readTreeIdentity(t, file)
	if err := tree.WaitGone(0); err == nil || !strings.Contains(err.Error(), "2 processes still alive") {
		t.Fatalf("want the exact active job count of two: %v", err)
	}
	tree.Close()
	_ = cmd.Wait()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		born, alive, err := treeProcessState(id.PID)
		if err != nil {
			t.Fatal(err)
		}
		if !alive || born != id.Born {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	assertTreeProcessGone(t, id)
	if err := tree.WaitGone(0); err == nil {
		t.Fatal("a closed job cannot prove that its tree is gone")
	}
}

func TestTreeWithoutAJobReportsUnverifiedCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHelperProcessTree$")
	file := filepath.Join(t.TempDir(), "child.json")
	cmd.Env = append(os.Environ(), "HRN_TREE_HELPER=parent", "HRN_TREE_PID_FILE="+file)
	tree, err := startTree(cmd, &Tree{})
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	if tree.Contained() {
		t.Fatal("fallback must report no containment")
	}
	id := readTreeIdentity(t, file)
	cancel()
	_ = cmd.Wait()
	if err := tree.WaitGone(0); err == nil || !strings.Contains(err.Error(), "cannot verify") {
		t.Fatalf("fallback must not claim verified cleanup: %v", err)
	}
	// The fallback is best effort; in a restricted sandbox taskkill can fail
	// and only the parent is killed. readTreeIdentity cleans up any survivor.
	born, alive, err := treeProcessState(id.PID)
	if err != nil {
		t.Fatal(err)
	}
	if alive && born == id.Born {
		t.Log("fallback left the grandchild running; cleanup is correctly unverified")
	}
}

func TestResumingThreadsSkipsOneThatCannotBeResumed(t *testing.T) {
	attempts := 0
	err := resumeInitialThreadWith(uint32(os.Getpid()), func(uint32) error {
		attempts++
		if attempts == 1 {
			return errors.New("thread exited before it could be opened or resumed")
		}
		return nil
	}, nil)
	if err != nil || attempts < 2 {
		t.Fatalf("want later threads resumed after one failure: attempts=%d err=%v", attempts, err)
	}
}

func TestResumingThreadsFailsWhenNoneCanBeResumed(t *testing.T) {
	err := resumeInitialThreadWith(uint32(os.Getpid()), func(uint32) error {
		return errors.New("unreadable thread")
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "unreadable thread") {
		t.Fatalf("want the failure when no thread could be resumed: %v", err)
	}
}

func TestResumingThreadsReportsFailuresEvenWhenAnotherThreadSucceeds(t *testing.T) {
	attempts := 0
	var warnings []string
	err := resumeInitialThreadWith(uint32(os.Getpid()), func(uint32) error {
		attempts++
		if attempts == 1 {
			return errors.New("access denied opening the initial thread")
		}
		return nil
	}, func(err error) { warnings = append(warnings, err.Error()) })
	if err != nil || attempts < 2 || len(warnings) != 1 || !strings.Contains(warnings[0], "thread") || !strings.Contains(warnings[0], "access denied") {
		t.Fatalf("successful partial resume must retain its diagnostic: attempts=%d warnings=%v err=%v", attempts, warnings, err)
	}
}
