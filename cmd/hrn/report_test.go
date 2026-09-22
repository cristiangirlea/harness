package main

import "testing"

func TestTableAlignsColumns(t *testing.T) {
	got := table([][]string{{"key", "n"}, {"a", "10"}, {"longer", "5"}}, 1)
	want := "key      n\na       10\nlonger   5\n"
	if got != want {
		t.Errorf("table =\n%q\nwant\n%q", got, want)
	}
	// A left-aligned last column carries no trailing spaces.
	if got := table([][]string{{"k", "label"}, {"kk", "x"}}, 2); got != "k   label\nkk  x\n" {
		t.Errorf("left-aligned table = %q", got)
	}
}

func TestShortKeyKeepsTheTail(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"abc", 4, "abc"},
		{"abcd", 4, "abcd"},
		{"abcdef", 4, "…def"},
		{"héllo wörld", 6, "…wörld"},
	} {
		if got := shortKey(tc.in, tc.n); got != tc.want {
			t.Errorf("shortKey(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}
