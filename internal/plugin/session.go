package plugin

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cristiangirlea/harness/internal/envpolicy"
	"github.com/cristiangirlea/harness/internal/gitenv"
	"github.com/cristiangirlea/harness/internal/nowindow"
	"github.com/cristiangirlea/harness/internal/pluginproto"
	"github.com/cristiangirlea/harness/internal/tools"
)

const (
	defaultHandshakeTimeout = 10 * time.Second
	cancelGrace             = 2 * time.Second
	closeGrace              = 3 * time.Second
	treeWait                = 5 * time.Second
	stderrDrain             = 250 * time.Millisecond
	maxFramedLine           = pluginproto.MaxLine + 2
)

type Options struct {
	Host             pluginproto.Program // Required host name and version.
	Dir              string              // Empty inherits the current directory.
	Stderr           io.Writer           // Optional synchronous copy of child diagnostics.
	HandshakeTimeout time.Duration       // Zero uses ten seconds.
}

type Info struct {
	Name         string
	Version      string
	Protocol     string
	Capabilities []pluginproto.Capability
	Path         string
	SHA256       string // Lowercase hex of the executable hash taken before start.
}

type Session struct {
	info     Info
	cmd      *exec.Cmd
	tree     *tools.Tree
	stdin    *os.File
	stdout   *os.File
	stderr   *stderrTail
	guardDir string

	stateMu   sync.Mutex
	broken    bool
	stopping  chan struct{}
	closeOnce sync.Once
	closeErr  error

	callMu   sync.Mutex
	callID   uint64
	sequence int64

	lines     chan pluginLine
	writes    chan hostWrite
	readDone  chan struct{}
	writeDone chan struct{}
	waited    chan struct{}
	waitErr   error
}

type pluginLine struct {
	data []byte
	err  error
}

type hostWrite struct {
	data []byte
	done chan error
}

// Start launches a plugin and checks its v1 handshake.
func Start(ctx context.Context, found Found, opts Options) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("plugin %s: handshake: %w", found.Name, err)
	}
	if !filepath.IsAbs(found.Path) {
		return nil, fmt.Errorf("plugin %s: executable path must be absolute", found.Name)
	}
	hello, err := encodeHostLine(pluginproto.Hello{
		Type:      pluginproto.TypeHello,
		Protocols: []string{pluginproto.V1},
		Host:      opts.Host,
		Limits:    &pluginproto.Limits{MaxLine: pluginproto.MaxLine},
	})
	if err != nil {
		return nil, fmt.Errorf("plugin %s: encode hello: %w", found.Name, err)
	}
	if _, err := pluginproto.ParseHostLine(hello); err != nil {
		return nil, fmt.Errorf("plugin %s: invalid hello: %w", found.Name, err)
	}
	sha, err := hashExecutable(found.Path)
	if err != nil {
		return nil, fmt.Errorf("plugin %s: hash executable: %w", found.Name, err)
	}
	s, err := startProcess(found, opts)
	if err != nil {
		return nil, fmt.Errorf("plugin %s: start: %w", found.Name, err)
	}
	s.info.SHA256 = sha
	timeout := opts.HandshakeTimeout
	if timeout == 0 {
		timeout = defaultHandshakeTimeout
	}
	handshakeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := s.handshake(handshakeCtx, hello); err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			err = fmt.Errorf("timed out after %s: %w", timeout, err)
		}
		s.breakSession()
		_ = s.Close()
		return nil, s.withStderr(fmt.Errorf("plugin %s: handshake: %w", found.Name, err))
	}
	return s, nil
}

