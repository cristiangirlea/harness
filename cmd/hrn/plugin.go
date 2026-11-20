package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/kernel"
	"github.com/cristiangirlea/harness/internal/mask"
	"github.com/cristiangirlea/harness/internal/plugin"
	"github.com/cristiangirlea/harness/internal/pluginproto"
	"github.com/cristiangirlea/harness/internal/runlog"
)

const pluginUsage = `usage: hrn plugin list [--json]
       hrn plugin info <name> [--json]
       hrn plugin call <name> <capability> [--input JSON | --input-file FILE] [--timeout D] [--json]`

func cmdPlugin(args []string) (int, error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	code, err := cmdPluginContext(ctx, os.Stdin, os.Stdout, os.Stderr, args)
	if err != nil {
		return code, errors.New(printable(mask.Text(err.Error())))
	}
	return code, nil
}

func cmdPluginContext(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args []string) (int, error) {
	if len(args) == 0 {
		return 2, errors.New(pluginUsage)
	}
	command := args[0]
	var positional int
	switch command {
	case "list":
	case "info":
		positional = 1
	case "call":
		positional = 2
	default:
		return 2, fmt.Errorf("unknown plugin command %q\n%s", command, pluginUsage)
	}
	args = args[1:]
	if len(args) < positional {
		return 2, errors.New(pluginUsage)
	}
	for _, arg := range args[:positional] {
		if arg == "" || strings.HasPrefix(arg, "-") {
			return 2, errors.New(pluginUsage)
		}
	}
	fs := flag.NewFlagSet("plugin "+command, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "")
	var input, inputFile *string
	var timeout time.Duration
	if command == "call" {
		fs.Func("input", "", func(value string) error { input = &value; return nil })
		fs.Func("input-file", "", func(value string) error { inputFile = &value; return nil })
		fs.DurationVar(&timeout, "timeout", 10*time.Minute, "")
	}
	if err := fs.Parse(args[positional:]); err != nil {
		return 2, fmt.Errorf("%w\n%s", err, pluginUsage)
	}
	if fs.NArg() != 0 {
		return 2, fmt.Errorf("unexpected argument %q\n%s", fs.Arg(0), pluginUsage)
	}
	if command == "list" {
		if err := listCommandPlugins(stdout, *jsonOut); err != nil {
			return 1, err
		}
		return 0, nil
	}
	if timeout < 0 {
		return 2, fmt.Errorf("--timeout must be non-negative\n%s", pluginUsage)
	}
	raw, err := readCommandPluginInput(stdin, input, inputFile)
	if err != nil {
		return 2, fmt.Errorf("%w\n%s", err, pluginUsage)
	}
	found, err := plugin.Find(args[0])
	if err != nil {
		return 1, err
	}
	dir, err := os.Getwd()
	if err != nil {
		return 1, err
	}
	s, err := plugin.Start(ctx, found, plugin.Options{
		Host: pluginproto.Program{Name: "hrn", Version: agent.Version},
		Dir:  dir,
	})
	if err != nil {
		return 1, err
	}
	if command == "info" {
		info := s.Info()
		if err := s.Close(); err != nil {
			return 1, err
		}
		if err := printCommandPluginInfo(stdout, info, *jsonOut); err != nil {
			return 1, err
		}
		return 0, nil
	}
	return callCommandPlugin(ctx, stdout, stderr, s, found.Installed, dir, args[1], raw, timeout, *jsonOut)
}

func listCommandPlugins(stdout io.Writer, jsonOut bool) error {
	found, err := plugin.List()
	if err != nil {
		return err
	}
	if jsonOut {
		type row struct {
			Name      string `json:"name"`
			Path      string `json:"path"`
			Installed bool   `json:"installed"`
		}
		rows := make([]row, 0, len(found))
		for _, p := range found {
			rows = append(rows, row{Name: p.Name, Path: p.Path, Installed: p.Installed})
		}
		return json.NewEncoder(stdout).Encode(struct {
			Plugins []row `json:"plugins"`
		}{Plugins: rows})
	}
	if len(found) == 0 {
		_, err := fmt.Fprintln(stdout, "no plugins found (hrn plugin call looks in <$HRN_HOME>/plugins and on PATH)")
		return err
	}
	for _, p := range found {
		source := "path"
		if p.Installed {
			source = "installed"
		}
		if _, err := fmt.Fprintf(stdout, "%s  %s  %s\n", p.Name, source, printable(mask.Text(p.Path))); err != nil {
			return err
		}
	}
	return nil
}

func printCommandPluginInfo(stdout io.Writer, info plugin.Info, jsonOut bool) error {
	if jsonOut {
		return json.NewEncoder(stdout).Encode(struct {
			Name         string                   `json:"name"`
			Version      string                   `json:"version"`
			Protocol     string                   `json:"protocol"`
			Path         string                   `json:"path"`
			SHA256       string                   `json:"sha256"`
			Capabilities []pluginproto.Capability `json:"capabilities"`
		}{
			Name: info.Name, Version: info.Version, Protocol: info.Protocol,
			Path: info.Path, SHA256: info.SHA256, Capabilities: info.Capabilities,
		})
	}
	if _, err := fmt.Fprintf(stdout, "name: %s\nversion: %s\nprotocol: %s\npath: %s\nsha256: %s\n",
		info.Name, info.Version, info.Protocol, printable(mask.Text(info.Path)), info.SHA256); err != nil {
		return err
	}
	for _, capability := range info.Capabilities {
		line := capability.Name
		if capability.Description != "" {
			line += "  " + printable(mask.Text(capability.Description))
		}
		if _, err := fmt.Fprintln(stdout, line); err != nil {
			return err
		}
	}
	return nil
}

