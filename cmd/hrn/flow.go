package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/cristiangirlea/harness/internal/flow"
	"github.com/cristiangirlea/harness/internal/flowview"
	"github.com/cristiangirlea/harness/internal/owner"
)

type flowDocument struct {
	Version int          `json:"version"`
	Events  []flow.Event `json:"events"`
	Tree    []*flow.Node `json:"tree"`
}

func cmdFlow(args []string) error {
	fs := flag.NewFlagSet("flow", flag.ContinueOnError)
	level := fs.String("level", "technical", "")
	jsonOut := fs.Bool("json", false, "")
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--json", strings.HasPrefix(args[i], "--level="):
			flags = append(flags, args[i])
		case args[i] == "--level":
			if i+1 == len(args) {
				return errors.New("flag needs an argument: --level")
			}
			flags = append(flags, args[i], args[i+1])
			i++
		case strings.HasPrefix(args[i], "-"):
			flags = append(flags, args[i])
		default:
			positional = append(positional, args[i])
		}
	}
	if err := fs.Parse(append(flags, positional...)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: hrn flow <run-or-batch-id> [--level simple|technical|debug] [--json]")
	}
	id := fs.Arg(0)
	if strings.ContainsAny(id, `/\\`) || id == "" {
		return flowNotFound(id)
	}
	if *level != "simple" && *level != "technical" && *level != "debug" {
		return fmt.Errorf("invalid flow level %q: use simple, technical or debug", *level)
	}
	view, err := flowview.Load(owner.LocalViewer(), id)
	if err != nil {
		if errors.Is(err, flowview.ErrNotFound) {
			return flowNotFound(id)
		}
		return err
	}
	tree := flowview.Filter(view.Tree, *level)
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(flowDocument{Version: view.Version, Events: view.Events, Tree: tree})
	}
	return flowview.WriteText(os.Stdout, safeFlowText(view.Summary), safeTree(tree), *level)
}

// safeFlowText keeps secrets and terminal controls in records out of the terminal.
func safeFlowText(s string) string { return printable(flow.Redact(s)) }

func flowNotFound(id string) error { return fmt.Errorf("hrn flow: no run or batch %s", id) }

func safeTree(nodes []*flow.Node) []*flow.Node {
	out := make([]*flow.Node, len(nodes))
	for i, n := range nodes {
		copy := *n
		copy.Name = safeFlowText(copy.Name)
		copy.Text = safeFlowText(copy.Text)
		copy.Reason = safeFlowText(copy.Reason)
		copy.Next = safeFlowText(copy.Next)
		copy.Children = safeTree(copy.Children)
		out[i] = &copy
	}
	return out
}
