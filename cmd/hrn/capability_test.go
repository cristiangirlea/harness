package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestCapabilityCommandWithoutModel(t *testing.T) {
	var out bytes.Buffer
	if err := runCapability(context.Background(), []string{"run", "math.add", "1", `{"a":12,"b":30}`}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != `{"sum":42}` {
		t.Fatalf("unexpected result: %s", out.String())
	}
}

func TestCapabilityCommandRejectsInvalidInput(t *testing.T) {
	for _, args := range [][]string{
		{"run", "math.add", "1", "invalid"},
		{"run", "unknown", "1", "{}"},
		{"run", "math.add", "1", `{"a":9223372036854775807,"b":1}`},
		{"run", "math.add"},
	} {
		if err := runCapability(context.Background(), args, &bytes.Buffer{}); err == nil {
			t.Fatalf("expected failure for %v", args)
		}
	}
}

func TestCapabilityList(t *testing.T) {
	var out bytes.Buffer
	if err := runCapability(context.Background(), []string{"list"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "math.add") {
		t.Fatalf("missing capability: %s", out.String())
	}
}

func TestDeterministicRouting(t *testing.T) {
	for _, tc := range []struct {
		prompt string
		want string
		matched bool
		wantErr bool
	}{
		{prompt: "add 12 and 30", want: `{"sum":42}`, matched: true},
		{prompt: "ADD -2 AND 3", want: `{"sum":1}`, matched: true},
		{prompt: "write a function to add 12 and 30", matched: false},
		{prompt: "add 9223372036854775807 and 1", matched: true, wantErr: true},
		{prompt: "add 999999999999999999999 and 1", matched: true, wantErr: true},
	} {
		result, matched, err := tryDeterministicTask(context.Background(), tc.prompt)
		if matched != tc.matched || (err != nil) != tc.wantErr {
			t.Fatalf("%q: matched=%v err=%v", tc.prompt, matched, err)
		}
		if !tc.wantErr && tc.matched && string(result.Output) != tc.want {
			t.Fatalf("%q: got %s", tc.prompt, result.Output)
		}
	}
}
