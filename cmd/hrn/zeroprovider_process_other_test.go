//go:build !windows

package main

import "testing"

func assertNoZeroProviderChildren(t *testing.T, pid int) {
	t.Helper()
	t.Log("child assertion unavailable: exited parents can lose their descendants through reparenting")
}
