package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/eval"
	"github.com/cristiangirlea/harness/internal/purpose"
	"github.com/cristiangirlea/harness/internal/resolve"
	"github.com/cristiangirlea/harness/internal/route"
	"github.com/cristiangirlea/harness/internal/runlog"
)

const routeUsage = `usage: hrn route show
       hrn route set <purpose> [-b NAME] [-m MODEL] [--effort LEVEL]
       hrn route set <purpose> --fallback -b NAME [-m MODEL] [--effort LEVEL]
       hrn route from-eval <id> [--margin 0.05]`

func cmdRoute(args []string) (int, error) {
	if len(args) == 0 {
		return 2, errors.New(routeUsage)
	}
	switch args[0] {
	case "show":
		if len(args) != 1 {
			return 2, errors.New(routeUsage)
		}
		t, err := route.Load()
		if err != nil {
			return 1, err
		}
		return 0, printRoutes(os.Stdout, t)
	case "set":
		if len(args) < 2 || !slices.Contains(purpose.Categories, args[1]) {
			return 2, fmt.Errorf("want a purpose from %v\n%s", purpose.Categories, routeUsage)
		}
		fs := flag.NewFlagSet("route set", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		var row route.Row
		for _, n := range []string{"b", "backend"} {
			fs.StringVar(&row.Backend, n, "", "")
		}
		for _, n := range []string{"m", "model"} {
			fs.StringVar(&row.Model, n, "", "")
		}
		fs.StringVar(&row.Effort, "effort", "", "")
		fallback := fs.Bool("fallback", false, "")
		if err := fs.Parse(args[2:]); err != nil {
			return 2, err
		}
		if fs.NArg() != 0 || row.Backend == "" && (*fallback || row.Model == "" && row.Effort == "") {
			return 2, errors.New(routeUsage)
		}
		if row.Backend != "" {
			if _, err := agent.Get(row.Backend); err != nil {
				return 2, err
			}
			if p, err := resolve.BackendPreferences(runlog.Home()); err == nil && !p.Enabled(row.Backend) {
				fmt.Fprintf(os.Stderr, "hrn: warning: backend %s is disabled; runs ignore this row until it is enabled (hrn backends set)\n", row.Backend)
			}
		}
		if err := agent.ValidateOptions(agent.Options{Effort: row.Effort}); err != nil {
			return 2, err
		}
		t, err := route.Load()
		if err != nil {
			return 1, err
		}
		row.Set = time.Now().Format("2006-01-02")
		old, exists := t[args[1]]
		if *fallback {
			if !exists {
				return 2, fmt.Errorf("purpose %s has no row to add a fallback to; set its first choice first", args[1])
			}
			if row.Backend == old.Backend {
				return 2, fmt.Errorf("backend %s is the first choice for %s; a fallback names another backend", row.Backend, args[1])
			}
			if row.Model == "" && row.Effort == "" {
				row = old.WithoutFallback(row.Backend)
			} else {
				row = old.WithFallback(row)
			}
		} else {
			for _, f := range old.Fallbacks {
				if f.Backend != row.Backend {
					row = row.WithFallback(f)
				}
			}
		}
		t[args[1]] = row
		if err := route.Save(t); err != nil {
			return 1, err
		}
		return 0, printRoutes(os.Stdout, route.Table{args[1]: row})
	case "from-eval":
		if len(args) < 2 {
			return 2, errors.New(routeUsage)
		}
		fs := flag.NewFlagSet("route from-eval", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		margin := fs.Float64("margin", .05, "")
		if err := fs.Parse(args[2:]); err != nil {
			return 2, err
		}
		if fs.NArg() != 0 || math.IsNaN(*margin) || math.IsInf(*margin, 0) || *margin < 0 || *margin > 1 {
			return 2, errors.New("margin must be a finite fraction from 0 to 1; " + routeUsage)
		}
		rec, err := eval.LoadRecord(args[1])
		if err != nil {
			return 1, err
		}
		rows, skipped := route.FromEval(rec, *margin)
		t, err := route.Load()
		if err != nil {
			return 1, err
		}
		if err := printRoutes(os.Stdout, rows); err != nil {
			return 1, err
		}
		r := newRenderer(false)
		for _, cell := range skipped {
			r.Line(r.c(dim, fmt.Sprintf("  skipped variant %s, task %s: %s", cell.Variant, cell.Task, cell.Reason)))
		}
		for category, row := range rows {
			t[category] = row
		}
		if err := route.Save(t); err != nil {
			return 1, err
		}
		return 0, nil
	default:
		return 2, fmt.Errorf("unknown route command %q\n%s", args[0], routeUsage)
	}
}

func printRoutes(out io.Writer, t route.Table) error {
	if len(t) == 0 {
		_, err := fmt.Fprintln(out, "(empty)")
		return err
	}
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "purpose\tbackend\tmodel\teffort\teval\tset")
	for _, k := range keys {
		r := t[k]
		source := r.Eval
		if source == "" {
			source = "hand"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", k, r.Backend, r.Model, r.Effort, source, r.Set)
		for _, f := range r.Fallbacks {
			source := f.Eval
			if source == "" {
				source = "hand"
			}
			fmt.Fprintf(w, "%s (fallback)\t%s\t%s\t%s\t%s\t%s\n", k, f.Backend, f.Model, f.Effort, source, f.Set)
		}
	}
	return w.Flush()
}
