package platform

import (
	"context"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
)

func TestFinishDistinguishesLocalStopFromTenantCancellation(t *testing.T) {
	for _, tenantCancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "local_stop", true: "tenant_cancel"}[tenantCancel], func(t *testing.T) {
			store := testStore(t)
			project, _ := testProject(t, store)
			ctx := context.Background()
			job, _, err := store.Submit(ctx, testSubmission(project, "", "inspect"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.AdmitOne(ctx); err != nil {
				t.Fatal(err)
			}
			claim, err := store.Claim(ctx, project.ID, "worker-a")
			if err != nil || claim.ID != job.ID {
				t.Fatalf("claim: %+v %v", claim, err)
			}
			if tenantCancel {
				state, err := store.Cancel(ctx, project.TenantID, project.ID, job.ID)
				if err != nil || state != Running {
					t.Fatalf("cancel request: %s %v", state, err)
				}
			}
			if err := store.Finish(ctx, project.ID, job.ID, "worker-a", claim.Generation,
				agent.Result{Backend: "api", Status: agent.StatusCanceled}); err != nil {
				t.Fatal(err)
			}
			observed, err := store.GetJob(ctx, project.TenantID, project.ID, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := RecoveryRequired
			if tenantCancel {
				want = Canceled
			}
			if observed.State != want {
				t.Fatalf("state after worker cancellation = %s, want %s", observed.State, want)
			}
			events, err := store.Events(ctx, project.TenantID, project.ID, job.ID, 0, 20)
			if err != nil || len(events) == 0 || events[len(events)-1].Kind != string(want) {
				t.Fatalf("events: %+v %v", events, err)
			}
		})
	}
}
