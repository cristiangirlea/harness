package platform

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func eventJob(t *testing.T, store *Store) (Job, string) {
	t.Helper()
	ctx := context.Background()
	name, err := newID("events_")
	if err != nil {
		t.Fatal(err)
	}
	tenant, token, err := store.CreateTenant(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	project, _, err := store.CreateProject(ctx, Project{TenantID: tenant, Name: "events",
		RepositoryURL: "https://example.invalid/example.git", BaseRef: fixtureRef, Backend: "api"})
	if err != nil {
		t.Fatal(err)
	}
	job, _, err := store.Submit(ctx, testSubmission(project, "", "stream events"))
	if err != nil {
		t.Fatal(err)
	}
	return job, token
}

func eventURL(server *httptest.Server, job Job) string {
	return fmt.Sprintf("%s/v1/jobs/%s/events?tenant_id=%s&project_id=%s", server.URL, job.ID, job.TenantID, job.ProjectID)
}

func openEventStream(t *testing.T, server *httptest.Server, job Job, token, cursor string) *http.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, eventURL(server, job), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Last-Event-ID", cursor)
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { response.Body.Close() })
	return response
}

func readEventStream(t *testing.T, response *http.Response) []Event {
	t.Helper()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream response: %d %v", response.StatusCode, response.Header)
	}
	var events []Event
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		if data, ok := strings.CutPrefix(scanner.Text(), "data: "); ok {
			var event Event
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				t.Fatal(err)
			}
			events = append(events, event)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("stream did not close cleanly: %v", err)
	}
	return events
}

func waitForEventStreams(t *testing.T, h *HTTPServer, tenant string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		h.eventStreams.mu.Lock()
		got := h.eventStreams.active[tenant]
		h.eventStreams.mu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("active streams = %d, want %d", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestHTTPEventStreamsCapTenantsAndReleaseDisconnectedStreams(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	store := testStore(t)
	job, token := eventJob(t, store)
	other, otherToken := eventJob(t, store)
	h := &HTTPServer{Store: store}
	server := httptest.NewServer(h.Handler())
	t.Cleanup(server.Close)
	var streams []*http.Response
	for i := 0; i < 4; i++ {
		response := openEventStream(t, server, job, token, "")
		if response.StatusCode != http.StatusOK {
			t.Fatalf("stream %d: %d", i, response.StatusCode)
		}
		streams = append(streams, response)
	}
	// A different job under the same tenant still shares the cap.
	sibling, _, err := store.Submit(context.Background(), Submission{TenantID: job.TenantID,
		ProjectID: job.ProjectID, Prompt: "another job", Options: job.Options, Backend: job.Backend, BaseRef: job.BaseRef})
	if err != nil {
		t.Fatal(err)
	}
	refused := openEventStream(t, server, sibling, token, "")
	body, err := io.ReadAll(refused.Body)
	if err != nil || refused.StatusCode != http.StatusTooManyRequests ||
		refused.Header.Get("Content-Type") != "application/json" || !strings.Contains(string(body), `"code":"stream_limit"`) {
		t.Fatalf("cap refusal: %d %s %v", refused.StatusCode, body, err)
	}
	if response := openEventStream(t, server, other, otherToken, ""); response.StatusCode != http.StatusOK {
		t.Fatalf("other tenant refused: %d", response.StatusCode)
	}
	status, _ := sendJSON(t, server.Client(), http.MethodGet, eventURL(server, job), token, nil)
	if status != http.StatusOK {
		t.Fatalf("JSON events counted against SSE cap: %d", status)
	}
	streams[0].Body.Close()
	waitForEventStreams(t, h, job.TenantID, 3)
	if response := openEventStream(t, server, sibling, token, ""); response.StatusCode != http.StatusOK {
		t.Fatalf("released slot refused: %d", response.StatusCode)
	}
}

// Match the store's atomic state/event commit without executing a model.
func finishEventJob(t *testing.T, store *Store, job Job, state State, backlog int) {
	t.Helper()
	ctx := context.Background()
	tx, err := store.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for i := 0; i < backlog; i++ {
		if err := appendEvent(ctx, tx, job.TenantID, job.ProjectID, job.ID, "checkpoint", nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(ctx, "UPDATE jobs SET state=$2 WHERE id=$1", job.ID, state); err != nil {
		t.Fatal(err)
	}
	if err := appendEvent(ctx, tx, job.TenantID, job.ProjectID, job.ID, string(state), nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPEventStreamsDrainTerminalBacklogsAndClose(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	store := testStore(t)
	h := &HTTPServer{Store: store}
	server := httptest.NewServer(h.Handler())
	t.Cleanup(server.Close)
	for _, state := range []State{Succeeded, Failed, Canceled} {
		t.Run(string(state), func(t *testing.T) {
			job, token := eventJob(t, store)
			finishEventJob(t, store, job, state, 105)
			want, err := store.Events(context.Background(), job.TenantID, job.ProjectID, job.ID, 0, 1000)
			if err != nil {
				t.Fatal(err)
			}
			got := readEventStream(t, openEventStream(t, server, job, token, ""))
			if len(got) != len(want) || got[len(got)-1].Kind != string(state) {
				t.Fatalf("terminal backlog: got %d events, want %d ending in %s", len(got), len(want), state)
			}
			for i := range want {
				if got[i].ID != want[i].ID {
					t.Fatalf("event %d: id %d, want %d", i, got[i].ID, want[i].ID)
				}
			}
			cursor := fmt.Sprint(got[len(got)-1].ID)
			if remaining := readEventStream(t, openEventStream(t, server, job, token, cursor)); len(remaining) != 0 {
				t.Fatalf("replayed events after terminal cursor: %v", remaining)
			}
			waitForEventStreams(t, h, job.TenantID, 0)
		})
	}
}

func TestHTTPEventStreamsSendLiveTerminalEventsBeforeClosing(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	store := testStore(t)
	h := &HTTPServer{Store: store}
	server := httptest.NewServer(h.Handler())
	t.Cleanup(server.Close)
	for _, state := range []State{Succeeded, Failed, Canceled} {
		t.Run(string(state), func(t *testing.T) {
			job, token := eventJob(t, store)
			response := openEventStream(t, server, job, token, "")
			finishEventJob(t, store, job, state, 0)
			events := readEventStream(t, response)
			if len(events) != 2 || events[len(events)-1].Kind != string(state) {
				t.Fatalf("live events: %+v", events)
			}
			waitForEventStreams(t, h, job.TenantID, 0)
		})
	}
}

func TestHTTPEventStreamsExpireAndReconnectWithTheirCursor(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	store := testStore(t)
	job, token := eventJob(t, store)
	h := &HTTPServer{Store: store, eventStreamDuration: 250 * time.Millisecond}
	server := httptest.NewServer(h.Handler())
	t.Cleanup(server.Close)
	events := readEventStream(t, openEventStream(t, server, job, token, ""))
	if len(events) != 1 {
		t.Fatalf("initial events: %+v", events)
	}
	waitForEventStreams(t, h, job.TenantID, 0)
	finishEventJob(t, store, job, Canceled, 0)
	resumed := readEventStream(t, openEventStream(t, server, job, token, fmt.Sprint(events[0].ID)))
	if len(resumed) != 1 || resumed[0].Kind != string(Canceled) || resumed[0].ID <= events[0].ID {
		t.Fatalf("resumed events: %+v", resumed)
	}
	waitForEventStreams(t, h, job.TenantID, 0)
}