func readCommandPluginInput(stdin io.Reader, input, path *string) (json.RawMessage, error) {
	var raw []byte
	switch {
	case input != nil && path != nil:
		return nil, errors.New("use only one of --input and --input-file")
	case input != nil:
		raw = []byte(*input)
	case path != nil:
		reader := stdin
		if *path != "-" {
			f, err := os.Open(*path)
			if err != nil {
				return nil, fmt.Errorf("--input-file: %w", err)
			}
			defer f.Close()
			reader = f
		}
		var err error
		raw, err = io.ReadAll(io.LimitReader(reader, pluginproto.MaxLine+1))
		if err != nil {
			return nil, fmt.Errorf("--input-file: %w", err)
		}
		if len(raw) > pluginproto.MaxLine {
			return nil, fmt.Errorf("--input-file exceeds %d bytes", pluginproto.MaxLine)
		}
	default:
		return nil, nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' || !utf8.Valid(raw) || !json.Valid(raw) {
		return nil, errors.New("input must be a JSON object")
	}
	return raw, nil
}

func callCommandPlugin(ctx context.Context, stdout, stderr io.Writer, s *plugin.Session, installed bool, dir, capability string, input json.RawMessage, timeout time.Duration, jsonOut bool) (int, error) {
	info := s.Info()
	task := kernel.Task{
		Prompt:  "plugin " + info.Name + ": " + capability,
		Dir:     dir,
		Options: kernel.Options{Timeout: kernel.Duration(timeout)},
	}
	w, err := runlog.CreatePlugin(task, runlog.PluginRun{
		Name: info.Name, Version: info.Version, Protocol: info.Protocol, Capability: capability,
		SHA256: info.SHA256, Path: info.Path, Installed: installed, Input: input,
	})
	if err != nil {
		return 1, errors.Join(fmt.Errorf("creating run record: %w", err), s.Close())
	}
	fmt.Fprintln(stderr, "run", w.ID())
	started := time.Now()
	reply, callErr := s.Call(ctx, capability, input, timeout, func(event pluginproto.Event) {
		fmt.Fprintf(stderr, "%s: %s\n", event.Kind, printable(mask.Text(event.Text)))
		w.Event(kernel.Event{Time: time.Now(), Kind: kernel.EventInfo, Text: event.Kind + ": " + event.Text})
	})
	result := commandPluginResult(reply, callErr, time.Since(started))
	reported := make([]kernel.Evidence, 0)
	if reply != nil {
		for _, e := range reply.Evidence {
			reported = append(reported, kernel.Evidence{Kind: e.Kind, Source: e.Source, Hash: e.Hash, Excerpt: e.Excerpt})
		}
	}
	w.SetPluginReported(reported)
	closeErr := s.Close()
	var recordErr error
	if err := w.Close(result); err != nil {
		recordErr = errors.New(runlog.CloseMessage(err))
	}
	outputErr := printCommandPluginResult(stdout, stderr, w.ID(), info.Name, info.Version, capability, result, reported, jsonOut)
	if err := errors.Join(closeErr, recordErr, outputErr); err != nil {
		return 1, err
	}
	if result.Status != kernel.StatusOK {
		return 1, nil
	}
	return 0, nil
}

func commandPluginResult(reply *pluginproto.Result, callErr error, duration time.Duration) kernel.Result {
	res := kernel.Result{
		Backend: runlog.PluginBackend, Duration: duration,
		Usage: &kernel.UsageMetadata{InputSemantics: "exclusive_cache", Status: "reported", CostStatus: "reported"},
	}
	if callErr != nil {
		res.Error = callErr.Error()
	}
	switch {
	case reply != nil && reply.Status == pluginproto.StatusOK:
		res.Status, res.Error, res.Final = kernel.StatusOK, "", string(reply.Output)
		if res.Final == "" {
			res.Final = "{}"
		}
	case reply != nil && reply.Status == pluginproto.StatusFailed:
		res.Status, res.Error = kernel.StatusError, reply.Error.Code+": "+reply.Error.Message
	case errors.Is(callErr, context.DeadlineExceeded):
		res.Status = kernel.StatusTimeout
	case errors.Is(callErr, context.Canceled) || (reply != nil && reply.Status == pluginproto.StatusCanceled):
		res.Status = kernel.StatusCanceled
		if res.Error == "" {
			res.Error = "plugin canceled the call"
		}
	default:
		res.Status = kernel.StatusError
	}
	return res
}

func printCommandPluginResult(stdout, stderr io.Writer, id, name, version, capability string, result kernel.Result, reported []kernel.Evidence, jsonOut bool) error {
	if jsonOut {
		return json.NewEncoder(stdout).Encode(struct {
			RunID            string            `json:"run_id"`
			Plugin           string            `json:"plugin"`
			Version          string            `json:"version"`
			Capability       string            `json:"capability"`
			Status           kernel.Status     `json:"status"`
			Output           json.RawMessage   `json:"output,omitempty"`
			Error            string            `json:"error"`
			ReportedEvidence []kernel.Evidence `json:"reported_evidence"`
		}{
			RunID: id, Plugin: name, Version: version, Capability: capability,
			Status: result.Status, Output: json.RawMessage(result.Final), Error: result.Error, ReportedEvidence: reported,
		})
	}
	if result.Status != kernel.StatusOK {
		line := fmt.Sprintf("plugin %s %s: %s: %s", name, capability, result.Status, result.Error)
		_, err := fmt.Fprintln(stderr, strings.ReplaceAll(printable(mask.Text(line)), "\n", " "))
		return err
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, []byte(result.Final), "", "  "); err != nil {
		return err
	}
	_, err := fmt.Fprintln(stdout, pretty.String())
	return err
}
