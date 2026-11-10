package batch

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/flow"
	"github.com/cristiangirlea/harness/internal/tools"
)

const defaultGateTimeout = 15 * time.Minute

const maxGateTimeout = 24 * time.Hour

// Refresh records so silent gates do not appear stopped.
var gateBeat = 10 * time.Minute

func gateEvent(id string, attempt, gate int, parent string, gr GateResult) flow.Event {
	status := flow.StatusOK
	reason := ""
	if gr.ExitCode != 0 {
		status, reason = flow.StatusFailed, gr.Outcome()
		if gr.Killed == KilledCanceled {
			status = flow.StatusCanceled
		}
	}
	return flow.Event{Step: gateStep(id, attempt, gate, gr), Parent: parent, Kind: flow.KindGate, Task: id, Attempt: attempt,
		Status: status, Name: gr.Command, Reason: reason, Text: lastLine(gr.Output)}
}

// stop waits for beat, preventing later writes.
func beating(every time.Duration, beat func()) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		tick := time.NewTicker(every)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				beat()
			}
		}
	})
	return func() { close(done); wg.Wait() }
}

func checkExpect(re *regexp.Regexp, final string) GateResult {
	gr := GateResult{Command: expectCommand(re.String()), Output: tail(final, 4000)}
	if !re.MatchString(final) {
		gr.ExitCode = 1
		if strings.TrimSpace(final) == "" {
			gr.Output = "(the agent gave no final answer)"
		}
	}
	return gr
}

var runGateShell = tools.RunShell

var cargoIncrementalLine = regexp.MustCompile(`(?m)^\s*incremental\s*=`)

// Package overrides under a dev or test profile also beat its incremental default.
func devOrTestProfile(table string) bool {
	for _, p := range []string{"[profile.dev", "[profile.test"} {
		if table == p+"]" || strings.HasPrefix(table, p+".") {
			return true
		}
	}
	return false
}

func batchEnv(dir string) ([]string, func() error, error) {
	parent := gateTempParent(dir)
	var env []string
	if cargo, ok := os.LookupEnv("CARGO_INCREMENTAL"); ok {
		env = append(env, "CARGO_INCREMENTAL="+cargo)
	} else {
		configured := false
		for _, name := range []string{".cargo/config.toml", ".cargo/config", "Cargo.toml"} {
			data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
			if err != nil {
				continue
			}
			table := ""
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "[") {
					table = ""
					if end := strings.IndexByte(line, ']'); end >= 0 {
						table = strings.Join(strings.Fields(line[:end+1]), "")
					}
					continue
				}
				if (devOrTestProfile(table) || name != "Cargo.toml" && table == "[build]") && cargoIncrementalLine.MatchString(line) {
					configured = true
					break
				}
			}
			if configured {
				break
			}
		}
		if !configured {
			env = append(env, "CARGO_INCREMENTAL=0")
		}
	}
	temp, err := os.MkdirTemp(parent, "tmp-")
	if err != nil && parent != os.TempDir() {
		temp, err = os.MkdirTemp(os.TempDir(), "tmp-")
	}
	if err != nil {
		return nil, nil, err
	}
	env = append(env, "TMP="+temp, "TEMP="+temp, "TMPDIR="+temp)
	return env, func() error {
		err := os.RemoveAll(temp)
		if parent != os.TempDir() && filepath.Dir(temp) == parent && filepath.Base(parent) == ".hrn-gate-tmp" {
			_ = os.Remove(parent)
		}
		return err
	}, nil
}

func gateTempParent(dir string) string {
	tree, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return os.TempDir()
	}
	parent := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(tree))))
	for path := parent; ; path = filepath.Dir(path) {
		_, err := os.Lstat(filepath.Join(path, ".git"))
		if err == nil {
			next := filepath.Dir(path)
			if next == path {
				return os.TempDir()
			}
			parent = next
		} else if !os.IsNotExist(err) {
			return os.TempDir()
		}
		if filepath.Dir(path) == path {
			break
		}
	}
	parent = filepath.Join(parent, ".hrn-gate-tmp")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return os.TempDir()
	}
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || !gateTempPermissionsSafe(info) {
		return os.TempDir()
	}
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil || !sameVolume(resolved, tree) {
		return os.TempDir()
	}
	for path := resolved; ; path = filepath.Dir(path) {
		if _, err := os.Lstat(filepath.Join(path, ".git")); !os.IsNotExist(err) {
			return os.TempDir()
		}
		if filepath.Dir(path) == path {
			return resolved
		}
	}
}

// commandGate is the adanext-exec binary and operator policy gate commands run through; zero runs them directly.
type commandGate struct{ path, policy, runDir string }

