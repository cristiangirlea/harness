package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/purpose"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestHelperRunPurpose(t *testing.T) {
	if os.Getenv("HRN_TEST_RUN_PURPOSE") != "1" {
		return
	}
	os.Args = append([]string{os.Args[0], "run"}, os.Args[3:]...)
	main()
}

func TestRunRefusesInvalidPurposeBeforeStarting(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	for _, value := range []string{"x", "unknown", "Review", ""} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperRunPurpose$", "--", "--purpose", value, "-b", "stub-cmd", "--no-switch", "fix it")
		cmd.Env = append(os.Environ(), "HRN_TEST_RUN_PURPOSE=1")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err := cmd.Run()
		if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 2 {
			t.Fatalf("purpose %q: error=%v, stderr=%s", value, err, &stderr)
		}
		if !strings.Contains(stderr.String(), "run: --purpose "+value+": one of "+strings.Join(purpose.Declarable(), ", ")) {
			t.Errorf("purpose %q: stderr=%s", value, &stderr)
		}
	}
	if recs, err := runlog.List(0); err != nil || len(recs) != 0 {
		t.Fatalf("refused runs left records: %v, %v", recs, err)
	}
}

func TestRunStoresTheDeclarationWithoutChangingThePrompt(t *testing.T) {
	for _, tc := range []struct{ flag, prompt, want string }{
		{"plan", "Purpose: review\nFix it.", "plan"},
		{"", "\nPurpose: ReViEw\nFix it.", "review"},
		{"", "Purpose: free text\nFix it.", ""},
		{"", "Purpose: unknown\nFix it.", ""},
		{"", "Fix it.", ""},
	} {
		t.Run(tc.prompt+tc.flag, func(t *testing.T) {
			drain(t)
			t.Setenv("HRN_HOME", t.TempDir())
			args := []string{"-b", "stub-cmd", "-C", t.TempDir(), "--no-switch"}
			if tc.flag != "" {
				args = append(args, "--purpose", tc.flag)
			}
			code, err := cmdRun(append(args, tc.prompt))
			if code != 0 || err != nil {
				t.Fatalf("code=%d error=%v", code, err)
			}
			task := <-recorded
			if task.Prompt != tc.prompt {
				t.Errorf("backend prompt = %q, want %q", task.Prompt, tc.prompt)
			}
			recs, err := runlog.List(0)
			if err != nil || len(recs) != 1 {
				t.Fatalf("records=%v error=%v", recs, err)
			}
			data, err := os.ReadFile(filepath.Join(runlog.Root(), recs[0].ID, "task.json"))
			if err != nil {
				t.Fatal(err)
			}
			if tc.want != "" && !strings.Contains(string(data), `"purpose": "`+tc.want+`"`) {
				t.Errorf("task.json lacks purpose %q: %s", tc.want, data)
			}
			if tc.want == "" && strings.Contains(string(data), `"purpose"`) {
				t.Errorf("undeclared task has purpose: %s", data)
			}
		})
	}
}
