//go:build !windows

package nowindow

import "os/exec"

// Set does nothing here: only Windows gives a child a console window of its own.
func Set(*exec.Cmd) {}
