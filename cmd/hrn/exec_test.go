package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestExecRequiresTheExplicitV1Protocol(t *testing.T) {
	for _, args := range [][]string{nil, {"--protocol", "v2"}, {"--protocol", ""}, {"--protocol", "v1", "extra"}} {
		code, err := cmdExec(args)
		if code != 2 || err == nil {
			t.Errorf("args %v: code %d err %v, want usage error and 2", args, code, err)
		}
	}
}

func TestHelperExecProtocol(t *testing.T) {
	if os.Getenv("HRN_EXEC_HELPER") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"hrn", "exec"}, os.Args[i+1:]...)
			main()
			return
		}
	}
	os.Exit(99)
}

func TestExecUsageErrorsExitTwoOnStderrWithoutHello(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	t.Setenv("HRN_EXEC_HELPER", "1")
	for _, args := range [][]string{nil, {"--protocol", "v2"}, {"--protocol", "v1", "extra"}} {
		cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestHelperExecProtocol$", "--"}, args...)...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 2 {
			t.Fatalf("args %v: exit %v, want 2", args, err)
		}
		if stdout.Len() != 0 || !strings.Contains(stderr.String(), "usage: hrn exec --protocol v1") {
			t.Errorf("args %v: stdout %q stderr %q", args, stdout.String(), stderr.String())
		}
	}
}
