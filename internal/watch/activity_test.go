package watch

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/runlog"
)

func ageFile(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func activityRun(t *testing.T, at time.Time, done bool) runlog.Record {
	t.Helper()
	rec := runlog.Record{ID: runlog.NewID(), Backend: "claude", Started: time.Now().Add(-3 * time.Hour),
		Task: agent.Task{Prompt: "Fix the test", Dir: "/repo/harness"}, Done: done}
	dir := filepath.Join(runlog.Root(), rec.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "task.json"
	if done {
		name = "result.json"
		rec.Result = agent.Result{Status: agent.StatusOK, Duration: time.Minute}
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{name: data, "events.jsonl": nil} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		ageFile(t, path, at)
	}
	ageFile(t, dir, at)
	return rec
}

func TestAQuietUnfinishedRunShowsStoppedOnEverySurface(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	quiet := time.Now().Add(-61 * time.Minute)
	rec := activityRun(t, quiet, false)
	want := "no word since " + quiet.Local().Format("15:04")
	r := runRow(rec)
	if r.Status != "stopped" || r.Since != "" || r.Took != want {
		t.Errorf("row = %+v, want stopped with %q and no running clock", r, want)
	}
	h := New(context.Background(), Options{})
	for _, path := range []string{"/", "/run/" + rec.ID} {
		body := viewAs(t, h, path, "127.0.0.1:1234")
		if !strings.Contains(body, "stopped") || !strings.Contains(body, want) || strings.Contains(body, "Running. New steps") || strings.Contains(body, "Running for") {
			t.Errorf("%s does not show stopped with %q", path, want)
		}
		if path != "/" && strings.Contains(body, "data-stream=") {
			t.Error("a stopped run's page still opens a live stream")
		}
	}
	card := purposesOf([]runlog.Record{rec})
	if card.Rows[0].Running != "0" || card.Rows[0].NoCost != "" {
		t.Errorf("stopped run counted as running or a finished cost: %+v", card.Rows[0])
	}
	assertStoppedBot(t, []runlog.Record{rec}, nil)
	loaded, err := runlog.Load(rec.ID)
	if err != nil || loaded.Done {
		t.Fatalf("watch changed the unfinished record: %+v, %v", loaded, err)
	}
}

func TestQuietStreamsShowStoppedWithoutReportingFinished(t *testing.T) {
	for _, kind := range []string{"run", "batch"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			quiet := time.Now().Add(-61 * time.Minute)
			rec := activityRun(t, quiet, false)
			id := rec.ID
			if kind == "batch" {
				id = runlog.NewID()
				saveBatch(t, batch.Result{ID: id, Started: rec.Started, Tasks: []batch.TaskResult{
					{ID: "first", Status: batch.StatusRunning, Attempts: []batch.Attempt{{RunID: rec.ID}}},
				}})
				ageFile(t, filepath.Join(runlog.Home(), "batches", id+".json"), quiet)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			req := httptest.NewRequest("GET", "http://127.0.0.1:8089/stream/"+kind+"/"+id, nil).WithContext(ctx)
			out := httptest.NewRecorder()
			New(context.Background(), Options{Poll: time.Millisecond}).ServeHTTP(out, req)
			body := out.Body.String()
			if !strings.Contains(body, "stopped") || !strings.Contains(body, "no word since ") || !strings.Contains(body, "event: end\ndata: stopped\n") || strings.Contains(body, "Finished") {
				t.Errorf("quiet %s stream = %s", kind, body)
			}
		})
	}
}

func assertStoppedBot(t *testing.T, runs []runlog.Record, batches []batch.Result) {
	t.Helper()
	cur := botLook(runs, batches, nil, time.Now())
	if got := statusOf(cur); got != "Nothing is running." {
		t.Errorf("Ada: %q", got)
	}
	for _, first := range []bool{true, false} {
		for _, s := range announce(botSnapshot{}, cur, first) {
			if s.Kind != "status" {
				t.Errorf("Ada announced stopped history: %+v", s)
			}
		}
	}
	prev := botSnapshot{items: append([]botItem(nil), cur.items...)}
	for i := range prev.items {
		prev.items[i].Going = true
	}
	for _, s := range announce(prev, cur, false) {
		if s.Kind != "status" {
			t.Errorf("Ada announced a quiet record as finished: %+v", s)
		}
	}
}

func TestABatchUsesItsRecordAndEveryRunsLatestWrite(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		recordFresh, runFresh, ended bool
	}{
		{name: "quiet first attempt"},
		{name: "long first attempt still writes", runFresh: true},
		{name: "batch still writes", recordFresh: true},
		{name: "finished batch stays finished", ended: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HRN_HOME", t.TempDir())
			quiet := time.Now().Add(-61 * time.Minute)
			runTime, recordTime := quiet, quiet.Add(-time.Minute)
			if tc.runFresh {
				runTime = time.Now()
			}
			if tc.recordFresh {
				recordTime = time.Now()
			}
			rec := activityRun(t, runTime, tc.ended)
			b := batch.Result{ID: runlog.NewID(), Started: rec.Started, Tasks: []batch.TaskResult{
				{ID: "first", Status: batch.StatusRunning, Attempts: []batch.Attempt{{RunID: rec.ID, Backend: rec.Backend}}},
				{ID: "queued", Status: batch.StatusRunning},
			}}
			if tc.ended {
				b.Duration = time.Hour
				for i := range b.Tasks {
					b.Tasks[i].Status = batch.StatusPass
				}
			}
			saveBatch(t, b)
			path := filepath.Join(runlog.Home(), "batches", b.ID+".json")
			ageFile(t, path, recordTime)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			want := "stopped"
			if tc.recordFresh || tc.runFresh {
				want = "running"
			} else if tc.ended {
				want = "done"
			}
			r := batchRow(b)
			if r.Status != want {
				t.Errorf("batch row status = %q, want %q", r.Status, want)
			}
			body := batchBodyOf(b, viewer{})
			if body.Status != want {
				t.Errorf("batch page status = %q, want %q", body.Status, want)
			}
			if want == "stopped" {
				if r.Took != "no word since "+quiet.Local().Format("15:04") || strings.Contains(r.Tasks, "running") {
					t.Errorf("stopped batch row = %+v", r)
				}
				for _, task := range body.Tasks {
					if task.Status != "stopped" || task.Waiting {
						t.Errorf("stopped task = %+v", task)
					}
					for _, a := range task.Attempts {
						if a.Status != "stopped" || a.Running {
							t.Errorf("stopped attempt = %+v", a)
						}
					}
				}
				h := New(context.Background(), Options{})
				for _, url := range []string{"/", "/batch/" + b.ID} {
					html := viewAs(t, h, url, "127.0.0.1:1234")
					if !strings.Contains(html, "no word since "+quiet.Local().Format("15:04")) || !strings.Contains(html, "stopped") {
						t.Errorf("%s does not show the stopped batch", url)
					}
				}
				assertStoppedBot(t, []runlog.Record{rec}, []batch.Result{b})
			} else if want == "running" {
				snap := botLook([]runlog.Record{rec}, []batch.Result{b}, nil, time.Now())
				if len(snap.items) != 1 || !snap.items[0].Going {
					t.Errorf("Ada lost the live batch: %+v", snap)
				}
			}
			after, err := os.ReadFile(path)
			if err != nil || string(before) != string(after) {
				t.Errorf("watch changed the batch record: %v", err)
			}
		})
	}
}
