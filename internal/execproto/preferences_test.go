package execproto

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/prefs"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func TestExecRefusesADisabledBackendBeforeCreatingARecord(t *testing.T) {
	called := false
	name := registerFake(t, func(context.Context, agent.Task, func(agent.Event)) agent.Result {
		called = true
		return agent.Result{Status: agent.StatusOK}
	})
	if err := prefs.Save(runlog.Home(), prefs.Prefs{EnabledNames: []string{"claude"}}); err != nil {
		t.Fatal(err)
	}
	input := fmt.Sprintf("{\"type\":\"start\",\"attempt_id\":\"prefs\",\"task\":{\"prompt\":\"hello\",\"dir\":\".\",\"backend\":%q,\"mode\":\"auto\"}}\n", name)
	var out bytes.Buffer
	code := Run(context.Background(), strings.NewReader(input), &out, nil)
	if code == 0 || called || !strings.Contains(out.String(), "backend "+name+" is disabled") {
		t.Fatalf("code %d, called %v, output %s", code, called, out.String())
	}
	records, err := runlog.List(0)
	if err != nil || len(records) != 0 {
		t.Fatalf("records %v, %v", records, err)
	}
}
