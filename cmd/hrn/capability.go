package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

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

var addTaskPattern = regexp.MustCompile(`^add (-?[0-9]+) and (-?[0-9]+)$`)

// tryDeterministicTask recognizes only an unambiguous, bounded task grammar.
// Unknown requests are never guessed at or rewritten into a different operation.
func tryDeterministicTask(ctx context.Context, prompt string) (capability.Result, bool, error) {
	matches := addTaskPattern.FindStringSubmatch(strings.ToLower(strings.TrimSpace(prompt)))
	if matches == nil {
		return capability.Result{}, false, nil
	}
	a, err := strconv.ParseInt(matches[1], 10, 64)
	if err != nil {
		return capability.Result{}, true, err
	}
	b, err := strconv.ParseInt(matches[2], 10, 64)
	if err != nil {
		return capability.Result{}, true, err
	}
	input, err := json.Marshal(map[string]int64{"a": a, "b": b})
	if err != nil {
		return capability.Result{}, true, err
	}
	result, err := capabilityRegistry().Execute(ctx, "math.add", "1", capability.Request{Input: input})
	return result, true, err
}
