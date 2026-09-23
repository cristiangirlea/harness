package main

import (
	"runtime/debug"
	"strings"
	"testing"
)

func TestVersionLineSaysWhichBuildThisIs(t *testing.T) {
	checkout := &debug.BuildInfo{GoVersion: "go1.26.4", Main: debug.Module{Version: "(devel)"}, Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "a0e5d2f1234567890abcdef"},
		{Key: "vcs.modified", Value: "true"},
	}}
	clean := &debug.BuildInfo{GoVersion: "go1.26.4", Main: debug.Module{Version: "(devel)"}, Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "a0e5d2f1234567890abcdef"},
		{Key: "vcs.modified", Value: "false"},
	}}
	installed := &debug.BuildInfo{GoVersion: "go1.26.4", Main: debug.Module{Version: "v0.1.0"}}

	for _, tc := range []struct {
		name string
		info *debug.BuildInfo
		want []string
		not  []string
	}{
		{"a checkout with edits", checkout, []string{"hrn ", "go1.26.4", "a0e5d2f1234", "modified"}, []string{"a0e5d2f1234567890"}},
		{"a clean checkout", clean, []string{"a0e5d2f1234"}, []string{"modified"}},
		{"go install at a tag", installed, []string{"v0.1.0", "go1.26.4"}, []string{"modified"}},
		{"no build info", nil, []string{"hrn "}, []string{"modified"}},
	} {
		got := versionLine(tc.info)
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %q lacks %q", tc.name, got, w)
			}
		}
		for _, n := range tc.not {
			if strings.Contains(got, n) {
				t.Errorf("%s: %q should not contain %q", tc.name, got, n)
			}
		}
	}
}

func TestVersionLineNamesTheCommitOnce(t *testing.T) {
	// Go 1.24+ builds from a checkout carry a pseudo-version that already ends
	// in the commit.
	info := &debug.BuildInfo{GoVersion: "go1.26.4", Main: debug.Module{Version: "v0.0.0-20260923070935-6fe88a3546b8"}, Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "6fe88a3546b8f00dfeed"},
		{Key: "vcs.modified", Value: "false"},
	}}
	if got := versionLine(info); strings.Count(got, "6fe88a3546b8") != 1 {
		t.Errorf("%q names the commit more than once", got)
	}
	dirty := &debug.BuildInfo{GoVersion: "go1.26.4", Main: debug.Module{Version: "v0.0.0-20260923070935-6fe88a3546b8+dirty"}, Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "6fe88a3546b8f00dfeed"},
		{Key: "vcs.modified", Value: "true"},
	}}
	if got := versionLine(dirty); strings.Count(got, "6fe88a3546b8") != 1 || strings.Contains(got, "modified") {
		t.Errorf("%q: the +dirty suffix already says it", got)
	}
}