func runGate(ctx context.Context, dir, command string, timeout time.Duration, env []string, through commandGate) GateResult {
	started := time.Now()
	// Gates are operator commands: preserve their environment except git repository overrides.
	env = append(os.Environ(), env...)
	var res tools.ShellResult
	var err error
	if through.path != "" {
		res, err = tools.RunGated(ctx, through.path, through.policy, dir, through.runDir, command, timeout, env)
	} else {
		res, err = runGateShell(ctx, dir, command, timeout, env)
	}
	output, decodedUTF16 := decodeGateOutput(res.Output)
	res.Output = output
	gr := GateResult{Command: command, ExitCode: res.ExitCode, Output: tail(res.Output, 4000), Duration: time.Since(started), Gated: through.path != ""}
	switch {
	case err != nil:
		gr.ExitCode = -1
		gr.Output = strings.TrimSpace(gr.Output + "\n" + err.Error())
		if ctx.Err() != nil {
			// Cancellation before shell startup is not an infrastructure failure.
			gr.Killed = KilledCanceled
		} else if errors.Is(err, tools.ErrWait) || gr.Gated {
			// The gate's own errors say whether it could not start or would not run the command.
			gr.InfrastructureError = err.Error()
		} else {
			gr.InfrastructureError = "shell could not start: " + err.Error()
		}
	case gr.ExitCode == -1 && ctx.Err() != nil:
		// Batch cancellation wins over timeout, but not an actual exit.
		gr.Killed = KilledCanceled
		gr.Output = strings.TrimSpace(gr.Output + "\n[canceled]")
	case res.TimedOut:
		gr.Killed, gr.Timeout = KilledTimeout, agent.Duration(timeout)
		gr.Output = strings.TrimSpace(gr.Output + fmt.Sprintf("\n[timed out after %s]", timeout))
	case gr.ExitCode != 0 && decodedUTF16:
		// Only initial launcher diagnostics count; tests may mention them later.
		prefix := res.Output[:min(len(res.Output), 1024)]
		if code := gateHCSError.FindString(prefix); code != "" {
			gr.InfrastructureError = "WSL launcher: " + code
		} else if strings.Contains(prefix, "The Windows Subsystem for Linux") {
			gr.InfrastructureError = "The Windows Subsystem for Linux launcher could not start"
		}
	}
	return gr
}

var gateHCSError = regexp.MustCompile(`\bHCS_E_[A-Z][A-Z0-9_]*\b`)

// WSL can emit UTF-16 to pipes; decode before truncation or launcher detection.
func decodeGateOutput(s string) (string, bool) {
	m := gateTruncation.FindStringSubmatchIndex(s)
	if m == nil {
		out, decoded, _ := decodeGateText(s)
		return out, decoded
	}
	head, headDecoded, order := decodeGateText(s[:m[0]])
	if order != nil {
		// The cut can end the head inside a character.
		head = strings.TrimSuffix(head, "\ufffd")
	}
	head = trimCutRune(head)
	rest := s[m[1]:]
	tail, tailDecoded, _ := decodeGateText(skipCutRune(rest))
	if order != nil {
		// The tail starts at byte len(head)+dropped of the stream, which fixes its code-unit boundary.
		dropped, _ := strconv.Atoi(s[m[2]:m[3]])
		t16, _, _ := decodeGateText(gateBOM(order) + rest[min(len(rest), (m[0]+dropped)%2):])
		// The cut can start the tail inside a surrogate pair.
		t16 = strings.TrimPrefix(t16, "\ufffd")
		if bad8, bad16 := gateBadRunes(tail), gateBadRunes(t16); bad8 > bad16 || bad8 == bad16 && (tailDecoded || !gatePlainUTF8(tail) || gateOneScript(t16)) {
			tail, tailDecoded = t16, true
		}
	}
	return head + s[m[0]:m[1]] + tail, headDecoded || tailDecoded
}

var gateTruncation = regexp.MustCompile(`\n\n\[\.\.\. (\d+) bytes truncated \.\.\.\]\n\n`)

// decodeGateText decodes s as UTF-16 when it starts as UTF-16, unless it is a UTF-16 prefix ending on a
// newline followed by UTF-8, which stdout and stderr in different encodings give; it returns the
// byte order the text ends in, or nil for UTF-8.
func decodeGateText(s string) (string, bool, binary.ByteOrder) {
	if k, order, bom := gateSplit(s); k > 0 {
		return decodeGateUTF16(s[bom:k], order) + s[k:], true, nil
	}
	order, bom := gateByteOrder(s)
	if order == nil {
		return s, false, nil
	}
	return decodeGateUTF16(s[bom:], order), true, order
}

