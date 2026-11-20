package plugin

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cristiangirlea/harness/internal/pluginproto"
	"github.com/cristiangirlea/harness/internal/tools"
)

func TestMain(m *testing.M) {
	base := filepath.Base(os.Args[0])
	if strings.EqualFold(filepath.Ext(base), ".exe") {
		base = base[:len(base)-4]
	}
	if name, ok := strings.CutPrefix(base, "hrn-"); ok {
		os.Exit(fakePlugin(name))
	}
	os.Exit(m.Run())
}

func fakePlugin(name string) int {
	log, err := os.OpenFile(filepath.Join(filepath.Dir(os.Args[0]), "received.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer log.Close()
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 5<<20)
	var seq int64
	var waiting string
	for scanner.Scan() {
		if _, err := fmt.Fprintln(log, scanner.Text()); err != nil {
			return 1
		}
		frame, err := pluginproto.ParseHostLine(scanner.Bytes())
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		switch frame := frame.(type) {
		case *pluginproto.Hello:
			if seq != 0 || frame.Host.Name != "hrn" || frame.Limits == nil || frame.Limits.MaxLine != pluginproto.MaxLine || len(frame.Protocols) != 1 || frame.Protocols[0] != pluginproto.V1 {
				return 1
			}
			switch name {
			case "silent":
				continue
			case "garbage":
				fmt.Fprintln(os.Stdout, "not json")
				continue
			case "longline":
				_, _ = io.Copy(os.Stdout, strings.NewReader(strings.Repeat("x", pluginproto.MaxLine+3)))
				continue
			case "oldproto":
				fakeWrite(pluginproto.Error{V: 1, Seq: 1, Type: pluginproto.TypeError, Code: pluginproto.CodeUnsupportedProtocol, Message: "this plugin speaks v2 only", Protocols: []string{"v2"}})
				return 2
			case "noisy":
				fmt.Fprintln(os.Stderr, "boom on stderr")
				return 1
			}
			seq++
			program := pluginproto.Program{Name: name, Version: "0.1.0"}
			if name == "othername" {
				program.Name = "someone-else"
			}
			capabilities := []pluginproto.Capability{{Name: "wait"}}
			if name == "echo" {
				capabilities = []pluginproto.Capability{{Name: "echo"}, {Name: "wait"}, {Name: "env"}, {Name: "exit"}, {Name: "skip"}}
			}
			fakeWrite(pluginproto.Ready{V: 1, Seq: seq, Type: pluginproto.TypeReady, Protocol: pluginproto.V1, Plugin: program, Capabilities: capabilities})
		case *pluginproto.Call:
			result := pluginproto.Result{V: 1, Type: pluginproto.TypeResult, ID: frame.ID, Status: pluginproto.StatusOK}
			switch frame.Capability {
			case "echo", "wait":
				seq++
				fakeWrite(pluginproto.Event{V: 1, Seq: seq, Type: pluginproto.TypeEvent, ID: frame.ID, Kind: pluginproto.KindProgress, Text: "working"})
				if frame.Capability == "wait" {
					waiting = frame.ID
					continue
				}
				result.Output = frame.Input
				if len(result.Output) == 0 {
					result.Output = json.RawMessage(`{}`)
				}
			case "env":
				result.Output, err = fakeEnv(frame.Input)
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
					return 1
				}
			case "exit":
				return 3
			case "skip":
				seq++
			default:
				return 1
			}
			seq++
			result.Seq = seq
			fakeWrite(result)
		case *pluginproto.Cancel:
			if waiting == "" || waiting != frame.ID {
				return 1
			}
			if name == "stubborn" {
				continue
			}
			waiting = ""
			seq++
			fakeWrite(pluginproto.Result{V: 1, Seq: seq, Type: pluginproto.TypeResult, ID: frame.ID, Status: pluginproto.StatusCanceled})
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if name == "stubborn" && waiting != "" {
		for {
			time.Sleep(time.Second)
		}
	}
	return 0
}

type envEntry struct {
	Set   bool   `json:"set"`
	Value string `json:"value,omitempty"`
}

func fakeEnv(input json.RawMessage) (json.RawMessage, error) {
	var request struct {
		Names []string `json:"names"`
	}
	if err := json.Unmarshal(input, &request); err != nil {
		return nil, err
	}
	output := make(map[string]envEntry, len(request.Names))
	for _, name := range request.Names {
		value, set := os.LookupEnv(name)
		entry := envEntry{Set: set}
		if name == tools.GuardedVar || name == "GH_TOKEN" {
			entry.Value = value
		}
		output[name] = entry
	}
	return json.Marshal(output)
}

func fakeWrite(frame any) {
	if err := json.NewEncoder(os.Stdout).Encode(frame); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func pluginHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	t.Setenv("PATH", "")
	if runtime.GOOS == "windows" {
		t.Setenv("PATHEXT", ".EXE")
	}
	return home
}

func installFake(t *testing.T, home, name string) string {
	t.Helper()
	return copyFake(t, filepath.Join(home, "plugins", name), name)
}

func copyFake(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "hrn-"+name)
	if runtime.GOOS == "windows" {
		path += ".exe"
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// A copy, not a hard link: Windows cannot delete a link to the running test binary.
	src, err := os.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(dst, src)
	closeErr := dst.Close()
	if copyErr != nil || closeErr != nil {
		t.Fatalf("copying test executable: copy error %v, close error %v", copyErr, closeErr)
	}
	return path
}

func receivedFrames(t *testing.T, exe string) []any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(filepath.Dir(exe), "received.log"))
	if err != nil {
		t.Fatal(err)
	}
	var frames []any
	for _, line := range bytes.Split(bytes.TrimSuffix(data, []byte{'\n'}), []byte{'\n'}) {
		frame, err := pluginproto.ParseHostLine(line)
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, frame)
	}
	return frames
}
