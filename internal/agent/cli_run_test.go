package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestHelperCLIError is not a test: with HX_CLI_HELPER=1 the test binary acts
// as an agent CLI that reports a structured error and then exits non-zero.
func TestHelperCLIError(t *testing.T) {
	if os.Getenv("HX_CLI_HELPER") != "1" {
		return
	}
	fmt.Println(`{"type":"error","error":{"message":"usage limit reached"}}`)
	os.Exit(1)
}

func TestCLIKeepsStreamErrorOnNonZeroExit(t *testing.T) {
	t.Setenv("HX_CLI_HELPER", "1")
	b := &CLIBackend{
		name:        os.Args[0],
		promptStdin: true,
		args:        func(Task, string) []string { return []string{"-test.run=^TestHelperCLIError$", "--"} },
		parse:       parseOpencodeStream,
	}
	res := b.Run(context.Background(), Task{Prompt: "go", Dir: t.TempDir(), Mode: ModeAsk}, func(Event) {})
	if res.Status != StatusError || !strings.Contains(res.Error, "usage limit reached") {
		t.Fatalf("the stream's error should survive a non-zero exit: %+v", res)
	}
}
