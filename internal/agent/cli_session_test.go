package agent

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"
)

func TestHelperCLISessions(t *testing.T) {
	if os.Getenv("HRN_SESSION_HELPER") != "1" {
		return
	}
	for _, id := range []string{"first", "first", "second", "second"} {
		fmt.Printf("{\"type\":\"thread.started\",\"thread_id\":%q}\n", id)
	}
	os.Exit(0)
}

func TestCLIEmitsEachNewSessionOnceAfterParsing(t *testing.T) {
	t.Setenv("HRN_SESSION_HELPER", "1")
	b := helperCLI(parseCodexStream)
	b.args = func(Task, string) []string { return []string{"-test.run=^TestHelperCLISessions$", "--"} }
	var sessions []string
	res := b.Run(context.Background(), Task{Prompt: "work", Dir: t.TempDir()}, func(e Event) {
		if e.Kind == EventSession {
			sessions = append(sessions, e.Text)
			if e.Time.IsZero() {
				t.Error("session event needs a timestamp")
			}
		}
	})
	if res.Status != StatusOK || res.Session != "second" || !reflect.DeepEqual(sessions, []string{"first", "second"}) {
		t.Fatalf("sessions %v result %+v", sessions, res)
	}
}
