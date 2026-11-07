//go:build windows

package tools

import "testing"

func TestFileToolsRefuseAParentJunctionSwapOutsideTheWorkspace(t *testing.T) {
	fileToolsRefuseParentSwap(t, junction)
}

func TestWriteFileRefusesAParentJunctionSwapBeforeCreatingDirectories(t *testing.T) {
	writeFileRefusesParentSwapBeforeCreatingDirectories(t, junction)
}

func TestFileToolsRefuseASecretJunctionSwappedInInsideTheWorkspace(t *testing.T) {
	fileToolsRefuseASecretSwappedInInsideTheWorkspace(t, junction)
}

func TestOpenInsideRefusesASecretReachedThroughAJunction(t *testing.T) {
	openInsideRefusesASecretReachedThroughALink(t, junction)
}

func TestFileToolsRefuseASecretSymlinkSwappedInInsideTheWorkspace(t *testing.T) {
	fileToolsRefuseASecretSwappedInInsideTheWorkspace(t, relativeSymlink)
}

func TestOpenInsideRefusesASecretReachedThroughASymlink(t *testing.T) {
	openInsideRefusesASecretReachedThroughALink(t, relativeSymlink)
}
