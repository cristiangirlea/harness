package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/update"
)

const updateUsage = "usage: hrn update [--main | --ref REF | --file PATH] [--dry-run] [--rollback]"

func cmdUpdate(args []string) (int, error) {
	opts, err := updateOptions(args)
	if err != nil {
		return 2, err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return update.Run(ctx, os.Stdout, opts)
}

// GITHUB_TOKEN may authenticate API requests but must never reach printed output.
func updateOptions(args []string) (update.Options, error) {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	mainBranch := fs.Bool("main", false, "")
	ref := fs.String("ref", "", "")
	file := fs.String("file", "", "")
	dryRun := fs.Bool("dry-run", false, "")
	rollback := fs.Bool("rollback", false, "")
	fs.Usage = func() {}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return update.Options{}, errors.New(updateUsage)
		}
		return update.Options{}, fmt.Errorf("%w\n%s", err, updateUsage)
	}
	if fs.NArg() != 0 {
		return update.Options{}, fmt.Errorf("unexpected argument %q\n%s", fs.Arg(0), updateUsage)
	}
	sources := 0
	for _, set := range []bool{*mainBranch, *ref != "", *file != ""} {
		if set {
			sources++
		}
	}
	if sources > 1 {
		return update.Options{}, fmt.Errorf("--main, --ref and --file name one source each; give one\n%s", updateUsage)
	}
	if *rollback && sources > 0 {
		return update.Options{}, fmt.Errorf("--rollback puts the backups back; it takes no source\n%s", updateUsage)
	}
	opts := update.Options{DryRun: *dryRun, Rollback: *rollback, Home: runlog.Home()}
	switch {
	case *mainBranch:
		opts.Source.Ref = "main"
	case *ref != "":
		opts.Source.Ref = *ref
	case *file != "":
		opts.Source.File = *file
	case !*rollback:
		opts.Source.Release = &update.ReleaseSource{Token: os.Getenv("GITHUB_TOKEN")}
	}
	return opts, nil
}
