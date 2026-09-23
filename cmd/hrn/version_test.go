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
