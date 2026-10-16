package shape

import (
	"fmt"
	"strings"
	"testing"
)

func jsonItems(n int) string {
	var b strings.Builder
	b.WriteString("[\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "  {\n    \"id\": %d,\n    \"name\": \"item-%d\",\n    \"tags\": [\"a\", \"b\"],\n    \"owner\": {\"login\": \"dev\", \"meta\": {\"rank\": %d}}\n  }", i, i, i%7)
		if i < n-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("]\n")
	return b.String()
}

const nestedJSON = `{
  "name": "hrn",
  "limits": {
    "output": {"bytes": 28000, "lines": 400, "head": 18000, "tail": 9000},
    "depth": 2
  },
  "paths": ["/tmp/a", "/tmp/b"],
  "deep": {"a": {"b": {"c": {"d": 1, "e": [10, 20, 30]}}}}
}
`

func csvRows(n, broken int) string {
	var b strings.Builder
	b.WriteString("id,name,status,ms\n")
	for i := 1; i <= n; i++ {
		status := "ok"
		if i == broken {
			status = "error: timeout"
		}
		fmt.Fprintf(&b, "%d,job-%d,%s,%d\n", i, i, status, i*10)
	}
	return b.String()
}

func tsvRows(n int) string {
	var b strings.Builder
	b.WriteString("host\tport\tstate\n")
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "node%d\t%d\tup\n", i, 8000+i)
	}
	return b.String()
}

const goTestJSON = `{"Time":"2026-10-16T10:00:00Z","Action":"start","Package":"example/pkg"}
{"Time":"2026-10-16T10:00:00Z","Action":"run","Package":"example/pkg","Test":"TestA"}
{"Time":"2026-10-16T10:00:00Z","Action":"output","Package":"example/pkg","Test":"TestA","Output":"=== RUN   TestA\n"}
{"Time":"2026-10-16T10:00:00Z","Action":"output","Package":"example/pkg","Test":"TestA","Output":"    a_test.go:9: got 1, want 2\n"}
{"Time":"2026-10-16T10:00:00Z","Action":"output","Package":"example/pkg","Test":"TestA","Output":"--- FAIL: TestA (0.00s)\n"}
{"Time":"2026-10-16T10:00:00Z","Action":"fail","Package":"example/pkg","Test":"TestA","Elapsed":0}
{"Time":"2026-10-16T10:00:00Z","Action":"fail","Package":"example/pkg","Elapsed":0.01}
`