func hashExecutable(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func startProcess(found Found, opts Options) (*Session, error) {
	cmd := exec.Command(found.Path)
	nowindow.Set(cmd)
	cmd.Dir = opts.Dir
	env := envpolicy.Filter(gitenv.WithoutRepoVars(os.Environ()))
	guardDir, err := os.MkdirTemp("", "hrn-plugin-guard-*")
	if err != nil {
		return nil, err
	}
	started := false
	defer func() {
		if !started {
			_ = os.RemoveAll(guardDir)
		}
	}()
	env, _ = tools.GuardEnv(env, guardDir)
	cmd.Env = env
	childStdin, stdin, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer childStdin.Close()
	defer func() {
		if !started {
			_ = stdin.Close()
		}
	}()
	cmd.Stdin = childStdin
	// Owned pipes let Wait reap the child independently of protocol I/O.
	stdout, childStdout, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer childStdout.Close()
	defer func() {
		if !started {
			_ = stdout.Close()
		}
	}()
	cmd.Stdout = childStdout
	stderr := &stderrTail{mirror: opts.Stderr}
	cmd.Stderr = stderr
	// A descendant holding stderr must not keep Wait blocked after the leader exits.
	cmd.WaitDelay = stderrDrain
	tree, err := tools.StartTree(cmd)
	if err != nil {
		return nil, err
	}
	started = true
	s := &Session{
		info:      Info{Name: found.Name, Path: found.Path},
		cmd:       cmd,
		tree:      tree,
		stdin:     stdin,
		stdout:    stdout,
		stderr:    stderr,
		guardDir:  guardDir,
		stopping:  make(chan struct{}),
		lines:     make(chan pluginLine, 1),
		writes:    make(chan hostWrite, 2),
		readDone:  make(chan struct{}),
		writeDone: make(chan struct{}),
		waited:    make(chan struct{}),
	}
	go s.readLines()
	go s.writeLines()
	go func() {
		s.waitErr = cmd.Wait()
		close(s.waited)
	}()
	return s, nil
}

func (s *Session) handshake(ctx context.Context, hello []byte) error {
	written, err := s.queueLine(hello)
	if err != nil {
		return err
	}
	if err := s.awaitWrite(ctx, written); err != nil {
		return err
	}
	frame, err := s.readFrame(ctx)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("plugin exited before ready: %w", err)
	}
	if err != nil {
		return err
	}
	switch frame := frame.(type) {
	case *pluginproto.Error:
		return fmt.Errorf("refused with code %q, message %q, protocols %q", frame.Code, frame.Message, frame.Protocols)
	case *pluginproto.Ready:
		if frame.Seq != 1 {
			return fmt.Errorf("ready sequence is %d, want 1", frame.Seq)
		}
		if frame.Protocol != pluginproto.V1 {
			return fmt.Errorf("ready selected protocol %q, want %q", frame.Protocol, pluginproto.V1)
		}
		if frame.Plugin.Name != s.info.Name {
			return fmt.Errorf("ready named plugin %q, want %q", frame.Plugin.Name, s.info.Name)
		}
		s.sequence = frame.Seq
		s.info.Version = frame.Plugin.Version
		s.info.Protocol = frame.Protocol
		s.info.Capabilities = frame.Capabilities
		return nil
	default:
		return fmt.Errorf("expected ready, got %T", frame)
	}
}

// Info returns the metadata agreed during the handshake.
func (s *Session) Info() Info {
	info := s.info
	info.Capabilities = slices.Clone(info.Capabilities)
	return info
}

// Call runs one capability and returns its result, including a failed or canceled result.
func (s *Session) Call(ctx context.Context, capability string, input json.RawMessage, timeout time.Duration, onEvent func(pluginproto.Event)) (*pluginproto.Result, error) {
	if !s.callMu.TryLock() {
		return nil, fmt.Errorf("plugin %s: a call is already in progress", s.info.Name)
	}
	defer s.callMu.Unlock()
	s.stateMu.Lock()
	err := s.stateError()
	s.stateMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("plugin %s: %w", s.info.Name, err)
	}
	if !slices.ContainsFunc(s.info.Capabilities, func(c pluginproto.Capability) bool { return c.Name == capability }) {
		return nil, fmt.Errorf("plugin %s: unknown capability %q", s.info.Name, capability)
	}
	if len(input) > 0 {
		trimmed := bytes.TrimSpace(input)
		if len(trimmed) == 0 || trimmed[0] != '{' || !utf8.Valid(input) || !json.Valid(input) {
			return nil, fmt.Errorf("plugin %s: input must be a JSON object", s.info.Name)
		}
	}
	id := fmt.Sprintf("c%d", s.callID+1)
	call := pluginproto.Call{Type: pluginproto.TypeCall, ID: id, Capability: capability, Input: input}
	if timeout > 0 {
		call.TimeoutMS = max(1, timeout.Milliseconds())
	}
	line, err := encodeHostLine(call)
	if err != nil {
		return nil, fmt.Errorf("plugin %s: encode call %s: %w", s.info.Name, id, err)
	}
	callCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	written, err := s.queueLine(line)
	if err != nil {
		return nil, fmt.Errorf("plugin %s: %w", s.info.Name, err)
	}
	s.callID++
	result, err := s.readResult(callCtx, written, id, onEvent)
	if err == nil {
		return result, nil
	}
	if callCtx.Err() == nil || !errors.Is(err, callCtx.Err()) {
		return nil, s.failCall(id, err)
	}
	var canceled error
	if err := ctx.Err(); err != nil {
		canceled = fmt.Errorf("plugin %s: call %s: %w", s.info.Name, id, err)
	} else {
		canceled = fmt.Errorf("plugin %s: call %s timed out after %s", s.info.Name, id, timeout)
	}
	return s.cancelCall(id, canceled, onEvent)
}