// gateSplit returns where s turns from UTF-16 ending on a newline to plain UTF-8, or 0.
// A UTF-16 newline holds a NUL byte, so only the one holding the last NUL can be followed by NUL-free text.
func gateSplit(s string) (int, binary.ByteOrder, int) {
	nul := strings.LastIndexByte(s, 0)
	k := nul + 2 - nul%2
	if nul < 0 || k >= len(s) || !gatePlainUTF8(s[k:]) {
		return 0, nil, 0
	}
	order, bom := gateByteOrder(s[:k])
	if order == nil || order.Uint16([]byte(s[k-2:k])) != '\n' || gateBadRunes(decodeGateUTF16(s[bom:k], order)) > 0 {
		return 0, nil, 0
	}
	// A UTF-16 last line in one script can also be valid UTF-8, so the rest must not read as such a line.
	if gateOneScript(decodeGateUTF16(s[k:], order)) {
		return 0, nil, 0
	}
	return k, order, bom
}

// gateByteOrder returns the UTF-16 byte order s starts in, or nil, and the length of its BOM.
func gateByteOrder(s string) (binary.ByteOrder, int) {
	switch {
	case strings.HasPrefix(s, "\xff\xfe"):
		return binary.LittleEndian, 2
	case strings.HasPrefix(s, "\xfe\xff"):
		return binary.BigEndian, 2
	}
	// Without a BOM, require alternating NUL and non-NUL bytes in
	// at least two code units, examining up to the first 32 bytes.
	n := min(len(s)/2, 16)
	if n < 2 {
		return nil, 0
	}
	le, be := true, true
	for i := 0; i < n*2; i += 2 {
		le = le && s[i] != 0 && s[i+1] == 0
		be = be && s[i] == 0 && s[i+1] != 0
	}
	switch {
	case le:
		return binary.LittleEndian, 0
	case be:
		return binary.BigEndian, 0
	}
	return nil, 0
}

func gateBOM(order binary.ByteOrder) string {
	if order == binary.BigEndian {
		return "\xfe\xff"
	}
	return "\xff\xfe"
}

// decodeGateUTF16 decodes s in the given byte order; an odd last byte becomes U+FFFD.
func decodeGateUTF16(s string, order binary.ByteOrder) string {
	units := make([]uint16, 0, (len(s)+1)/2)
	for i := 0; i+1 < len(s); i += 2 {
		units = append(units, order.Uint16([]byte(s[i:i+2])))
	}
	if len(s)%2 != 0 {
		units = append(units, '\ufffd')
	}
	return string(utf16.Decode(units))
}

// gatePlainUTF8 reports whether s is valid UTF-8 with no control bytes but tab, newline, carriage return and escape.
func gatePlainUTF8(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 && c != '\t' && c != '\n' && c != '\r' && c != 0x1b || c == 0x7f {
			return false
		}
	}
	return utf8.ValidString(s)
}

// gateOneScript reports whether s is printable text in at most one script besides Common and Inherited.
// Real UTF-16 text reads so; UTF-8 bytes read as UTF-16 scatter over scripts or hit unassigned code points.
func gateOneScript(s string) bool {
	script := ""
	for _, r := range s {
		switch {
		case r == '\t' || r == '\n' || r == '\r' || r == 0x1b:
			continue
		case r == utf8.RuneError || !unicode.IsGraphic(r):
			return false
		case unicode.In(r, unicode.Common, unicode.Inherited):
			continue
		}
		if k := gateScript(r); script == "" {
			script = k
		} else if k != script {
			return false
		}
	}
	return true
}

// gateCJK are the scripts written together in Chinese, Japanese and Korean text.
var gateCJK = []*unicode.RangeTable{unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul, unicode.Bopomofo}

func gateScript(r rune) string {
	for _, t := range gateCJK {
		if unicode.Is(t, r) {
			return "CJK"
		}
	}
	for name, t := range unicode.Scripts {
		if unicode.Is(t, r) {
			return name
		}
	}
	return ""
}

// gateBadRunes counts the NULs and invalid or replaced characters in s.
func gateBadRunes(s string) int {
	n := 0
	for _, r := range s {
		if r == 0 || r == utf8.RuneError {
			n++
		}
	}
	return n
}

// trimCutRune drops a UTF-8 character that s ends inside.
func trimCutRune(s string) string {
	for i := len(s) - 1; i >= max(0, len(s)-3); i-- {
		if utf8.RuneStart(s[i]) {
			if !utf8.FullRuneInString(s[i:]) {
				return s[:i]
			}
			break
		}
	}
	return s
}

// skipCutRune drops the continuation bytes a cut leaves before the first whole UTF-8 character.
func skipCutRune(s string) string {
	for k := 0; k < 3 && s != "" && s[0]&0xc0 == 0x80; k++ {
		s = s[1:]
	}
	return s
}
