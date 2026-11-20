package pluginproto

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type parser func([]byte) (any, error)

func TestGoldenFramesRoundTripByteForByte(t *testing.T) {
	for _, side := range []struct {
		name  string
		parse parser
	}{
		{"host", ParseHostLine},
		{"plugin", ParsePluginLine},
	} {
		forEachFixture(t, side.name, func(t *testing.T, _ string, line []byte) {
			frame, err := side.parse(line)
			if err != nil {
				t.Fatal(err)
			}
			got, err := json.Marshal(frame)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(append(got, '\n'), line) {
				t.Fatalf("got %s, want %s", got, line)
			}
		})
	}
}

func TestToleratedFramesParseAndDiscardUnknownFieldsAndNulls(t *testing.T) {
	forEachFixture(t, "tolerated", func(t *testing.T, name string, line []byte) {
		var parse parser
		switch prefix, _, _ := strings.Cut(name, "-"); prefix {
		case "ready", "result":
			parse = ParsePluginLine
		case "call":
			parse = ParseHostLine
		default:
			t.Fatalf("unexpected fixture %q", name)
		}
		frame, err := parse(line)
		if err != nil {
			t.Fatal(err)
		}
		switch frame := frame.(type) {
		case *Result:
			if frame.Output != nil {
				t.Fatalf("output = %s, want nil", frame.Output)
			}
		case *Ready:
			if frame.Plugin.Name != "search" {
				t.Fatalf("plugin name = %q, want search", frame.Plugin.Name)
			}
		case *Call:
			if frame.ID != "c1" || frame.Input != nil || frame.TimeoutMS != 0 {
				t.Fatalf("unexpected call: %+v", frame)
			}
		default:
			t.Fatalf("unexpected frame %T", frame)
		}
	})
}