func (s *Session) readResult(ctx context.Context, written <-chan error, id string, onEvent func(pluginproto.Event)) (*pluginproto.Result, error) {
	if err := s.awaitWrite(ctx, written); err != nil {
		return nil, err
	}
	for {
		frame, err := s.readFrame(ctx)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("plugin exited before the result: %w", err)
		}
		if err != nil {
			return nil, err
		}
		var seq int64
		var frameID string
		switch frame := frame.(type) {
		case *pluginproto.Event:
			seq, frameID = frame.Seq, frame.ID
		case *pluginproto.Result:
			seq, frameID = frame.Seq, frame.ID
		case *pluginproto.Error:
			seq, frameID = frame.Seq, frame.ID
		case *pluginproto.Ready:
			return nil, fmt.Errorf("unexpected ready during a call")
		}
		if seq != s.sequence+1 {
			return nil, fmt.Errorf("sequence is %d, want %d", seq, s.sequence+1)
		}
		s.sequence = seq
		if frameID != "" && frameID != id {
			return nil, fmt.Errorf("unexpected call ID %q, want %q", frameID, id)
		}
		switch frame := frame.(type) {
		case *pluginproto.Event:
			if onEvent != nil {
				onEvent(*frame)
			}
		case *pluginproto.Result:
			return frame, nil
		case *pluginproto.Error:
			return nil, fmt.Errorf("received error code %q, message %q", frame.Code, frame.Message)
		}
	}
}

func (s *Session) cancelCall(id string, cause error, onEvent func(pluginproto.Event)) (*pluginproto.Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cancelGrace)
	defer cancel()
	line, err := encodeHostLine(pluginproto.Cancel{Type: pluginproto.TypeCancel, ID: id})
	if err != nil {
		return nil, s.failCall(id, err)
	}
	written, err := s.queueLine(line)
	if err != nil {
		return nil, cause
	}
	result, err := s.readResult(ctx, written, id, onEvent)
	if err == nil {
		return result, cause
	}
	s.breakSession()
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, cause
	}
	return nil, s.callStderr(fmt.Errorf("%w: %v", cause, err))
}

func (s *Session) failCall(id string, err error) error {
	s.breakSession()
	return s.callStderr(fmt.Errorf("plugin %s: call %s: %w", s.info.Name, id, err))
}

func (s *Session) callStderr(err error) error {
	timer := time.NewTimer(stderrDrain)
	defer timer.Stop()
	select {
	case <-s.waited:
	case <-s.stopping:
	case <-timer.C:
	}
	return s.withStderr(err)
}

func (s *Session) withStderr(err error) error {
	if tail := s.stderr.String(); tail != "" {
		return fmt.Errorf("%w\n%s", err, tail)
	}
	return err
}

// Close ends the input stream and reaps the plugin and its process tree.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.stateMu.Lock()
		close(s.stopping)
		broken := s.broken
		s.stateMu.Unlock()
		// A pending pipe write may keep Close blocked until the child is killed.
		stdinClosed := make(chan struct{})
		go func() {
			_ = s.stdin.Close()
			close(stdinClosed)
		}()
		timer := time.NewTimer(closeGrace)
		defer timer.Stop()
		killed := false
		select {
		case <-s.waited:
		case <-timer.C:
			select {
			case <-s.waited:
			default:
				s.kill()
				killed = true
			}
		}
		<-s.waited
		if err := s.tree.WaitGone(treeWait); err != nil && s.tree.Contained() {
			s.kill()
			_ = s.tree.WaitGone(treeWait)
		}
		s.tree.Close()
		_ = s.stdout.Close()
		<-stdinClosed
		<-s.readDone
		<-s.writeDone
		_ = os.RemoveAll(s.guardDir)
		var exitErr *exec.ExitError
		if !broken && !killed && errors.As(s.waitErr, &exitErr) {
			s.closeErr = s.withStderr(fmt.Errorf("plugin %s: %w", s.info.Name, exitErr))
		}
	})
	return s.closeErr
}

