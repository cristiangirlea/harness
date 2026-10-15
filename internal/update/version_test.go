package update

import (
	"strings"
	"testing"
)

func TestParseVersionLineReadsWhatHrnVersionPrints(t *testing.T) {
	for _, tc := range []struct {
		line     string
		release  string
		module   string
		commit   string
		modified bool
	}{
		{"hrn 0.2.0 (go1.26.4, a0e5d2f12345, modified)", "0.2.0", "", "a0e5d2f12345", true},
		{"hrn 0.2.0 (go1.26.4, a0e5d2f12345)", "0.2.0", "", "a0e5d2f12345", false},
		{"hrn 0.2.0 (go1.26.4, v0.1.0)", "0.2.0", "v0.1.0", "", false},
		{"hrn 0.2.0 (go1.26.4, v0.0.0-20260923070935-6fe88a3546b8)", "0.2.0", "v0.0.0-20260923070935-6fe88a3546b8", "6fe88a3546b8", false},
		{"hrn 0.2.0 (go1.26.4, v0.0.0-20260923070935-6fe88a3546b8+dirty)", "0.2.0", "v0.0.0-20260923070935-6fe88a3546b8+dirty", "6fe88a3546b8", true},
		{"hrn 0.2.0 (go1.26.4)", "0.2.0", "", "", false},
		{"hrn 0.2.0 (go1.26.4, a0e5d2f12345)\nmore output\n", "0.2.0", "", "a0e5d2f12345", false},
	} {
		v, err := ParseVersionLine(tc.line)
		if err != nil {
			t.Errorf("%q: %v", tc.line, err)
			continue
		}
		if v.Release != tc.release || v.Module != tc.module || v.Commit != tc.commit || v.Modified != tc.modified {
			t.Errorf("%q: got release %q module %q commit %q modified %v; want %q %q %q %v",
				tc.line, v.Release, v.Module, v.Commit, v.Modified, tc.release, tc.module, tc.commit, tc.modified)
		}
		if v.Line != strings.SplitN(strings.TrimSpace(tc.line), "\n", 2)[0] {
			t.Errorf("%q: Line = %q; want the first line", tc.line, v.Line)
		}
	}
}

func TestParseVersionLineRefusesWhatIsNotHrn(t *testing.T) {
	for _, line := range []string{"", "   \n", "git version 2.47.0", "hrn: unknown command", "hrn 0.2.0", "hrn 0.2.0 (go1.26.4"} {
		if _, err := ParseVersionLine(line); err == nil {
			t.Errorf("%q parsed; want an error", line)
		}
	}
}

func TestSameBuildComparesTheCommitOrTheModuleVersion(t *testing.T) {
	parse := func(s string) Version {
		v, err := ParseVersionLine(s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, tc := range []struct {
		a, b string
		same bool
	}{
		{"hrn 0.2.0 (go1.26.4, a0e5d2f12345)", "hrn 0.2.0 (go1.26.5, a0e5d2f12345)", true},
		{"hrn 0.2.0 (go1.26.4, a0e5d2f12345)", "hrn 0.2.0 (go1.26.4, b1f6e3a23456)", false},
		{"hrn 0.2.0 (go1.26.4, v0.2.0)", "hrn 0.2.0 (go1.26.4, v0.2.0)", true},
		{"hrn 0.2.0 (go1.26.4, v0.1.0)", "hrn 0.2.0 (go1.26.4, v0.2.0)", false},
		// A pseudo-version names the commit a checkout build was stamped with.
		{"hrn 0.2.0 (go1.26.4, v0.0.0-20260923070935-6fe88a3546b8)", "hrn 0.2.0 (go1.26.4, 6fe88a3546b8)", true},
		// The same commit with and without edits is not the same build.
		{"hrn 0.2.0 (go1.26.4, a0e5d2f12345, modified)", "hrn 0.2.0 (go1.26.4, a0e5d2f12345)", false},
		// Two builds without any stamp: only the whole line can tell.
		{"hrn 0.2.0 (go1.26.4)", "hrn 0.2.0 (go1.26.4)", true},
		{"hrn 0.2.0 (go1.26.4)", "hrn 0.2.1 (go1.26.4)", false},
		{"hrn 0.2.0 (go1.26.4)", "hrn 0.2.0 (go1.26.4, a0e5d2f12345)", false},
	} {
		if got := parse(tc.a).Same(parse(tc.b)); got != tc.same {
			t.Errorf("Same(%q, %q) = %v; want %v", tc.a, tc.b, got, tc.same)
		}
	}
}

func TestShortNamesTheBuildForABackupFile(t *testing.T) {
	for _, tc := range []struct{ line, want string }{
		{"hrn 0.2.0 (go1.26.4, a0e5d2f12345, modified)", "a0e5d2f12345"},
		{"hrn 0.2.0 (go1.26.4, v0.1.0)", "v0.1.0"},
		{"hrn 0.2.0 (go1.26.4, v0.0.0-20260923070935-6fe88a3546b8+dirty)", "6fe88a3546b8"},
		{"hrn 0.2.0 (go1.26.4)", "0.2.0"},
	} {
		v, err := ParseVersionLine(tc.line)
		if err != nil {
			t.Fatal(err)
		}
		if got := v.Short(); got != tc.want {
			t.Errorf("%q: Short() = %q; want %q", tc.line, got, tc.want)
		}
	}
	if got := (Version{}).Short(); got != "unknown" {
		t.Errorf("an empty version is %q; want unknown", got)
	}
}
