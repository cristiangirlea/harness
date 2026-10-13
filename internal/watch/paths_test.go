package watch

import "testing"

var testSystem = []string{`C:\Windows\System32`, `C:\Program Files`, `C:\Program Files (x86)`, "/usr/bin", "/bin", "/usr/local/bin"}

func TestPathsInAWindowsRunAreShortenedToTheWorkspaceHomeAndProgramNames(t *testing.T) {
	sh := newShortener(`E:\__Workdir\personal\ada-os-ipc`, `C:\Users\crist`, testSystem)
	for _, tc := range []struct{ in, want string }{
		{`codex.exe exec --json --skip-git-repo-check -C E:\__Workdir\personal\ada-os-ipc -c model_reasoning_effort=high --sandbox workspace-write`,
			`codex.exe exec --json --skip-git-repo-check -C . -c model_reasoning_effort=high --sandbox workspace-write`},
		{`E:\__Workdir\personal\ada-os-ipc`, `.`},
		{`E:\__Workdir\personal\ada-os-ipc\`, `.\`},
		{`E:\__Workdir\personal\ada-os-ipc\src\main.go:12`, `.\src\main.go:12`},
		{`e:/__workdir/Personal/ADA-os-ipc/src/main.go`, `./src/main.go`},
		{`cd 'E:\__Workdir\personal\ada-os-ipc'; ls`, `cd '.'; ls`},
		{`{"file_path":"E:\\__Workdir\\personal\\ada-os-ipc\\src\\a.go"}`, `{"file_path":".\\src\\a.go"}`},
		{`{"cwd":"E:\\__Workdir\\personal\\ada-os-ipc"}`, `{"cwd":"."}`},
		// Only at a path boundary.
		{`E:\__Workdir\personal\ada-os-ipc-old\x`, `E:\__Workdir\personal\ada-os-ipc-old\x`},
		{`E:\__Workdir\personal\ada-os-ipc2`, `E:\__Workdir\personal\ada-os-ipc2`},
		{`E:\__Workdir\personal\ada-os-ipc.bak`, `E:\__Workdir\personal\ada-os-ipc.bak`},
		{`D:\copy\E:\__Workdir\personal\ada-os-ipc`, `D:\copy\E:\__Workdir\personal\ada-os-ipc`},
		// The home folder.
		{`C:\Users\crist\.codex\config.toml`, `~\.codex\config.toml`},
		{`c:/users/CRIST/.claude`, `~/.claude`},
		{`C:\Users\crist2\x`, `C:\Users\crist2\x`},
		// Programs in system folders, quoted, JSON-escaped or bare.
		{`shell "C:\\WINDOWS\\System32\\WindowsPowerShell\\v1.0\\powershell.exe" -Command 'Get-ChildItem'`,
			`shell "powershell.exe" -Command 'Get-ChildItem'`},
		{`C:\Windows\system32\cmd.exe /c dir`, `cmd.exe /c dir`},
		{`"C:\Program Files\Git\bin\bash.exe" -lc "ls"`, `"bash.exe" -lc "ls"`},
		{`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe --headless`, `msedge.exe --headless`},
		{`{"command":"\"C:\\Program Files\\Git\\bin\\bash.exe\" -lc ls"}`, `{"command":"\"bash.exe\" -lc ls"}`},
		{`{"command":"C:\\Windows\\System32\\cmd.exe /c dir C:\\x"}`, `{"command":"cmd.exe /c dir C:\\x"}`},
		{`"C:\Program Files\Microsoft VS Code\Code.exe" .`, `"Code.exe" .`},
		// In quotes a folder's name may hold punctuation; bare, it ends the path.
		{`"C:\Program Files\Git (Preview)\bin\bash.exe" -lc ls`, `"bash.exe" -lc ls`},
		{`{"command":"\"C:\\Program Files\\Git (Preview)\\bin\\bash.exe\" -lc ls"}`, `{"command":"\"bash.exe\" -lc ls"}`},
		{`'C:\Program Files\Tools [x64]\rg.exe' -n x`, `'rg.exe' -n x`},
		{`run(C:\Windows\System32\where.exe,git)`, `run(where.exe,git)`},
		// A folder, not a program, and a program outside them, stay.
		{`C:\Windows\System32`, `C:\Windows\System32`},
		{`C:\Program Files\Go`, `C:\Program Files\Go`},
		{`D:\tools\rg.exe -n x`, `D:\tools\rg.exe -n x`},
		{`C:\Program FilesX\a.exe`, `C:\Program FilesX\a.exe`},
		{"", ""},
		{"no paths here", "no paths here"},
	} {
		if got := sh.shorten(tc.in); got != tc.want {
			t.Errorf("shorten(%q)\n got %q\nwant %q", tc.in, got, tc.want)
		}
	}
}

func TestPathsInAUnixRunAreShortenedToTheWorkspaceHomeAndProgramNames(t *testing.T) {
	sh := newShortener("/home/ana/src/hrn", "/home/ana", testSystem)
	for _, tc := range []struct{ in, want string }{
		{"cd /home/ana/src/hrn && /usr/bin/git status", "cd . && git status"},
		{"/home/ana/src/hrn/internal/x.go:12: oops", "./internal/x.go:12: oops"},
		{`{"path":"/home/ana/src/hrn/a b.go"}`, `{"path":"./a b.go"}`},
		{"/home/ana/.config/hrn", "~/.config/hrn"},
		{"/home/ana/src/other", "~/src/other"},
		{"/HOME/ana/src/hrn", "/HOME/ana/src/hrn"}, // case matters here
		{"/home/ana/src/hrnx", "~/src/hrnx"},
		{"/home/anabel/x", "/home/anabel/x"},
		{"/opt/home/ana/x", "/opt/home/ana/x"},
		{"/usr/local/bin/node script.js", "node script.js"},
		{`exec "/bin/sh" -c true`, `exec "sh" -c true`},
		{"$(/usr/bin/env)", "$(env)"},
		{"/usr/bin", "/usr/bin"},
		{"/usr/binx/tool", "/usr/binx/tool"},
	} {
		if got := sh.shorten(tc.in); got != tc.want {
			t.Errorf("shorten(%q)\n got %q\nwant %q", tc.in, got, tc.want)
		}
	}
}

func TestAWorkspaceInsideHomeIsShortenedAsTheWorkspace(t *testing.T) {
	sh := newShortener(`C:\Users\crist\src\app`, `C:\Users\crist`, nil)
	for in, want := range map[string]string{
		`C:\Users\crist\src\app\main.go`: `.\main.go`,
		`C:\Users\crist\src\app`:         `.`,
		`C:\Users\crist\src\other`:       `~\src\other`,
	} {
		if got := sh.shorten(in); got != want {
			t.Errorf("shorten(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNoWorkspaceOrARootFolderShortensNothingButTheRest(t *testing.T) {
	for _, ws := range []string{"", "/", `C:\`, "C:"} {
		sh := newShortener(ws, "/home/ana", nil)
		if got := sh.shorten(`/x C:\y /home/ana/z`); got != `/x C:\y ~/z` {
			t.Errorf("workspace %q: got %q", ws, got)
		}
	}
}