func forEachFixture(t *testing.T, dir string, check func(*testing.T, string, []byte)) {
	t.Helper()
	dir = filepath.Join("testdata", "v1", dir)
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, file := range files {
		if !file.Type().IsRegular() {
			continue
		}
		count++
		t.Run(file.Name(), func(t *testing.T) {
			line, err := os.ReadFile(filepath.Join(dir, file.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.HasSuffix(line, []byte{'\n'}) || bytes.Count(line, []byte{'\n'}) != 1 {
				t.Fatal("fixture must contain one newline-terminated line")
			}
			check(t, file.Name(), line)
		})
	}
	if count == 0 {
		t.Fatalf("no fixtures in %s", dir)
	}
}

const (
	helloLine  = `{"type":"hello","protocols":["v1"],"host":{"name":"hrn","version":"0.2.0"}}`
	callLine   = `{"type":"call","id":"c1","capability":"search"}`
	cancelLine = `{"type":"cancel","id":"c1"}`
	readyLine  = `{"v":1,"seq":1,"type":"ready","protocol":"v1","plugin":{"name":"search","version":"0.1.0"},"capabilities":[{"name":"search"}]}`
	eventLine  = `{"v":1,"seq":2,"type":"event","id":"c1","kind":"info","text":""}`
	resultLine = `{"v":1,"seq":3,"type":"result","id":"c1","status":"ok"}`
	failedLine = `{"v":1,"seq":3,"type":"result","id":"c1","status":"failed"}`
	errorLine  = `{"v":1,"seq":1,"type":"error","code":"bad_request","message":"bad input"}`
)

type invalidLine struct {
	name  string
	line  string
	field string
}

func TestBothParsersRejectMalformedLines(t *testing.T) {
	for _, side := range []struct {
		name  string
		parse parser
		line  string
		other string
	}{
		{"host", ParseHostLine, callLine, readyLine},
		{"plugin", ParsePluginLine, readyLine, callLine},
	} {
		t.Run(side.name, func(t *testing.T) {
			rejectLines(t, side.parse, []invalidLine{
				{"line is too long", paddedLine(side.line, MaxLine+1), "line"},
				{"line is empty", "", "line"},
				{"line is null", "null", "line"},
				{"line is an array", "[]", "line"},
				{"line is a scalar", "42", "line"},
				{"line has trailing data", side.line + " {}", "line"},
				{"line has invalid UTF-8", appendFields(side.line, "\"x\":\"\xff\""), "line"},
				{"line has an embedded newline", "{\n" + side.line[1:], "line"},
				{"type is missing", "{}", "type"},
				{"type uses the wrong case", `{"Type":"call"}`, "type"},
				{"type belongs to the other side", side.other, "type"},
				{"type is unknown", `{"type":"unknown"}`, "type"},
			})
		})
	}
}

func TestHostParserRejectsInvalidFields(t *testing.T) {
	rejectLines(t, ParseHostLine, []invalidLine{
		{"host name must start with a letter", replace(helloLine, "hrn", "9hrn"), "hello.host.name"},
		{"host name is case sensitive", replace(helloLine, `"name"`, `"Name"`), "hello.host.name"},
		{"host name is too long", replace(helloLine, "hrn", strings.Repeat("a", 65)), "hello.host.name"},
		{"host version has an invalid character", replace(helloLine, "0.2.0", "0/2"), "hello.host.version"},
		{"host version is too long", replace(helloLine, "0.2.0", strings.Repeat("1", 65)), "hello.host.version"},
		{"protocol list is empty", replace(helloLine, `["v1"]`, "[]"), "hello.protocols"},
		{"protocol list is too long", replace(helloLine, `["v1"]`, repeatedJSON(`"v1"`, 17)), "hello.protocols"},
		{"protocol entry is empty", replace(helloLine, "v1", ""), "hello.protocols"},
		{"protocol entry has an invalid character", replace(helloLine, "v1", "V1"), "hello.protocols"},
		{"protocol entry is too long", replace(helloLine, "v1", strings.Repeat("v", 17)), "hello.protocols"},
		{"line limit is zero", appendFields(helloLine, `"limits":{"max_line":0}`), "hello.limits.max_line"},
		{"line limit is negative", appendFields(helloLine, `"limits":{"max_line":-1}`), "hello.limits.max_line"},
		{"line limit is null", appendFields(helloLine, `"limits":{"max_line":null}`), "hello.limits.max_line"},
		{"call capability has an invalid character", replace(callLine, "search", "a_b"), "call.capability"},
		{"call id has an invalid character", replace(callLine, "c1", "c/1"), "call.id"},
		{"cancel id is too long", replace(cancelLine, "c1", strings.Repeat("a", 129)), "cancel.id"},
		{"call input is an array", appendFields(callLine, `"input":[]`), "call.input"},
		{"call timeout is zero", appendFields(callLine, `"timeout_ms":0`), "call.timeout_ms"},
		{"call timeout is negative", appendFields(callLine, `"timeout_ms":-1`), "call.timeout_ms"},
		{"call timeout is null", appendFields(callLine, `"timeout_ms":null`), "call.timeout_ms"},
	})
}

func TestPluginParserRejectsInvalidFields(t *testing.T) {
	rejectLines(t, ParsePluginLine, []invalidLine{
		{"version is not one", replace(readyLine, `"v":1`, `"v":2`), "ready.v"},
		{"sequence is zero", replace(readyLine, `"seq":1`, `"seq":0`), "ready.seq"},
		{"ready protocol is empty", replace(readyLine, "v1", ""), "ready.protocol"},
		{"ready protocol has an invalid character", replace(readyLine, "v1", "v_1"), "ready.protocol"},
		{"plugin name has an invalid character", replace(readyLine, "search", "Search"), "ready.plugin.name"},
		{"plugin version is empty", replace(readyLine, "0.1.0", ""), "ready.plugin.version"},
		{"capability list is empty", replace(readyLine, `[{"name":"search"}]`, "[]"), "ready.capabilities"},
		{"capability name is empty", replace(readyLine, `[{"name":"search"}]`, `[{"name":""}]`), "ready.capabilities[0].name"},
		{"capability names repeat", replace(readyLine, `[{"name":"search"}]`, repeatedJSON(`{"name":"search"}`, 2)), "ready.capabilities"},
		{"capability list is too long", replace(readyLine, `[{"name":"search"}]`, repeatedJSON(`{"name":"search"}`, 65)), "ready.capabilities"},
		{"capability description is too long", replace(readyLine, `[{"name":"search"}]`, `[{"name":"search","description":"`+strings.Repeat("a", 257)+`"}]`), "ready.capabilities[0].description"},
		{"event id is empty", replace(eventLine, "c1", ""), "event.id"},
		{"event kind is unknown", replace(eventLine, "info", "debug"), "event.kind"},
		{"event text is missing", replace(eventLine, `,"text":""`, ""), "event.text"},
		{"event text is too long", replace(eventLine, `""`, `"`+strings.Repeat("a", MaxEventText+1)+`"`), "event.text"},
		{"result id has an invalid character", replace(resultLine, "c1", "c+1"), "result.id"},
		{"result status is unknown", replace(resultLine, "ok", "success"), "result.status"},
		{"result output is a scalar", appendFields(resultLine, `"output":true`), "result.output"},
		{"failed result has no error", failedLine, "result.error"},
		{"successful result has an error", appendFields(resultLine, `"error":{"code":"internal","message":"failed"}`), "result.error"},
		{"canceled result has an error", appendFields(replace(resultLine, "ok", "canceled"), `"error":{"code":"internal","message":"failed"}`), "result.error"},
		{"result problem has an unknown code", appendFields(failedLine, `"error":{"code":"unknown","message":"failed"}`), "result.error.code"},
		{"result problem has an empty message", appendFields(failedLine, `"error":{"code":"internal","message":""}`), "result.error.message"},
		{"evidence list is too long", appendFields(resultLine, `"evidence":`+repeatedJSON("{}", 65)), "result.evidence"},
		{"evidence hash is too short", appendFields(resultLine, `"evidence":[{"hash":"`+strings.Repeat("a", 63)+`"}]`), "result.evidence[0].hash"},
		{"evidence hash has an uppercase letter", appendFields(resultLine, `"evidence":[{"hash":"`+strings.Repeat("a", 63)+`A"}]`), "result.evidence[0].hash"},
		{"evidence excerpt is too long", appendFields(resultLine, `"evidence":[{"excerpt":"`+strings.Repeat("a", MaxExcerpt+1)+`"}]`), "result.evidence[0].excerpt"},
		{"error id is empty", appendFields(errorLine, `"id":""`), "error.id"},
		{"error code is unknown", replace(errorLine, "bad_request", "unknown"), "error.code"},
		{"error message is empty", replace(errorLine, "bad input", ""), "error.message"},
		{"error protocols are missing on refusal", replace(errorLine, "bad_request", "unsupported_protocol"), "error.protocols"},
		{"error protocols are empty", appendFields(errorLine, `"protocols":[]`), "error.protocols"},
		{"error protocols contain a bad entry", appendFields(errorLine, `"protocols":["v/1"]`), "error.protocols"},
	})
}

func rejectLines(t *testing.T, parse parser, cases []invalidLine) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if value := recover(); value != nil {
					t.Fatalf("parser panicked: %v", value)
				}
			}()
			if _, err := parse([]byte(tc.line)); err == nil || !strings.HasPrefix(err.Error(), tc.field+":") {
				t.Fatalf("got error %v, want an error naming %s", err, tc.field)
			}
		})
	}
}