func (s *Session) breakSession() {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.stateError() == nil {
		s.broken = true
		s.kill()
	}
}

func (s *Session) kill() {
	if err := s.tree.Kill(); err != nil {
		_ = s.cmd.Process.Kill()
	}
}

func (s *Session) stateError() error {
	select {
	case <-s.stopping:
		return fmt.Errorf("session is closed")
	default:
	}
	if s.broken {
		return fmt.Errorf("session is broken")
	}
	return nil
}

func encodeHostLine(frame any) ([]byte, error) {
	data, err := json.Marshal(frame)
	if err != nil {
		return nil, err
	}
	if len(data) > pluginproto.MaxLine {
		return nil, fmt.Errorf("line exceeds %d bytes", pluginproto.MaxLine)
	}
	return append(data, '\n'), nil
}

func (s *Session) queueLine(data []byte) (<-chan error, error) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if err := s.stateError(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	// The queue fits an in-flight call and its cancel even if the child stops reading.
	s.writes <- hostWrite{data: data, done: done}
	return done, nil
}

func (s *Session) awaitWrite(ctx context.Context, done <-chan error) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.stopping:
		return fmt.Errorf("session is closed")
	case err := <-done:
		if err != nil {
			return fmt.Errorf("write protocol line: %w", err)
		}
		return nil
	}
}

func (s *Session) writeLines() {
	defer close(s.writeDone)
	var err error
	for {
		select {
		case <-s.stopping:
			return
		case write := <-s.writes:
			if err == nil {
				_, err = s.stdin.Write(write.data)
			}
			write.done <- err
		}
	}
}

func (s *Session) readFrame(ctx context.Context) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.stopping:
		return nil, fmt.Errorf("session is closed")
	case line, ok := <-s.lines:
		if !ok {
			return nil, io.EOF
		}
		if line.err != nil {
			return nil, line.err
		}
		frame, err := pluginproto.ParsePluginLine(line.data)
		if err != nil {
			return nil, fmt.Errorf("invalid protocol line: %w", err)
		}
		return frame, nil
	}
}

func (s *Session) readLines() {
	defer close(s.readDone)
	defer close(s.lines)
	limit := &io.LimitedReader{R: s.stdout}
	reader := bufio.NewReader(limit)
	for {
		// Cap the final read so a plugin cannot stall by stopping just past the line limit.
		limit.N = int64(maxFramedLine - reader.Buffered())
		line, err := readPluginLine(reader)
		if err == io.EOF {
			return
		}
		select {
		case <-s.stopping:
			return
		case s.lines <- pluginLine{data: line, err: err}:
		}
		if err != nil {
			return
		}
	}
}

func readPluginLine(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		part, err := reader.ReadSlice('\n')
		line = append(line, part...)
		switch {
		case err == nil:
			return line, nil
		case len(line) >= maxFramedLine:
			return nil, fmt.Errorf("protocol line exceeds %d bytes", pluginproto.MaxLine)
		case err == io.EOF && len(line) > 0:
			return nil, io.ErrUnexpectedEOF
		case err != bufio.ErrBufferFull:
			return nil, err
		}
	}
}

type stderrTail struct {
	mu     sync.Mutex
	data   [8 << 10]byte
	used   int
	mirror io.Writer
}

func (t *stderrTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	if len(p) >= len(t.data) {
		t.used = copy(t.data[:], p[len(p)-len(t.data):])
	} else {
		if overflow := t.used + len(p) - len(t.data); overflow > 0 {
			t.used = copy(t.data[:], t.data[overflow:t.used])
		}
		t.used += copy(t.data[t.used:], p)
	}
	t.mu.Unlock()
	if t.mirror != nil {
		_, _ = t.mirror.Write(p)
	}
	return len(p), nil
}

func (t *stderrTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.data[:t.used])
}
