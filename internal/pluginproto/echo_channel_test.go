package pluginproto

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

var errEchoRefused = errors.New("echo handshake refused")

func TestHelperEchoChannel(t *testing.T) {
	dir := os.Getenv("ECHO_CHANNEL_SERVICE_DIR")
	if dir == "" {
		return
	}
	if err := runEchoChannel(dir, os.Stdin, os.Stdout); err != nil {
		if errors.Is(err, errEchoRefused) {
			os.Exit(ExitRefused)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func runEchoChannel(dir string, in io.Reader, out io.Writer) error {
	lines := bufio.NewScanner(in)
	lines.Buffer(make([]byte, 4096), MaxLine+2)
	if !lines.Scan() {
		if err := lines.Err(); err != nil {
			return fmt.Errorf("read hello: %w", err)
		}
		return fmt.Errorf("missing hello: %w", io.EOF)
	}
	frame, err := ParseHostLine(lines.Bytes())
	if err != nil {
		return err
	}
	hello, ok := frame.(*Hello)
	if !ok {
		return fmt.Errorf("expected hello")
	}
	send := json.NewEncoder(out).Encode
	version, ok := Negotiate(hello.Protocols, []string{V2})
	if !ok {
		if err := send(Error{V: 1, Seq: 1, Type: TypeError, Code: CodeUnsupportedProtocol, Message: "echo requires v2", Protocols: []string{V2}}); err != nil {
			return err
		}
		return errEchoRefused
	}
	seq := int64(1)
	err = send(Ready{V: 1, Seq: seq, Type: TypeReady, Protocol: version, Plugin: Program{Name: "echo", Version: "1"}, Kind: KindChannel, Channel: &Channel{Name: "echo"}})
	if err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(dir, "messages"))
	if err != nil {
		return err
	}
	messages := strings.Fields(string(data))
	offsetPath := filepath.Join(dir, "offset")
	saved := struct {
		Offset int
		Acked  map[string]bool
	}{Acked: make(map[string]bool)}
	data, err = os.ReadFile(offsetPath)
	if err == nil {
		err = json.Unmarshal(data, &saved)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if saved.Offset < 0 || saved.Offset > len(messages) {
		return fmt.Errorf("offset outside message file")
	}
	inFlight := make(map[string]bool)
	for _, id := range messages[saved.Offset:] {
		if saved.Acked[id] {
			continue
		}
		seq++
		if err := send(Inbound{V: 1, Seq: seq, Type: TypeInbound, ID: id, Time: "2026-01-15T10:30:00Z", Sender: Sender{ID: "reader"}, Chat: Chat{ID: "room"}, Text: id}); err != nil {
			return err
		}
		inFlight[id] = true
	}
	for lines.Scan() {
		frame, err := ParseHostLineV2(lines.Bytes(), KindChannel)
		if err != nil {
			fields, kind, parseErr := parseLine(lines.Bytes())
			var id string
			seq++
			if parseErr == nil && kind == TypeDeliver && json.Unmarshal(fields["id"], &id) == nil && callID.MatchString(id) {
				err = send(DeliveryFailed{V: 1, Seq: seq, Type: TypeDeliveryFailed, ID: id, Error: Problem{Code: CodeBadRequest, Message: err.Error()}})
			} else {
				err = send(Error{V: 1, Seq: seq, Type: TypeError, Code: CodeBadRequest, Message: err.Error()})
			}
			if err != nil {
				return err
			}
			continue
		}
		switch f := frame.(type) {
		case *Ack:
			if !inFlight[f.ID] {
				return fmt.Errorf("unexpected ack %q", f.ID)
			}
			delete(inFlight, f.ID)
			saved.Acked[f.ID] = true
			for saved.Offset < len(messages) && saved.Acked[messages[saved.Offset]] {
				delete(saved.Acked, messages[saved.Offset])
				saved.Offset++
			}
			// A restart needs both the contiguous offset and any acks beyond the gap.
			data, err := json.Marshal(saved)
			if err != nil {
				return err
			}
			if err := os.WriteFile(offsetPath+".tmp", data, 0600); err != nil {
				return err
			}
			if err := os.Rename(offsetPath+".tmp", offsetPath); err != nil {
				return err
			}
		case *Deliver:
			seq++
			if err := send(Delivered{V: 1, Seq: seq, Type: TypeDelivered, ID: f.ID, MessageID: strconv.FormatInt(seq, 10)}); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unexpected host line %T", f)
		}
	}
	return lines.Err()
}

type echoProcess struct {
	cmd      *exec.Cmd
	input    io.WriteCloser
	output   *bufio.Reader
	sequence int64
	channel  string
}

func startEcho(t *testing.T, dir string) *echoProcess {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHelperEchoChannel$")
	cmd.Env = append(os.Environ(), "ECHO_CHANNEL_SERVICE_DIR="+dir)
	cmd.Stderr = os.Stderr
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	p := &echoProcess{cmd: cmd, input: input, output: bufio.NewReader(output)}
	p.send(t, Hello{Type: TypeHello, Protocols: []string{V2, V1}, Host: Program{Name: "ada", Version: "1"}})
	r, ok := p.receive(t).(*Ready)
	if !ok || r.Kind != KindChannel || r.Protocol != V2 || r.Channel == nil {
		t.Fatalf("unexpected ready: %+v", r)
	}
	p.channel = r.Channel.Name
	return p
}

func (p *echoProcess) send(t *testing.T, frame any) {
	t.Helper()
	if err := json.NewEncoder(p.input).Encode(frame); err != nil {
		t.Fatal(err)
	}
}

func (p *echoProcess) crash(t *testing.T) {
	t.Helper()
	if err := p.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := p.cmd.Wait(); err == nil {
		t.Fatal("expected killed plugin")
	}
}

func (p *echoProcess) receive(t *testing.T) any {
	t.Helper()
	line, err := p.output.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	frame, err := ParsePluginLineV2(line, KindChannel)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct{ Seq int64 }
	p.sequence++
	if err := json.Unmarshal(line, &envelope); err != nil || envelope.Seq != p.sequence {
		t.Fatalf("expected sequence %d, got %s", p.sequence, line)
	}
	return frame
}

func TestEchoChannelRefusesV1(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHelperEchoChannel$")
	cmd.Env = append(os.Environ(), "ECHO_CHANNEL_SERVICE_DIR="+t.TempDir())
	cmd.Stdin = strings.NewReader(helloLine + "\n")
	line, err := cmd.Output()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != ExitRefused {
		t.Fatalf("expected refused exit, got %v", err)
	}
	frame, err := ParsePluginLine(line)
	if err != nil {
		t.Fatal(err)
	}
	f := frame.(*Error)
	if f.Seq != 1 || f.Code != CodeUnsupportedProtocol || len(f.Protocols) != 1 || f.Protocols[0] != V2 {
		t.Fatalf("unexpected refusal: %+v", f)
	}
}

func TestEchoChannelResumesAfterLastAck(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "messages"), []byte("one\ntwo\nthree\n"), 0600); err != nil {
		t.Fatal(err)
	}
	type record struct {
		Channel string
		Message Inbound
	}
	storePath := filepath.Join(dir, "stored")
	seen := map[[2]string]bool{}
	receiveAndStore := func(p *echoProcess, id string) {
		t.Helper()
		msg := p.receive(t).(*Inbound)
		if msg.ID != id {
			t.Fatalf("got %q, want %q", msg.ID, id)
		}
		key := [2]string{p.channel, msg.ID}
		if !seen[key] {
			f, err := os.OpenFile(storePath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0600)
			if err != nil {
				t.Fatal(err)
			}
			writeErr := json.NewEncoder(f).Encode(record{Channel: p.channel, Message: *msg})
			if err := errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
				t.Fatal(err)
			}
			seen[key] = true
		}
	}
	checkOffset := func(want int) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, "offset"))
		if err != nil {
			t.Fatal(err)
		}
		var saved struct{ Offset *int }
		if err := json.Unmarshal(data, &saved); err != nil || saved.Offset == nil || *saved.Offset != want {
			t.Fatalf("saved position = %s, want %d (decode error %v)", data, want, err)
		}
	}
	beforeStore := startEcho(t, dir)
	if msg := beforeStore.receive(t).(*Inbound); msg.ID != "one" {
		t.Fatalf("expected one before crash, got %q", msg.ID)
	}
	beforeStore.crash(t)
	first := startEcho(t, dir)
	for _, id := range []string{"one", "two", "three"} {
		receiveAndStore(first, id)
	}
	for _, ack := range []struct {
		id     string
		offset int
	}{{"three", 0}, {"one", 1}} {
		first.send(t, Ack{Type: TypeAck, ID: ack.id})
		// A delivery response confirms the plugin processed the preceding ack.
		first.send(t, Deliver{Type: TypeDeliver, ID: ack.id, ChatID: "room", Text: "reply"})
		if got := first.receive(t).(*Delivered); got.ID != ack.id {
			t.Fatalf("unexpected delivery response: %+v", got)
		}
		checkOffset(ack.offset)
	}
	first.crash(t)
	last := startEcho(t, dir)
	receiveAndStore(last, "two")
	last.send(t, Ack{Type: TypeAck, ID: "two"})
	last.send(t, Call{Type: TypeCall, ID: "c1", Capability: "echo"})
	if got := last.receive(t).(*Error); got.Code != CodeBadRequest {
		t.Fatalf("call: %+v", got)
	}
	id := strings.Repeat("d", 128)
	last.send(t, Deliver{Type: TypeDeliver, ID: id, ChatID: "room", Text: "reply"})
	if got := last.receive(t).(*Delivered); got.ID != id || got.MessageID == "" {
		t.Fatalf("deliver: %+v", got)
	}
	last.send(t, json.RawMessage(`{"type":"deliver","id":"d2","ID":"ignored","chat_id":"","text":"reply"}`))
	if got := last.receive(t).(*DeliveryFailed); got.ID != "d2" || got.Error.Code != CodeBadRequest {
		t.Fatalf("bad deliver: %+v", got)
	}
	if err := last.input.Close(); err != nil {
		t.Fatal(err)
	}
	if err := last.cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatal(err)
	}
	records := json.NewDecoder(strings.NewReader(string(stored)))
	for _, id := range []string{"one", "two", "three"} {
		var r record
		if err := records.Decode(&r); err != nil {
			t.Fatal(err)
		}
		if r.Channel != "echo" || r.Message.ID != id || r.Message.Text != id || r.Message.Sender.ID != "reader" || r.Message.Chat.ID != "room" {
			t.Fatalf("stored record for %s: %+v", id, r)
		}
	}
	if err := records.Decode(new(record)); err != io.EOF {
		t.Fatalf("unexpected trailing record: %v", err)
	}
	checkOffset(3)
}
