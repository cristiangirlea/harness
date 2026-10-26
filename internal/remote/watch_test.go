package remote

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
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

func TestAWatchStartedAfterTheJobEndedPrintsWhatRemainsAndReturns(t *testing.T) {
	const job = "j_cccccccccccccccccccccccccccccccc"
	events := []string{`{"id":1,"kind":"running"}`, `{"id":2,"kind":"checkpoint"}`, `{"id":3,"kind":"succeeded"}`}
	for _, test := range []struct {
		after int64
		want  string
	}{
		{3, ""},
		{9, ""},
		{1, events[1] + "\n" + events[2] + "\n"},
	} {
		t.Run(fmt.Sprint(test.after), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("tenant_id") != "t_1" {
					t.Errorf("request %s lacks the scope", r.URL)
				}
				switch r.URL.Path {
				case "/v1/jobs/" + job:
					fmt.Fprintf(w, `{"id":%q,"state":"canceled"}`, job)
				case "/v1/jobs/" + job + "/events":
					if r.Header.Get("Accept") == "text/event-stream" {
						t.Error("a watch of an ended job opened a stream")
						w.WriteHeader(http.StatusForbidden)
						return
					}
					after, _ := strconv.Atoi(r.URL.Query().Get("after"))
					// Pages of one event make the client ask again until a page is empty.
					if after < len(events) {
						fmt.Fprintf(w, `{"events":[%s]}`, events[after])
						return
					}
					fmt.Fprint(w, `{"events":[]}`)
				default:
					t.Errorf("unexpected request %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client := &Client{origin: server.URL, http: server.Client()}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var output bytes.Buffer
			err := client.WatchJob(ctx, job, url.Values{"tenant_id": {"t_1"}}, test.after, &output)
			if err != nil || ctx.Err() != nil {
				t.Fatalf("WatchJob error = %v, context = %v; want completion before deadline", err, ctx.Err())
			}
			if output.String() != test.want {
				t.Fatalf("output = %q, want %q", output.String(), test.want)
			}
		})
	}
}

func TestAWatchOfAJobStillRunningStreamsUntilTheTerminalEvent(t *testing.T) {
	const job = "j_cccccccccccccccccccccccccccccccc"
	var streams atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/jobs/" + job:
			fmt.Fprint(w, `{"state":"running"}`)
		case "/v1/jobs/" + job + "/events":
			streams.Add(1)
			if r.Header.Get("Last-Event-ID") != "1" || r.URL.Query().Get("tenant_id") != "t_1" {
				t.Errorf("stream request %s, cursor %q", r.URL, r.Header.Get("Last-Event-ID"))
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "id: 2\nevent: succeeded\ndata: {\"id\":2}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		default:
			t.Errorf("unexpected request %s", r.URL)
		}
	}))
	defer server.Close()
	client := &Client{origin: server.URL, http: server.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var output bytes.Buffer
	if err := client.WatchJob(ctx, job, url.Values{"tenant_id": {"t_1"}}, 1, &output); err != nil || ctx.Err() != nil {
		t.Fatalf("WatchJob error = %v, context = %v", err, ctx.Err())
	}
	if output.String() != "{\"id\":2}\n" || streams.Load() != 1 {
		t.Fatalf("output = %q, streams = %d", output.String(), streams.Load())
	}
}

func TestAWatchStreamsWhenTheJobStateIsBrieflyUnavailable(t *testing.T) {
	const job = "j_cccccccccccccccccccccccccccccccc"
	for _, test := range []struct {
		status  int
		streams int32
	}{{http.StatusServiceUnavailable, 1}, {http.StatusForbidden, 0}} {
		t.Run(strconv.Itoa(test.status), func(t *testing.T) {
			var streams atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/jobs/"+job {
					w.WriteHeader(test.status)
					return
				}
				streams.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "id: 2\nevent: succeeded\ndata: {\"id\":2}\n\n")
			}))
			defer server.Close()
			client := &Client{origin: server.URL, http: server.Client()}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			err := client.WatchJob(ctx, job, url.Values{}, 1, io.Discard)
			var httpErr *HTTPError
			if test.status >= 500 && err != nil || test.status < 500 && (!errors.As(err, &httpErr) || httpErr.Status != test.status) {
				t.Fatalf("WatchJob error = %v", err)
			}
			if ctx.Err() != nil || streams.Load() != test.streams {
				t.Fatalf("streams = %d, want %d; context = %v", streams.Load(), test.streams, ctx.Err())
			}
		})
	}
}

func TestAWatchOfAnEndedJobRefusesAPageThatDoesNotAdvanceTheCursor(t *testing.T) {
	const job = "j_cccccccccccccccccccccccccccccccc"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/jobs/"+job {
			fmt.Fprint(w, `{"state":"failed"}`)
			return
		}
		fmt.Fprint(w, `{"events":[{"id":2}]}`)
	}))
	defer server.Close()
	client := &Client{origin: server.URL, http: server.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.WatchJob(ctx, job, url.Values{}, 2, io.Discard); !errors.Is(err, ErrMalformedEvent) || ctx.Err() != nil {
		t.Fatalf("WatchJob error = %v, context = %v; want ErrMalformedEvent", err, ctx.Err())
	}
}