func TestClassifyRecognisesJSONAndTables(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		want       Kind
	}{
		{"object", `  {"a": 1}` + "\n", JSON},
		{"array", "[1, 2, 3]", JSON},
		{"array of objects", jsonItems(3), JSON},
		{"scalar is not structured", `"just a string"`, Terminal},
		{"number is not structured", "42", Terminal},
		{"invalid json", `{"a": 1`, Terminal},
		{"csv", csvRows(4, 0), Table},
		{"tsv", tsvRows(4), Table},
		{"four lines are too few", csvRows(3, 0), Terminal},
		{"no delimiter", "a\nb\nc\nd\ne\n", Terminal},
		{"csv with quoted commas", "id,name\n1,\"Smith, J\"\n2,Lee\n3,\"Ng, K\"\n4,Ode\n", Terminal},
		// Order: JSON, then Test, Table, Log, Terminal.
		{"json array of log lines", `["2026-10-16 10:00:00 ERROR one", "2026-10-16 10:00:01 INFO two"]`, JSON},
		{"csv with an ERROR column", "level,msg\nINFO,start\nERROR,broken\nINFO,retry\nERROR,gave up\n", Table},
		{"tsv log", "2026-10-16 10:00:00\tINFO\tstart\n2026-10-16 10:00:01\tWARN\tslow\n2026-10-16 10:00:02\tERROR\tbroken\n2026-10-16 10:00:03\tINFO\tretry\n2026-10-16 10:00:04\tINFO\tdone\n", Table},
		{"test output with consistent tabs", "ok\texample/a\t0.1s\nok\texample/b\t0.1s\nok\texample/c\t0.1s\nok\texample/d\t0.1s\nFAIL\texample/e\t0.1s\n", Test},
		{"go test json", goTestJSON, Terminal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.text); got != tc.want {
				t.Fatalf("Classify = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGoTestJSONIsNeitherJSONNorTable(t *testing.T) {
	// Every line with the same comma count still reads as NDJSON, not a table.
	uniform := strings.Repeat(`{"Action":"run","Test":"TestA"}`+"\n", 6)
	for _, text := range []string{goTestJSON, uniform} {
		if got := Classify(text); got == JSON || got == Table {
			t.Fatalf("Classify = %v for NDJSON", got)
		}
		if _, ok := Summarize(JSON, text); ok {
			t.Fatal("summarized NDJSON as JSON")
		}
		if _, ok := Summarize(Table, text); ok {
			t.Fatal("summarized NDJSON as a table")
		}
	}
}

func TestJSONArraySummaryNamesCountKeysAndOneSample(t *testing.T) {
	text := jsonItems(500)
	want := "json: array of 500 items\n" +
		`keys: "id","name","tags","owner"` + "\n" +
		`sample: {"id":0,"name":"item-0","tags":["a","b"],"owner":{"login":"dev","meta":{...}}}`
	s, ok := Summarize(JSON, text)
	if !ok || s != want {
		t.Fatalf("summary ok=%v:\n%s", ok, s)
	}
	if lost := lostFact(text, s); lost != "" {
		t.Fatal(lost)
	}
}

func TestNestedObjectSummaryLimitsDepthToTwo(t *testing.T) {
	want := "json: object with 4 keys\n" +
		`keys: "name","limits","paths","deep"` + "\n" +
		`sample: {"name":"hrn","limits":{"output":{...},"depth":2},"paths":["/tmp/a","/tmp/b"],"deep":{"a":{...}}}`
	s, ok := Summarize(JSON, nestedJSON)
	if !ok || s != want {
		t.Fatalf("summary ok=%v:\n%s", ok, s)
	}
	if lost := lostFact(nestedJSON, s); lost != "" {
		t.Fatal(lost)
	}
}

func TestJSONSummaryKeepsErrorLinesAndOmitsSamplesThatHoldThem(t *testing.T) {
	text := "[\n  {\n    \"id\": 1,\n    \"error\": null\n  },\n  {\n    \"id\": 2,\n    \"error\": \"disk full\"\n  },\n  {\n    \"id\": 3,\n    \"error\": null\n  },\n  {\n    \"id\": 4,\n    \"error\": null\n  }\n]\n"
	want := "json: array of 4 items\n    \"error\": null\n    \"error\": \"disk full\"\n    \"error\": null\n    \"error\": null"
	s, ok := Summarize(JSON, text)
	if !ok || s != want {
		t.Fatalf("summary ok=%v:\n%s", ok, s)
	}
}

func TestEmptyContainersSummarizeWithoutASample(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{"[" + strings.Repeat(" ", 100) + "]", "json: array of 0 items"},
		{"[[]," + strings.Repeat(" ", 100) + "[]]", "json: array of 2 items\nsample: []"},
		{"{" + strings.Repeat("\n", 100) + "}", "json: object with 0 keys\nsample: {}"},
	} {
		if s, ok := Summarize(JSON, tc.text); !ok || s != tc.want {
			t.Errorf("summary of %q: %q ok=%v", tc.text, s, ok)
		}
	}
}

func TestCSVAndTSVSummariesKeepHeaderEdgesAndErrors(t *testing.T) {
	for _, tc := range []struct{ name, text, want string }{
		{"csv", csvRows(1000, 600), "table: 1000 rows, 4 columns\nid,name,status,ms\n1,job-1,ok,10\n2,job-2,ok,20\n3,job-3,ok,30\n[... omitted ...]\n600,job-600,error: timeout,6000\n[... omitted ...]\n999,job-999,ok,9990\n1000,job-1000,ok,10000"},
		{"tsv", tsvRows(1000), "table: 1000 rows, 3 columns\nhost\tport\tstate\nnode1\t8001\tup\nnode2\t8002\tup\nnode3\t8003\tup\n[... omitted ...]\nnode999\t8999\tup\nnode1000\t9000\tup"},
		{"error in the last row", csvRows(1000, 1000), "table: 1000 rows, 4 columns\nid,name,status,ms\n1,job-1,ok,10\n2,job-2,ok,20\n3,job-3,ok,30\n[... omitted ...]\n999,job-999,ok,9990\n1000,job-1000,error: timeout,10000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.text); got != Table {
				t.Fatalf("Classify = %v", got)
			}
			s, ok := Summarize(Table, tc.text)
			if !ok || s != tc.want {
				t.Fatalf("summary ok=%v:\n%s", ok, s)
			}
			if lost := lostFact(tc.text, s); lost != "" {
				t.Fatal(lost)
			}
		})
	}
}

