package main

import (
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

	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/eval"
	"github.com/cristiangirlea/harness/internal/report"
)

const evalSetupUsage = "usage: hrn eval setup [--base REF] [--paths P,P] [--held-out SHARE --seed N] [--keep] [--json] [-v] <eval.json>"

var (
	runSetupComparison = eval.RunSetupComparison
	removeBatch        = batch.Remove
)

func cmdEvalSetup(args []string) (int, error) { return evalSetup(args, os.Stdout, os.Stderr) }

func evalSetup(args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("eval setup", flag.ContinueOnError)
	base := fs.String("base", "origin/main", "")
	paths := fs.String("paths", "", "")
	heldOut := fs.Float64("held-out", 0, "")
	seed := fs.Uint64("seed", 0, "")
	keep := fs.Bool("keep", false, "")
	jsonOut := fs.Bool("json", false, "")
	var verbose bool
	for _, name := range []string{"v", "verbose"} {
		fs.BoolVar(&verbose, name, false, "")
	}
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		return 2, errors.New(evalSetupUsage)
	}
	spec, err := eval.LoadSpec(fs.Arg(0))
	if err != nil {
		return 2, err
	}
	opts := eval.SetupOptions{Source: *base, HeldOut: *heldOut, Seed: *seed}
	if *paths != "" {
		opts.Paths = strings.Split(*paths, ",")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	r := newRenderer(false)
	r.out = stdout
	if *jsonOut {
		r.out = stderr
	}
	run, err := runSetupComparison(ctx, spec, opts, func(e batch.Event) {
		if e.Kind == "tool" && !verbose {
			return
		}
		r.Line(printable(fmt.Sprintf("[%s] %s", e.Task, e.Text)))
	})
	if !*keep {
		for _, res := range []batch.Result{run.BaselineRes, run.CandidateRes} {
			if res.ID == "" {
				continue
			}
			if rmErr := removeBatch(res); rmErr != nil {
				r.Line(r.c(red, "removing worktrees: "+rmErr.Error()))
			}
		}
	}
	if err != nil {
		return 1, err
	}
	if *jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return 0, enc.Encode(run.Comparison)
	}
	printEvalDiff(r, eval.Compare(run.Baseline, run.Candidate))
	printSetupComparison(r, run.Comparison)
	return 0, nil
}

func printSetupComparison(r *renderer, c eval.SetupComparison) {
	r.Line("")
	r.Line(fmt.Sprintf("  setup: %s's setup files → this branch's (%s)", c.Source, strings.Join(c.Changed, ", ")))
	for _, s := range []struct {
		name string
		side eval.SetupSide
	}{{"baseline", c.Baseline}, {"candidate", c.Candidate}} {
		perSuccess := "-"
		if s.side.TokensPerSuccess != nil {
			perSuccess = report.Tokens(int64(*s.side.TokensPerSuccess))
		}
		failed := report.Tokens(s.side.FailedTokens)
		if s.side.UnknownTokens > 0 {
			// Cells without a known count add nothing, so the sum is a floor.
			failed = fmt.Sprintf("at least %s (%d cells unknown)", failed, s.side.UnknownTokens)
		}
		line := fmt.Sprintf("  %-10s eval %s · %d/%d passed · %s tokens on failures · %s per success",
			s.name, s.side.Eval, s.side.Passed, s.side.Acted, failed, perSuccess)
		if s.side.DidNotAct > 0 {
			line += fmt.Sprintf(" · %d did not act", s.side.DidNotAct)
		}
		r.Line(line)
	}
	r.Line(fmt.Sprintf("  verdict: %s, %s", c.Verdict, c.Reason))
	r.Line(r.c(dim, "  a judge's scores, when there is one, never change the verdict: the gates decide passing"))
}