func TestWatchReturnsOnARedirectWithoutFollowingIt(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer server.Close()
	t.Setenv("HRN_SERVER_URL", server.URL)
	t.Setenv("HRN_TENANT_TOKEN", strings.Repeat("t", 64))
	t.Setenv("HRN_SERVER_CA_FILE", "")
	client, err := NewFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err = client.Watch(ctx, "/events", 0, io.Discard)
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusFound || requests.Load() != 1 || ctx.Err() != nil {
		t.Fatalf("Watch error = %v, requests = %d, context = %v; want one HTTP 302", err, requests.Load(), ctx.Err())
	}
}

func TestWatchReturnsOnAMalformedEventWithoutReconnecting(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "id: 1\nevent: running\ndata: not json\n\n")
	}))
	defer server.Close()
	client := &Client{origin: server.URL, http: server.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := client.Watch(ctx, "/events", 0, io.Discard)
	if !errors.Is(err, ErrMalformedEvent) || requests.Load() != 1 || ctx.Err() != nil {
		t.Fatalf("Watch error = %v, requests = %d, context = %v; want one ErrMalformedEvent", err, requests.Load(), ctx.Err())
	}
}

func TestReadEventsRefusesALineOver1MiB(t *testing.T) {
	stream := "id: 1\ndata: \"" + strings.Repeat("x", 1<<20) + "\"\n\n"
	if _, err := ReadEvents(strings.NewReader(stream), 0, io.Discard); !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("ReadEvents error = %v, want bufio.ErrTooLong", err)
	}
}

func TestWatchJobRefusesAJobIDThatIsNotServerIssuedWithoutARequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	client := &Client{origin: server.URL, http: server.Client()}
	for _, job := range []string{"", "j_../x", "j_" + strings.Repeat("A", 32), "p_" + strings.Repeat("a", 32), "j_" + strings.Repeat("a", 32) + "/events"} {
		if err := client.WatchJob(context.Background(), job, url.Values{}, 0, io.Discard); err == nil || !strings.Contains(err.Error(), "server-issued") {
			t.Errorf("WatchJob(%q) error = %v", job, err)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("an invalid job ID reached the server in %d requests", requests.Load())
	}
}

func TestWatchJobRefusesAJobWhoseStateIsNotAString(t *testing.T) {
	const job = "j_cccccccccccccccccccccccccccccccc"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/jobs/"+job {
			t.Errorf("unexpected request %s", r.URL)
		}
		fmt.Fprint(w, `{"state":5}`)
	}))
	defer server.Close()
	client := &Client{origin: server.URL, http: server.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.WatchJob(ctx, job, url.Values{}, 0, io.Discard); err == nil || err.Error() != "invalid job from control service" {
		t.Fatalf("WatchJob error = %v", err)
	}
}

func TestReadEventsRefusesAnEventWhoseJoinedDataExceeds1MiB(t *testing.T) {
	line := "data: " + strings.Repeat(" ", 1<<10) + "\n"
	for _, stream := range []string{
		"id: 1\ndata: \"\"\n" + strings.Repeat(line, 1<<10) + "\n",
		"id: 1\ndata:\"\"\n" + strings.Repeat("data:\n", (1<<20)+1) + "\n",
	} {
		if _, err := ReadEvents(strings.NewReader(stream), 0, io.Discard); !errors.Is(err, ErrMalformedEvent) ||
			!strings.Contains(err.Error(), "1 MiB") {
			t.Fatalf("ReadEvents error = %v, want ErrMalformedEvent naming the 1 MiB limit", err)
		}
	}
}

func TestReadEventsEscapesC1ControlCharactersBeforePrinting(t *testing.T) {
	stream := "id: 1\ndata: {\"text\":\"a\u009b31mb\u0085c\u00a0d\x9b\ufffd\"}\n\n"
	var output bytes.Buffer
	if _, err := ReadEvents(strings.NewReader(stream), 0, &output); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "{\"text\":\"a\\u009b31mb\\u0085c\u00a0d\\ufffd\ufffd\"}\n"; got != want {
		t.Fatalf("printed %q, want %q", got, want)
	}
}

func TestReadEventsAcceptsFieldsWithoutTheSpaceAfterTheColon(t *testing.T) {
	stream := "id:1\nevent:succeeded\ndata:{\"id\":1,\ndata:\"kind\":\"succeeded\"}\n\nid:2\ndata:{\"id\":2}\n\n"
	var output bytes.Buffer
	after, terminal, err := readEvents(strings.NewReader(stream), 0, &output, true)
	if err != nil || !terminal || after != 1 {
		t.Fatalf("readEvents = after %d, terminal %v, err %v; want 1, true, nil", after, terminal, err)
	}
	if got, want := output.String(), "{\"id\":1,\n\"kind\":\"succeeded\"}\n"; got != want {
		t.Fatalf("printed %q, want %q", got, want)
	}
}
