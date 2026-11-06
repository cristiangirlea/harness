//go:build windows

package tools

import "testing"

func TestFileToolsRefuseAParentJunctionSwapOutsideTheWorkspace(t *testing.T) {
	fileToolsRefuseParentSwap(t, junction)
}

func TestWriteFileRefusesAParentJunctionSwapBeforeCreatingDirectories(t *testing.T) {
	writeFileRefusesParentSwapBeforeCreatingDirectories(t, junction)
}
