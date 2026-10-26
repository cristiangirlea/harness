package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"text/tabwriter"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/ws"
)

const wsUsage = `usage: hrn ws take [--repo DIR] [--branch NAME] [--base REF] [--for TEXT] [--max N] [--no-fetch]
       hrn ws free [PATH]
       hrn ws list [--repo DIR] [--json]
       hrn ws issues [--json] [--free owner/name#N]`

// takeRunIssue leases opts.Issue before the agent starts; nil when the run names no issue.
func takeRunIssue(ctx context.Context, opts agent.Options, dir, run, backend string) (*ws.IssueLease, error) {
	if opts.Issue == "" {
		return nil, nil
	}
	il, err := ws.TakeRunIssue(ctx, runlog.Home(), opts.Issue, dir, ws.Lease{Holder: "hrn run", Run: run, Backend: backend}, opts.TakeOver)
	if err != nil {
		return nil, err
	}
	for _, n := range il.Notes {
		fmt.Fprintln(os.Stderr, "hrn: "+n)
	}
	return il, nil
}

func releaseRunIssue(ctx context.Context, il *ws.IssueLease, reason string) {
	if il == nil {
		return
	}
	seen := len(il.Notes)
	if err := il.Release(ctx, reason); err != nil {
		fmt.Fprintln(os.Stderr, "hrn:", err)
	}
	for _, n := range il.Notes[seen:] {
		fmt.Fprintln(os.Stderr, "hrn: "+n)
	}
}

// Keep take's stdout path-only so callers can use it directly with cd.
func cmdWS(args []string) (int, error) {
	if len(args) == 0 {
		return 2, fmt.Errorf("%s", wsUsage)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	switch args[0] {
	case "take":
		return wsTake(ctx, args[1:])
	case "free":
		return wsFree(ctx, args[1:])
	case "list":
		return wsList(ctx, args[1:])
	case "issues":
		return wsIssues(ctx, args[1:])
	}
	return 2, fmt.Errorf("unknown ws command %q\n%s", args[0], wsUsage)
}

func wsTake(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("ws take", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repo := fs.String("repo", ".", "")
	branch := fs.String("branch", "", "")
	base := fs.String("base", "", "")
	holder := fs.String("for", "hrn ws take", "")
	max := fs.Int("max", ws.DefaultMax, "")
	noFetch := fs.Bool("no-fetch", false, "")
	if err := fs.Parse(args); err != nil {
		return 2, fmt.Errorf("%w\n%s", err, wsUsage)
	}
	if fs.NArg() > 0 {
		return 2, fmt.Errorf("unexpected %q\n%s", fs.Arg(0), wsUsage)
	}
	if *max < 1 {
		return 2, fmt.Errorf("--max %d: at least one slot\n%s", *max, wsUsage)
	}
	if *max > ws.Limit {
		return 2, fmt.Errorf("--max %d: at most %d slots per repository", *max, ws.Limit)
	}
	got, err := ws.Take(ctx, ws.TakeOptions{Repo: *repo, Branch: *branch, Base: *base, Holder: *holder, Max: *max, NoFetch: *noFetch})
	for _, n := range got.Notes {
		fmt.Fprintln(os.Stderr, n)
	}
	if err != nil {
		return 1, err
	}
	fmt.Println(got.Path)
	return 0, nil
}

func wsFree(ctx context.Context, args []string) (int, error) {
	if len(args) > 1 {
		return 2, fmt.Errorf("%s", wsUsage)
	}
	path := "."
	if len(args) == 1 {
		path = args[0]
	}
	got, err := ws.Free(ctx, path)
	if err != nil {
		return 1, err
	}
	fmt.Fprintf(os.Stderr, "freed slot %d (%s), held by %q\n", got.N, got.Path, got.Lease.Holder)
	return 0, nil
}

func wsList(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("ws list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repo := fs.String("repo", ".", "")
	asJSON := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil {
		return 2, fmt.Errorf("%w\n%s", err, wsUsage)
	}
	if fs.NArg() > 0 {
		return 2, fmt.Errorf("unexpected %q\n%s", fs.Arg(0), wsUsage)
	}
	rows, err := ws.List(ctx, *repo)
	if err != nil {
		return 1, err
	}
	if *asJSON {
		if rows == nil {
			rows = []ws.Status{}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return 0, enc.Encode(rows)
	}
	if len(rows) == 0 {
		fmt.Println("no slots yet; hrn ws take makes the first")
		return 0, nil
	}
	printWS(os.Stdout, rows)
	return 0, nil
}

func wsIssues(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("ws issues", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "")
	free := fs.String("free", "", "")
	if err := fs.Parse(args); err != nil {
		return 2, fmt.Errorf("%w\n%s", err, wsUsage)
	}
	if fs.NArg() > 0 {
		return 2, fmt.Errorf("unexpected %q\n%s", fs.Arg(0), wsUsage)
	}
	if *free != "" {
		ref, err := ws.ParseIssue(*free, ".")
		if err != nil {
			return 2, err
		}
		if err := ws.FreeIssue(runlog.Home(), ref); err != nil {
			return 1, err
		}
		fmt.Fprintf(os.Stderr, "freed issue %s\n", ref)
		return 0, nil
	}
	rows, err := ws.ListIssues(ctx, runlog.Home())
	if err != nil {
		return 1, err
	}
	if *asJSON {
		if rows == nil {
			rows = []ws.IssueStatus{}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return 0, enc.Encode(rows)
	}
	if len(rows) == 0 {
		fmt.Println("no issue leases; hrn run --issue takes one")
		return 0, nil
	}
	printIssues(os.Stdout, rows)
	return 0, nil
}

func printIssues(w io.Writer, rows []ws.IssueStatus) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, s := range rows {
		dash := func(v string) string {
			if v == "" {
				return "-"
			}
			return v
		}
		since := "-"
		if !s.Since.IsZero() {
			since = s.Since.Local().Format("2006-01-02 15:04")
		}
		state := s.State
		if s.Why != "" {
			state += " (" + s.Why + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.Issue, state, dash(s.Holder), dash(s.Run), dash(s.Backend), dash(s.Branch), since, dash(s.PR))
	}
	tw.Flush()
}

func printWS(w io.Writer, rows []ws.Status) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, s := range rows {
		holder, since, what := "-", "-", "-"
		if s.Holder != "" {
			holder = s.Holder
		}
		if !s.Since.IsZero() {
			since = s.Since.Local().Format("2006-01-02 15:04")
		}
		switch {
		case s.Branch != "":
			what = s.Branch
		case s.Head != "":
			what = "(detached " + s.Head + ")"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", s.N, s.Path, s.State, holder, since, what, s.Contents)
	}
	tw.Flush()
}
