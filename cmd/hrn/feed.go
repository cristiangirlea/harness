package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/prefs"
	"github.com/cristiangirlea/harness/internal/runlog"
)

type feedConfig struct {
	task, model         string
	parallel, maxTokens int
	timeout             time.Duration
}

type feedResult struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	Output       string `json:"output"`
	RunID        string `json:"run_id"`
	DurationMS   int64  `json:"duration_ms"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	Error        string `json:"error,omitempty"`
}

func cmdFeed(args []string) (int, error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	return cmdFeedContext(ctx, args, os.Stdin, os.Stdout, os.Stderr)
}

func cmdFeedContext(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("feed", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var c feedConfig
	backend := fs.String("b", "", "required backend")
	fs.StringVar(&c.model, "model", "", "model id")
	fs.StringVar(&c.task, "task", "", "instruction for each item")
	taskFile := fs.String("task-file", "", "instruction file")
	in := fs.String("in", "-", "JSONL input path, or - for stdin")
	out := fs.String("out", "", "JSONL output path")
	fs.IntVar(&c.parallel, "parallel", 4, "concurrent items (1..16)")
	fs.DurationVar(&c.timeout, "timeout", 2*time.Minute, "timeout per item")
	fs.IntVar(&c.maxTokens, "max-tokens", 0, "output token limit (api only)")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if fs.NArg() != 0 || *backend == "" || set["task"] == set["task-file"] {
		return 2, errors.New("feed requires -b BACKEND and exactly one of --task or --task-file, with no positional arguments")
	}
	if c.parallel < 1 || c.parallel > 16 || c.timeout <= 0 || c.maxTokens < 0 {
		return 2, errors.New("feed requires --parallel 1..16, a positive --timeout and nonnegative --max-tokens")
	}
	if set["max-tokens"] && (c.maxTokens == 0 || *backend != "api") {
		return 2, errors.New("feed --max-tokens requires a positive value and the api backend")
	}
	b, err := feedBackend(*backend)
	if err != nil {
		return 2, err
	}
	if set["task-file"] {
		data, err := os.ReadFile(*taskFile)
		if err != nil {
			return 2, err
		}
		c.task = string(data)
	}
	if strings.TrimSpace(c.task) == "" {
		return 2, errors.New("feed instruction is empty")
	}
	input := stdin
	if *in != "" && *in != "-" {
		f, err := os.Open(*in)
		if err != nil {
			return 1, err
		}
		defer f.Close()
		input = f
	}
	if *out != "" {
		if f, ok := input.(*os.File); ok {
			a, err := f.Stat()
			if err != nil {
				return 1, err
			}
			if b, err := os.Stat(*out); err == nil && os.SameFile(a, b) {
				return 2, errors.New("feed input and output must be different files")
			}
		}
		f, err := os.OpenFile(*out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return 1, err
		}
		defer f.Close()
		stdout = f
	}
	// Closing the command's input releases a reader waiting for another line on cancellation.
	if f, ok := input.(*os.File); ok {
		defer f.Close()
		stop := context.AfterFunc(ctx, func() { _ = f.Close() })
		defer stop()
	}
	if err := runFeed(ctx, b, c, input, stdout, stderr); err != nil {
		return 1, err
	}
	return 0, nil
}

func feedBackend(name string) (agent.Backend, error) {
	p, err := prefs.Load(runlog.Home())
	if err != nil {
		return nil, err
	}
	if err := p.Check(name); err != nil {
		return nil, err
	}
	b, err := agent.Get(name)
	if err != nil {
		return nil, err
	}
	if name != "api" {
		return b, nil
	}
	base := os.Getenv("ANTHROPIC_BASE_URL")
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1") {
		return nil, errors.New("feed refuses api unless ANTHROPIC_BASE_URL is a loopback http URL (127.0.0.1, localhost or ::1), to avoid accidental per-token billing")
	}
	if api, ok := b.(*agent.APIBackend); ok {
		copy := *api
		copy.Tools, copy.BaseURL, copy.APIKey = nil, base, "local-feed"
		// Local inference must neither follow a redirect nor use a proxy or billing credentials.
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		copy.HTTPClient = &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		return &copy, nil
	}
	return b, nil
}

type feedItem struct{ id, text, invalid string }

func readFeedItem(r *bufio.Reader, line int) (feedItem, error) {
	item := feedItem{id: strconv.Itoa(line)}
	var data []byte
	tooLong := false
	for {
		part, err := r.ReadSlice('\n')
		if !tooLong {
			data = append(data, part...)
			tooLong = len(bytes.TrimSuffix(data, []byte{'\n'})) > 64<<10
			if tooLong {
				data = nil
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return item, err
		}
		if len(data) == 0 && !tooLong && errors.Is(err, io.EOF) {
			return item, io.EOF
		}
		break
	}
	if tooLong {
		item.invalid = "line exceeds 64 KiB"
		return item, nil
	}
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '"' {
		if err := json.Unmarshal(data, &item.text); err != nil {
			item.invalid = "invalid JSON string"
		}
		return item, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil || obj == nil {
		item.invalid = "expected a JSON object or string"
		return item, nil
	}
	if id, ok := obj["id"]; ok {
		if len(id) == 0 || id[0] != '"' || json.Unmarshal(id, &item.id) != nil {
			item.invalid = "id must be a string"
			return item, nil
		}
	}
	text, ok := obj["text"]
	if !ok || len(text) == 0 || text[0] != '"' || json.Unmarshal(text, &item.text) != nil {
		item.invalid = "text must be a string"
	}
	return item, nil
}

func runFeed(parent context.Context, b agent.Backend, c feedConfig, input io.Reader, output, stderr io.Writer) error {
	started := time.Now()
	dir, err := os.MkdirTemp("", "hrn-feed-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	ctx, stop := context.WithCancel(parent)
	defer stop()
	jobs := make(chan feedItem)
	results := make(chan feedResult)
	readError := make(chan error, 1)
	go func() {
		defer close(jobs)
		r := bufio.NewReader(input)
		for line := 1; ; line++ {
			if ctx.Err() != nil {
				return
			}
			item, err := readFeedItem(r, line)
			if err != nil {
				if !errors.Is(err, io.EOF) {
					readError <- err
				}
				return
			}
			select {
			case jobs <- item:
			case <-ctx.Done():
				return
			}
		}
	}()
	var workers sync.WaitGroup
	for i := 0; i < c.parallel; i++ {
		workers.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case item, ok := <-jobs:
					if !ok || ctx.Err() != nil {
						return
					}
					results <- runFeedItem(context.WithoutCancel(parent), b, c, dir, item)
				}
			}
		})
	}
	go func() { workers.Wait(); close(results) }()
	w := bufio.NewWriter(output)
	enc := json.NewEncoder(w)
	items, ok := 0, 0
	var writeErr error
	for result := range results {
		items++
		if result.Status == "ok" {
			ok++
		}
		if writeErr == nil {
			writeErr = enc.Encode(result)
			if writeErr == nil {
				writeErr = w.Flush()
			}
			if writeErr != nil {
				stop()
			}
		}
	}
	elapsed := time.Since(started)
	_, summaryErr := fmt.Fprintf(stderr, "feed: items=%d ok=%d failed=%d wall=%s items/s=%.2f\n", items, ok, items-ok, elapsed.Round(time.Millisecond), float64(items)/elapsed.Seconds())
	select {
	case err = <-readError:
	default:
	}
	return errors.Join(writeErr, err, summaryErr, parent.Err())
}

func runFeedItem(ctx context.Context, b agent.Backend, c feedConfig, dir string, item feedItem) feedResult {
	out := feedResult{ID: item.id, Status: "invalid", Error: item.invalid}
	if item.invalid != "" {
		return out
	}
	started := time.Now()
	task := agent.Task{Dir: dir, Purpose: "none", MaxTokens: c.maxTokens,
		Prompt:  c.task + "\n\n<item>\n" + html.EscapeString(item.text) + "\n</item>",
		Options: agent.Options{Model: c.model, Mode: agent.ModeReadOnly, MaxTurns: 1, NoInstructions: true, NoSwitch: true, Lean: agent.Bool(true), Timeout: agent.Duration(c.timeout)}}
	rec, err := runlog.Create(b.Name(), task, agent.ResolveSetup(b.Name(), task))
	if err != nil {
		out.Status, out.Error = "error", err.Error()
		return out
	}
	out.RunID = rec.ID()
	task.RunDir = rec.Dir()
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	res := agent.Run(ctx, b, task, rec.Event)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		res.Status, res.Error = agent.StatusTimeout, context.DeadlineExceeded.Error()
	}
	res.Duration = time.Since(started)
	out.Status = "error"
	if res.Status == agent.StatusOK {
		out.Status = "ok"
	} else if res.Status == agent.StatusTimeout {
		out.Status = "timeout"
	}
	out.Output, out.Error = res.Final, res.Error
	if out.Status != "ok" && out.Error == "" {
		out.Error = "backend status: " + string(res.Status)
	}
	if err := rec.Close(res); err != nil {
		out.Status, out.Error = "error", errors.Join(errors.New(out.Error), err).Error()
	}
	out.DurationMS, out.InputTokens, out.OutputTokens = res.Duration.Milliseconds(), res.InputTokens, res.OutputTokens
	return out
}
