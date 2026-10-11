package tools

import "testing"

func TestWSLsLauncherIsRecognisedHoweverSystem32IsSpelt(t *testing.T) {
	for path, want := range map[string]bool{
		`C:\Windows\System32\bash.exe`:          true,
		`C:\WINDOWS\system32\bash.exe`:          true, // how PATH often spells it
		`c:/windows/SYSTEM32/bash.exe`:          true,
		`C:\Program Files\Git\usr\bin\bash.exe`: false,
		`C:\Program Files\Git\bin\bash.exe`:     false,
		`C:\tools\system32x\bash.exe`:           false,
		`bash.exe`:                              false,
	} {
		if got := inSystem32(path); got != want {
			t.Errorf("inSystem32(%q) = %v, want %v", path, got, want)
		}
	}
}
