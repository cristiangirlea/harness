package batch

import (
	"reflect"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/prefs"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestBatchRefusesDisabledBackendsAndFiltersRetries(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	if err := prefs.Save(runlog.Home(), prefs.Prefs{EnabledNames: []string{"codex", "claude"}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name                 string
		backend, taskBackend string
		retry, taskRetry     []string
		refused              bool
	}{
		{name: "spec disabled", backend: "api", refused: true},
		{name: "task disabled", taskBackend: "api", refused: true},
		{name: "spec retries all disabled", retry: []string{"api"}, refused: true},
		{name: "task retries all disabled", taskRetry: []string{"api"}, refused: true},
		{name: "filter both", retry: []string{"claude", "api", "codex", "claude"}, taskRetry: []string{"api", "claude"}},
		{name: "explicit empty", taskRetry: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := Spec{Backend: tc.backend, Retry: tc.retry, Tasks: []Task{{Prompt: "hello", Backend: tc.taskBackend, Retry: tc.taskRetry}}}
			err := s.normalize()
			if tc.refused {
				if err == nil || !strings.Contains(err.Error(), "enabled: codex, claude") {
					t.Fatalf("got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "filter both" && (!reflect.DeepEqual(s.Retry, []string{"claude", "codex", "claude"}) || !reflect.DeepEqual(s.Tasks[0].Retry, []string{"claude"})) {
				t.Fatalf("retries: %v, %v", s.Retry, s.Tasks[0].Retry)
			}
			if tc.name == "explicit empty" && s.Tasks[0].Retry == nil {
				t.Fatal("empty became inherited")
			}
		})
	}
}
