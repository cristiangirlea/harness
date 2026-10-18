package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/cristiangirlea/harness/internal/capability"
)

// capabilityRegistry is deliberately local and deterministic. Later adapters can
// register domain-specific solvers without changing the CLI or requiring a model.
func capabilityRegistry() *capability.Registry {
	r := capability.NewRegistry()
	_ = r.Register(capability.Descriptor{
		ID:          "math.add",
		Version:     "1",
		Domain:      "math",
		Description: "Add two integers without an LLM",
	}, func(_ context.Context, input []byte) (capability.Result, error) {
		var values struct {
			A int64 `json:"a"`
			B int64 `json:"b"`
		}
		if err := json.Unmarshal(input, &values); err != nil {
			return capability.Result{}, err
		}
		sum := values.A + values.B
		if (values.B > 0 && sum < values.A) || (values.B < 0 && sum > values.A) {
			return capability.Result{}, errors.New("integer overflow")
		}
		output, err := json.Marshal(map[string]int64{"sum": sum})
		return capability.Result{Output: output}, err
	})
	return r
}

func runCapability(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: hrn capability list | run <id> <version> <json-input>")
	}
	r := capabilityRegistry()
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return errors.New("usage: hrn capability list")
		}
		return json.NewEncoder(out).Encode(r.List())
	case "run":
		if len(args) != 4 {
			return errors.New("usage: hrn capability run <id> <version> <json-input>")
		}
		result, err := r.Execute(ctx, args[1], args[2], capability.Request{Input: []byte(args[3])})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, string(result.Output))
		return err
	default:
		return fmt.Errorf("unknown capability subcommand %q", args[0])
	}
}
