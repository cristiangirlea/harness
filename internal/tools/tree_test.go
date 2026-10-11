package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type treeIdentity struct {
	PID  int
	Born string
}

// TestHelperProcessTree starts its child before doing any other work.
func TestHelperProcessTree(t *testing.T) {
	switch os.Getenv("HRN_TREE_HELPER") {
	case "parent":
		child := exec.Command(os.Args[0], "-test.run=^TestHelperProcessTree$")
		child.Env = append(os.Environ(), "HRN_TREE_HELPER=sleep")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			panic(err)
		}
		born, _, err := treeProcessState(child.Process.Pid)
		if err != nil {
			panic(err)
		}
		data, _ := json.Marshal(treeIdentity{child.Process.Pid, born})
		if err := os.WriteFile(os.Getenv("HRN_TREE_PID_FILE"), data, 0600); err != nil {
			panic(err)
		}
		time.Sleep(time.Minute)
		os.Exit(0)
	case "sleep":
		time.Sleep(time.Minute)
		os.Exit(0)
	}
}

func readTreeIdentity(t *testing.T, file string) treeIdentity {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(file)
		var id treeIdentity
		if err == nil && json.Unmarshal(data, &id) == nil && id.PID > 0 {
			t.Cleanup(func() {
				born, alive, err := treeProcessState(id.PID)
				if err == nil && alive && born == id.Born {
					p, _ := os.FindProcess(id.PID)
					if p != nil {
						_ = p.Kill()
					}
				}
			})
			return id
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("helper did not write its grandchild identity")
	return treeIdentity{}
}

func assertTreeProcessGone(t *testing.T, id treeIdentity) {
	t.Helper()
	born, alive, err := treeProcessState(id.PID)
	if err != nil {
		t.Fatal(err)
	}
	if alive && born == id.Born {
		t.Fatalf("grandchild %d (created %s) still running", id.PID, id.Born)
	}
}

func testTreeCancellation(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	file := filepath.Join(t.TempDir(), "child.json")
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHelperProcessTree$")
	cmd.Env = append(os.Environ(), "HRN_TREE_HELPER=parent", "HRN_TREE_PID_FILE="+file)
	tree, err := StartTree(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	if !tree.Contained() {
		t.Fatal("job creation or assignment refused: tree is not contained")
	}
	id := readTreeIdentity(t, file)
	if err := tree.WaitGone(30 * time.Millisecond); err == nil || !strings.Contains(err.Error(), "still alive") {
		t.Fatalf("WaitGone must report surviving processes: %v", err)
	}
	cancel()
	_ = cmd.Wait()
	if err := tree.WaitGone(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	assertTreeProcessGone(t, id)
}

func TestTreeCancellationKillsTheGrandchild(t *testing.T) { testTreeCancellation(t) }

func TestTreeStartsACommandWithoutAContext(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcessTree$")
	cmd.Env = append(os.Environ(), "HRN_TREE_HELPER=")
	tree, err := StartTree(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := tree.WaitGone(time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestTreeReturnsStartErrors(t *testing.T) {
	cmd := exec.Command(filepath.Join(t.TempDir(), "missing-executable"))
	tree, err := StartTree(cmd)
	if err == nil || tree != nil {
		t.Fatalf("tree=%v err=%v, want no tree and a start error", tree, err)
	}
}

func TestTreeHandlesCancellationDuringStartup(t *testing.T) {
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		timer := time.AfterFunc(time.Millisecond, cancel)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHelperProcessTree$")
		cmd.Env = append(os.Environ(), "HRN_TREE_HELPER=sleep")
		tree, err := StartTree(cmd)
		timer.Stop()
		cancel()
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			continue
		}
		_ = cmd.Wait()
		err = tree.WaitGone(time.Second)
		tree.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestTreeKillStopsChildrenWithoutContextCancellation(t *testing.T) {
	file := filepath.Join(t.TempDir(), "child.json")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcessTree$")
	cmd.Env = append(os.Environ(), "HRN_TREE_HELPER=parent", "HRN_TREE_PID_FILE="+file)
	tree, err := StartTree(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	id := readTreeIdentity(t, file)
	if err := tree.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if err := tree.WaitGone(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	if err := tree.Kill(); err != nil {
		t.Fatalf("killing an already gone tree: %v", err)
	}
	assertTreeProcessGone(t, id)
}