func TestParsersAcceptTheExactLimits(t *testing.T) {
	capabilities := make([]string, 64)
	for i := range capabilities {
		capabilities[i] = `{"name":"cap-` + strconv.Itoa(i) + `"}`
	}
	for _, tc := range []struct {
		name  string
		parse parser
		line  string
	}{
		{"host line size", ParseHostLine, paddedLine(callLine, MaxLine) + "\r\n"},
		{"plugin line size", ParsePluginLine, paddedLine(readyLine, MaxLine)},
		{"protocol count", ParseHostLine, replace(helloLine, `["v1"]`, repeatedJSON(`"v1"`, 16))},
		{"capability count", ParsePluginLine, replace(readyLine, `[{"name":"search"}]`, "["+strings.Join(capabilities, ",")+"]")},
		{"event text bytes", ParsePluginLine, replace(eventLine, `""`, `"`+strings.Repeat("\u00e9", MaxEventText/2)+`"`)},
		{"evidence count", ParsePluginLine, appendFields(resultLine, `"evidence":`+repeatedJSON("{}", 64))},
		{"excerpt bytes", ParsePluginLine, appendFields(resultLine, `"evidence":[{"excerpt":"`+strings.Repeat("\u00e9", MaxExcerpt/2)+`"}]`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.parse([]byte(tc.line)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestParsersStripOneTrailingLineEnding(t *testing.T) {
	for _, tc := range []struct {
		parse parser
		line  string
	}{
		{ParseHostLine, callLine},
		{ParsePluginLine, eventLine},
	} {
		for _, ending := range []string{"\n", "\r\n"} {
			if _, err := tc.parse([]byte(tc.line + ending)); err != nil {
				t.Fatalf("line ending %q: %v", ending, err)
			}
		}
	}
}

func TestNegotiateUsesTheHostsOrder(t *testing.T) {
	got, ok := Negotiate([]string{"v3", "v2", V1}, []string{V1, "v2"})
	if !ok || got != "v2" {
		t.Fatalf("got (%q, %t), want (v2, true)", got, ok)
	}
}

func TestNegotiateReportsNoMatchForDisjointOrEmptyLists(t *testing.T) {
	for _, tc := range []struct{ offered, supported []string }{
		{[]string{"v2"}, []string{V1}},
		{nil, []string{V1}},
		{[]string{V1}, nil},
		{nil, nil},
	} {
		if got, ok := Negotiate(tc.offered, tc.supported); got != "" || ok {
			t.Fatalf("got (%q, %t) for %v and %v, want an empty version and false", got, ok, tc.offered, tc.supported)
		}
	}
}

func appendFields(line, fields string) string {
	return strings.TrimSuffix(line, "}") + "," + fields + "}"
}

func replace(line, old, new string) string {
	return strings.Replace(line, old, new, 1)
}

func repeatedJSON(value string, count int) string {
	return "[" + strings.Repeat(value+",", count-1) + value + "]"
}

func paddedLine(line string, size int) string {
	return line + strings.Repeat(" ", size-len(line))
}
