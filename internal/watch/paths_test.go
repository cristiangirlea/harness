package watch

import "testing"

var testSystem = []string{`C:\Windows\System32`, `C:\Program Files`, `C:\Program Files (x86)`, "/usr/bin", "/bin", "/usr/local/bin"}

func TestPathsInAWindowsRunAreShortenedToTheWorkspaceHomeAndProgramNames(t *testing.T) {
	sh := newShortener(`D:\src\demo-app`, `C:\Users\dev`, testSystem)
	for _, tc := range []struct{ in, want string }{
		{`codex.exe exec --json --skip-git-repo-check -C D:\src\demo-app -c model_reasoning_effort=high --sandbox workspace-write`,
			`codex.exe exec --json --skip-git-repo-check -C . -c model_reasoning_effort=high --sandbox workspace-write`},
		{`D:\src\demo-app`, `.`},
		{`D:\src\demo-app\`, `.\`},
		{`D:\src\demo-app\src\main.go:12`, `.\src\main.go:12`},
		{`d:/SRC/Demo-APP/src/main.go`, `./src/main.go`},
		{`cd 'D:\src\demo-app'; ls`, `cd '.'; ls`},
		{`{"file_path":"D:\\src\\demo-app\\src\\a.go"}`, `{"file_path":".\\src\\a.go"}`},
		{`{"cwd":"D:\\src\\demo-app"}`, `{"cwd":"."}`},
		// Only at a path boundary.
		{`D:\src\demo-app-old\x`, `D:\src\demo-app-old\x`},
		{`D:\src\demo-app2`, `D:\src\demo-app2`},
		{`D:\src\demo-app.bak`, `D:\src\demo-app.bak`},
		{`E:\copy\D:\src\demo-app`, `E:\copy\D:\src\demo-app`},
		// The home folder.
		{`C:\Users\dev\.codex\config.toml`, `~\.codex\config.toml`},
		{`c:/users/DEV/.claude`, `~/.claude`},
		{`C:\Users\dev2\x`, `C:\Users\dev2\x`},
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
		// Only the matching quote ends a quoted path.
		{`"C:\Program Files\Bob's Tools\tool.exe" --help`, `"tool.exe" --help`},
		{`{"command":"\"C:\\Program Files\\Bob's Tools\\tool.exe\" --help"}`, `{"command":"\"tool.exe\" --help"}`},
		// Double quotes cannot occur in Windows folder names; single quoting stays the same.
		{`'C:\Program Files\Other Tools\tool.exe' --help`, `'tool.exe' --help`},
		{`{"command":"'C:\\Program Files\\Other Tools\\tool.exe' --help"}`, `{"command":"'tool.exe' --help"}`},
		{`C:\Program Files\Bob's Tools\tool.exe --help`, `C:\Program Files\Bob's Tools\tool.exe --help`},
		// Unterminated quotes stop at the input's end or a line end.
		{`"C:\Program Files\Bob's Tools\tool.exe`, `"tool.exe`},
		{"\"C:\\Program Files\\Bob's Tools\nnext", "\"C:\\Program Files\\Bob's Tools\nnext"},
		// An unterminated single quote in JSON text stops at the string's end, keeping the next field.
		{`{"command":"'C:\\Windows\\System32\\cmd.exe","cwd":"D:\\work.dir"}`, `{"command":"'cmd.exe","cwd":"D:\\work.dir"}`},
		// Two backslashes before the quote are an escaped backslash: the quote still ends the string.
		{`["'C:\\Windows\\System32\\cmd.exe\\",1.5]`, `["'cmd.exe\\",1.5]`},
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
		// A bare double quote ends a single-quoted path even so: in JSON text it closes the
		// string. Escaped, as JSON writes one inside a string, it is part of the path.
		{`{"command":"'/usr/bin/Bob\"s/tool' --help"}`, `{"command":"'tool' --help"}`},
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
	sh := newShortener(`C:\Users\dev\src\app`, `C:\Users\dev`, nil)
	for in, want := range map[string]string{
		`C:\Users\dev\src\app\main.go`: `.\main.go`,
		`C:\Users\dev\src\app`:         `.`,
		`C:\Users\dev\src\other`:       `~\src\other`,
	} {
		if got := sh.shorten(in); got != want {
			t.Errorf("shorten(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAHomeInsideTheWorkspaceIsShortenedAsTheHome(t *testing.T) {
	sh := newShortener("/srv/box", "/srv/box/home/ana", nil)
	for in, want := range map[string]string{
		"/srv/box/home/ana/.config": "~/.config",
		"/srv/box/home/ana":         "~",
		"/srv/box/home/bob":         "./home/bob",
		"/srv/box":                  ".",
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