func TestStructuredSummariesFallBackWhenNotSmaller(t *testing.T) {
	for _, tc := range []struct {
		kind Kind
		text string
	}{
		{JSON, `[{"a": 1}]`}, {Table, csvRows(5, 0)},
		{JSON, `{"blob": "` + strings.Repeat("x", 40000) + `"}`},
		{JSON, csvRows(10, 0)}, {Table, jsonItems(10)},
	} {
		if s, ok := Summarize(tc.kind, tc.text); ok {
			t.Errorf("kind %v accepted %q", tc.kind, s)
		}
	}
}

func TestStructuredInvariantsRejectAdversarialSummaries(t *testing.T) {
	items := jsonItems(500)
	good := "json: array of 500 items\n" + `keys: "id","name","tags","owner"` + "\n" + `sample: {"id":0,"name":"item-0","tags":["a","b"],"owner":{"login":"dev","meta":{...}}}`
	errItems := "[\n  {\"id\": 1},\n  {\"id\": 2, \"status\": \"error: disk full\"},\n  {\"id\": 3},\n  {\"id\": 4}\n]\n"
	csv := csvRows(1000, 600)
	table := "table: 1000 rows, 4 columns\nid,name,status,ms\n1,job-1,ok,10\n[... omitted ...]\n600,job-600,error: timeout,6000\n[... omitted ...]\n1000,job-1000,ok,10000"
	if !valid(JSON, items, good) || !valid(Table, csv, table) {
		t.Fatal("rejected a true summary")
	}
	for _, tc := range []struct {
		name              string
		kind              Kind
		original, summary string
	}{
		{"wrong item count", JSON, items, strings.Replace(good, "500", "499", 1)},
		{"item count of another kind", JSON, items, strings.Replace(good, "array of 500 items", "object with 500 keys", 1)},
		{"changed number in the sample", JSON, items, strings.Replace(good, `"id":0`, `"id":7777`, 1)},
		{"invented path in the sample", JSON, items, strings.Replace(good, `"item-0"`, `"/etc/passwd"`, 1)},
		{"invented negation in the sample", JSON, items, strings.Replace(good, `"dev"`, `"not dev"`, 1)},
		{"dropped error line", JSON, errItems, "json: array of 4 items\n" + `keys: "id"` + "\n" + `sample: {"id":1}`},
		{"invented error in the sample", JSON, errItems, "json: array of 4 items\n" + `sample: {"id":1,"status":"error: disk full"}` + "\n  {\"id\": 2, \"status\": \"error: disk full\"},"},
		{"head line not first", JSON, items, "keys: \"id\"\n" + strings.SplitN(good, "\n", 2)[0]},
		{"json head for a table kind", Table, items, good},
		{"json head over a table", Table, csv, strings.Replace(table, "table: 1000 rows, 4 columns", "json: array of 1000 items", 1)},
		{"table head for a json kind", JSON, csv, table},
		{"wrong row count", Table, csv, strings.Replace(table, "1000 rows", "999 rows", 1)},
		{"wrong column count", Table, csv, strings.Replace(table, "4 columns", "5 columns", 1)},
		{"table head over text that is not a table", Table, "a,b\n1,2\n", "table: 1 rows, 2 columns\na,b\n1,2"},
		{"dropped middle error row", Table, csv, strings.Replace(table, "600,job-600,error: timeout,6000\n[... omitted ...]\n", "", 1)},
		{"changed number in a row", Table, csv, strings.Replace(table, "1,job-1,ok,10\n", "1,job-1,ok,77777\n", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if valid(tc.kind, tc.original, tc.summary) {
				t.Fatal("accepted an invalid summary")
			}
		})
	}
}
