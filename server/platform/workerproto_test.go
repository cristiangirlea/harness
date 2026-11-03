package platform

import (
	"bytes"
	"strings"
	"testing"
)

type closeBuffer struct {
	bytes.Buffer
	closed bool
}

func (b *closeBuffer) Close() error { b.closed = true; return nil }
func TestCancelBeforeStart(t *testing.T) {
	out := &closeBuffer{}
	c := workerClient{input: strings.NewReader(`{"v":1,"seq":1,"type":"hello","protocol":"v1"}` + "\n"), output: out, attemptID: "a", start: []byte(`{"type":"start"}`)}
	if err := c.cancel(); err != nil {
		t.Fatal(err)
	}
	if !out.closed || out.Len() != 0 {
		t.Fatalf("cancel wrote before start: %q closed=%v", out.String(), out.closed)
	}
	_, _ = c.run()
	if out.Len() != 0 {
		t.Fatalf("start written after cancellation: %q", out.String())
	}
}

func TestStderrTailBounded(t *testing.T) {
	var b tailBuffer
	_, _ = b.Write([]byte(strings.Repeat("x", 5000)))
	_, _ = b.Write([]byte("tail"))
	if len(b.data) != 4096 || !strings.HasSuffix(string(b.data), "tail") {
		t.Fatalf("tail length %d", len(b.data))
	}
}
