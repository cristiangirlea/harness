package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWatchPrintsTerminalEventAndReturnsWithoutReconnecting(t *testing.T) {
	for _, event := range []struct{ kind, details string }{
		{"succeeded", "null"},
		{"failed", "null"},
		{"canceled", "null"},
		{"admission_timeout", `{"state":"failed"}`},
		{"recovery_resolved", `{"state":"succeeded"}`},
		{"recovery_resolved", `{"state":"failed"}`},
		{"recovery_resolved", `{"state":"canceled"}`},
	} {
		for _, ending := range []string{"open", "closed", "EOF without separator"} {
			t.Run(event.kind+"/"+event.details+"/"+ending, func(t *testing.T) {
				t.Parallel()
				var requests atomic.Int32
				data := fmt.Sprintf(`{"id":3,"kind":%q,"details":%s}`, event.kind, event.details)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if requests.Add(1) > 1 {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "id: 1\nevent: running\ndata: {\"id\":1}\n\n"+
						"id: 2\nevent: checkpoint\ndata: {\"id\":2}\n\n")
					fmt.Fprintf(w, "id: 3\nevent: %s\ndata: %s\n", event.kind, data)
					if ending != "EOF without separator" {
						fmt.Fprint(w, "\n")
					}
					if ending == "open" {
						w.(http.Flusher).Flush()
						<-r.Context().Done()
					}
				}))
				defer server.Close()
				client := &Client{origin: server.URL, http: server.Client()}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				var output bytes.Buffer
				err := client.Watch(ctx, "/events", 0, &output)
				if err != nil || ctx.Err() != nil {
					t.Errorf("Watch error = %v, context = %v; want completion before deadline", err, ctx.Err())
				}
				if got := requests.Load(); got != 1 {
					t.Errorf("requests = %d, want 1", got)
				}
				if want := "{\"id\":1}\n{\"id\":2}\n" + data + "\n"; output.String() != want {
					t.Errorf("output = %q, want %q", output.String(), want)
				}
			})
		}
	}
}

func TestWatchReconnectsAfterCleanEndWithoutTerminalEvent(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) > 1 {
			if got := r.Header.Get("Last-Event-ID"); got != "5" {
				t.Errorf("reconnect cursor = %q, want 5", got)
			}
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i, kind := range []string{"cancel_requested", "recovery_required", "admission_timeout", "recovery_resolved", "unknown"} {
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: {\"details\":{\"state\":\"queued\"}}\n\n", i+1, kind)
		}
	}))
	defer server.Close()
	client := &Client{origin: server.URL, http: server.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := client.Watch(ctx, "/events", 0, io.Discard)
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusForbidden || requests.Load() != 2 {
		t.Fatalf("Watch error = %v, requests = %d; want second request's HTTP 403", err, requests.Load())
	}
}

func TestReadEventsReportsTerminalEventOutputFailure(t *testing.T) {
	_, err := ReadEvents(strings.NewReader("id: 1\nevent: succeeded\ndata: {}\n\n"), 0, brokenWriter{})
	if !errors.Is(err, ErrEventOutput) || !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("terminal output error = %v", err)
	}
}

func TestAWrongShapedTerminalDetailIsNotTerminalAndALaterTerminalEventStillEndsTheWatch(t *testing.T) {
	stream := "id: 1\nevent: admission_timeout\ndata: {\"id\":1,\"kind\":\"admission_timeout\",\"details\":{\"state\":5}}\n\n" +
		"id: 2\nevent: recovery_resolved\ndata: {\"id\":2,\"kind\":\"recovery_resolved\",\"details\":\"failed\"}\n\n" +
		"id: 3\nevent: succeeded\ndata: {\"id\":3,\"kind\":\"succeeded\"}\n\n" +
		"id: 4\nevent: checkpoint\ndata: {\"id\":4}\n\n"
	var output bytes.Buffer
	after, terminal, err := readEvents(strings.NewReader(stream), 0, &output, true)
	if err != nil || !terminal || after != 3 {
		t.Fatalf("readEvents = after %d, terminal %v, err %v; want 3, true, nil", after, terminal, err)
	}
}

func TestAnEventWhoseDataIsNotJSONIsMalformed(t *testing.T) {
	stream := "id: 1\nevent: succeeded\ndata: not json\n\n"
	if _, _, err := readEvents(strings.NewReader(stream), 0, io.Discard, true); !errors.Is(err, ErrMalformedEvent) {
		t.Fatalf("readEvents error = %v, want ErrMalformedEvent", err)
	}
}

func TestReadEventsStillReadsPastATerminalEvent(t *testing.T) {
	stream := "id: 1\nevent: succeeded\ndata: {\"id\":1,\"kind\":\"succeeded\"}\n\n" +
		"id: 2\nevent: checkpoint\ndata: {\"id\":2}\n\n"
	after, err := ReadEvents(strings.NewReader(stream), 0, io.Discard)
	if err != nil || after != 2 {
		t.Fatalf("ReadEvents = after %d, err %v; want 2, nil", after, err)
	}
}
