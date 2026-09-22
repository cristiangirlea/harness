package mcp

import (
	"bytes"
	"strings"
	"testing"
)

// FuzzCodec feeds arbitrary frames to the reader. A peer that sends nonsense
// must get a parse error, not take the process down.
func FuzzCodec(f *testing.F) {
	f.Add(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")
	f.Add(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n")
	f.Add("not json\n")
	f.Add("{\n")
	f.Add("")

	f.Fuzz(func(t *testing.T, frames string) {
		c := newCodec(strings.NewReader(frames), &bytes.Buffer{})
		for i := 0; i < 100; i++ {
			m, err := c.read()
			if err != nil {
				return
			}
			// Whatever came back, the classifiers must agree on exactly one shape.
			n := 0
			for _, is := range []bool{m.isRequest(), m.isNotification(), m.isResponse()} {
				if is {
					n++
				}
			}
			if n > 1 {
				t.Fatalf("message is more than one kind: %+v", m)
			}
		}
	})
}
