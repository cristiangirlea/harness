package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/cristiangirlea/harness/internal/execproto"
)

func cmdExec(args []string) (int, error) {
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	protocol := fs.String("protocol", "", "Supervisor protocol version (required: v1).")
	fs.Usage = func() { fmt.Fprint(os.Stderr, "usage: hrn exec --protocol v1\n") }
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if *protocol != "v1" || fs.NArg() != 0 {
		return 2, fmt.Errorf("usage: hrn exec --protocol v1 (an explicit v1 is required)")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return execproto.Run(ctx, os.Stdin, os.Stdout, nil), nil
}
