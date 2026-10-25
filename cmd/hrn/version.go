package main

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/cristiangirlea/harness/internal/agent"
)

func versionLine(info *debug.BuildInfo) string {
	parts := []string{runtime.Version()}
	if info != nil {
		if info.GoVersion != "" {
			parts[0] = info.GoVersion
		}
		if v := info.Main.Version; v != "" && v != "(devel)" {
			parts = append(parts, v)
		}
		var rev string
		var modified bool
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				modified = s.Value == "true"
			}
		}
		if len(rev) > 12 {
			rev = rev[:12]
		}
		// Checkout pseudo-versions already include commit and dirty state; avoid duplicating them.
		named := rev != "" && strings.Contains(info.Main.Version, rev)
		if rev != "" && !named {
			parts = append(parts, rev)
		}
		if modified && !strings.Contains(info.Main.Version, "+dirty") {
			parts = append(parts, "modified")
		}
	}
	return fmt.Sprintf("hrn %s (%s)", agent.Version, strings.Join(parts, ", "))
}
