package tools

import "testing"

func TestOneLine(t *testing.T) {
	if got := OneLine("  a\n  b\t c  ", 100); got != "a b c" {
		t.Errorf("OneLine = %q", got)
	}
	if got := OneLine("abcdef", 3); got != "abc…" {
		t.Errorf("OneLine truncation = %q", got)
	}
}
