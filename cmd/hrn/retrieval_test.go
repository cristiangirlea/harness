package main

import (
	"bytes"
	"encoding/json"
	"github.com/cristiangirlea/harness/internal/eval"
	"strings"
	"testing"
)

func TestRetrievalFixtureCommand(t *testing.T) {
	t.Setenv("HRN_SERVER_URL", "invalid")
	var out bytes.Buffer
	if e := runRetrieval([]string{"fixture", "--json"}, &out); e != nil {
		t.Fatal(e)
	}
	var c eval.RetrievalComparison
	if e := json.Unmarshal(out.Bytes(), &c); e != nil {
		t.Fatal(e)
	}
	if c.ObservedPairs != 30 || c.EvidenceKind != "synthetic_fixture" || c.TokenSavingsSupported || c.CostSavingsSupported {
		t.Fatalf("%+v", c)
	}
	out.Reset()
	if e := runRetrieval([]string{"fixture"}, &out); e != nil || !strings.Contains(out.String(), "synthetic_fixture") {
		t.Fatal(e, out.String())
	}
	t.Log(out.String())
}
