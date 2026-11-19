//go:build !windows

package prefs

func systemOrgPath() (string, error) { return "/etc/hrn/backends.json", nil }
