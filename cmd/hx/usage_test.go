package main

import (
	"flag"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/agent"
)

// The usage text is written by hand; this keeps it from falling behind the
// options struct, which is where the flags now come from.
func TestUsageNamesEveryOptionFlag(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	var o agent.Options
	agent.BindFlags(fs, &o)
	fs.VisitAll(func(f *flag.Flag) {
		if !strings.Contains(usage, "--"+f.Name) {
			t.Errorf("usage text does not mention --%s", f.Name)
		}
	})
}
