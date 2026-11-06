package tools

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBoundedBufferCutsOutputExactlyLikeTruncate(t *testing.T) {
	inputs := []string{
		strings.Repeat("ăîșț€😀", 4000),
		"x" + strings.Repeat("😀", 9000),
		strings.Repeat("ab", 10000) + "\x80\x80\x80" + strings.Repeat("€", 5000),
		strings.Repeat("y", 19999) + "😀" + strings.Repeat("z", 9998) + "€" + "w",
		strings.Repeat("é", 14999) + "\xff" + strings.Repeat("ș", 6000),
		strings.Repeat("plain", 3000),
		strings.Repeat("plain", 7000),
	}
	for i, input := range inputs {
		for _, chunk := range []int{1, 3, 7, 4096, len(input)} {
			t.Run(fmt.Sprintf("input%d/chunk%d", i, chunk), func(t *testing.T) {
				b := &boundedBuffer{keepHead: maxOutput * 2 / 3, keepTail: maxOutput / 3}
				for rest := input; len(rest) > 0; {
					n := min(chunk, len(rest))
					if _, err := b.Write([]byte(rest[:n])); err != nil {
						t.Fatal(err)
					}
					rest = rest[n:]
				}
				got, want := b.String(), Truncate(input)
				if got != want {
					t.Fatalf("buffer output differs from Truncate: got %d bytes, want %d", len(got), len(want))
				}
				if utf8.ValidString(input) && !utf8.ValidString(got) {
					t.Fatal("valid input was cut inside a character")
				}
			})
		}
	}
}
