package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunCommandShapesLargeOutput(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		ws, tl := setup(t)
		ws.ShapeTools, ws.RunDir = enabled, t.TempDir()
		text := "count 10000\n" + strings.Repeat("working\n", 10000) + "middle preserved\n"
		must(t, os.WriteFile(filepath.Join(ws.Root, "output.txt"), []byte(text), 0o644))
		command := "cat output.txt"
		if shell, _ := Shell(); shell == "powershell" {
			command = "[Console]::Write([IO.File]::ReadAllText('output.txt'))"
		}
		out, err := call(t, ws, tl["run_command"], map[string]any{"command": command})
		must(t, err)
		if enabled {
			if !strings.Contains(out, "original:") || !strings.Contains(out, "[exit code 0]") {
				t.Fatalf("shell shaping missing: returned %d bytes, handle=%v", len(out), strings.Contains(out, "original:"))
			}
			files, err := os.ReadDir(filepath.Join(ws.RunDir, "originals"))
			must(t, err)
			if len(files) != 1 {
				t.Fatalf("originals: %d", len(files))
			}
			data, err := os.ReadFile(filepath.Join(ws.RunDir, "originals", files[0].Name()))
			must(t, err)
			if string(data) != text {
				t.Fatal("shell original cut or changed")
			}
			rangeOut, err := call(t, ws, tl["read_original"], map[string]any{"handle": strings.TrimSuffix(files[0].Name(), ".txt"), "range": "20000-20010"})
			must(t, err)
			if rangeOut != text[20000:20010] {
				t.Fatal("shell range changed")
			}
		} else if out != Truncate(text)+"\n[exit code 0]" {
			t.Fatal("disabled output changed")
		}
	}
}

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
