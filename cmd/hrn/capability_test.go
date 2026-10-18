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
