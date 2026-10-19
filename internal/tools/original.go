package tools

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/cristiangirlea/harness/internal/shape"
)

var originalHandle = regexp.MustCompile(`^[0-9a-f]{16}$`)
var originalRange = regexp.MustCompile(`^(\d+)-(\d+)$`)
var pointerIndex = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

// Serialize the collision check and publication, so concurrent tool results
// cannot replace each other's hash-named files. It also guards the sizes a
// Workspace keeps of the originals already stored.
var originalWrites sync.Mutex

// The store refuses past these caps instead of evicting, so a handle already
// in a conversation never dangles.
const (
	defaultOriginalsRunMax = 64 << 20
	defaultOriginalsMax    = 1 << 30
)

var errOriginalsFull = errors.New("originals store is full")

func (w *Workspace) toolOutput(text string) string {
	if len(text) <= maxOutput {
		return text
	}
	if w == nil || !w.ShapeTools || w.RunDir == "" {
		return Truncate(text)
	}
	handle, err := w.storeOriginal(text)
	if err != nil {
		return Truncate(text)
	}
	summary, ok := shape.Summarize(shape.Classify(text), text)
	if !ok {
		summary = Truncate(text)
	}
	return fmt.Sprintf("%s\noriginal: %s (%d bytes); read_original reads it", summary, handle, len(text))
}

// Root keeps links inside the record directory; the second root also confines
// reads and writes to originals, even if a hash-named file is replaced by a link.
func (w *Workspace) originals(create bool) (*os.Root, error) {
	if w == nil || w.RunDir == "" {
		return nil, fmt.Errorf("run has no record directory")
	}
	root, err := os.OpenRoot(w.RunDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if create {
		if err := root.Mkdir("originals", 0o755); err != nil && !os.IsExist(err) {
			return nil, err
		}
	}
	return root.OpenRoot("originals")
}

func (w *Workspace) storeOriginal(text string) (string, error) {
	return w.storeOriginalWithWriter(text, io.WriteString)
}

func (w *Workspace) storeOriginalWithWriter(text string, write func(io.Writer, string) (int, error)) (string, error) {
	handle := originalHandleOf(text)
	root, err := w.originals(true)
	if err != nil {
		return "", err
	}
	defer root.Close()
	name := handle + ".txt"
	originalWrites.Lock()
	defer originalWrites.Unlock()
	if held, err := holdsOriginal(root, name, text); err != nil || held {
		return handle, err
	}
	// An unmeasured store would fail open, so it refuses, which falls back to the cut.
	if err := w.measureOriginals(); err != nil {
		return "", err
	}
	size := int64(len(text))
	if w.originalsRun+size > originalsCap("HRN_ORIGINALS_RUN_MAX", defaultOriginalsRunMax) ||
		w.originalsAll+size > originalsCap("HRN_ORIGINALS_MAX", defaultOriginalsMax) {
		return "", errOriginalsFull
	}
	if err := writeOriginal(root, name, text, write); err != nil {
		return "", err
	}
	w.originalsRun += size
	w.originalsAll += size
	return handle, nil
}

func originalHandleOf(text string) string {
	hash := sha256.Sum256([]byte(text))
	return fmt.Sprintf("%x", hash)[:16]
}

// holdsOriginal reports whether name already holds text; other bytes there are an error.
func holdsOriginal(root *os.Root, name, text string) (bool, error) {
	data, err := root.ReadFile(name)
	switch {
	case err == nil && string(data) == text:
		return true, nil
	case err == nil:
		return false, fmt.Errorf("original handle %s already holds different bytes", strings.TrimSuffix(name, ".txt"))
	case os.IsNotExist(err):
		return false, nil
	}
	return false, err
}

func writeOriginal(root *os.Root, name, text string, write func(io.Writer, string) (int, error)) error {
	temp := ".original-" + rand.Text() + ".tmp"
	f, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer root.Remove(temp)
	n, writeErr := write(f, text)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if n != len(text) {
		return io.ErrShortWrite
	}
	if closeErr != nil {
		return closeErr
	}
	return root.Rename(temp, name)
}

// measureOriginals sizes this run's originals and every run's beside it (the
// record directory's parent is HRN_HOME/runs) once; stores keep both current.
// Concurrent runs each count from their own measurement, so they may overshoot.
func (w *Workspace) measureOriginals() error {
	if w.originalsSeen {
		return nil
	}
	run, err := originalsSize(filepath.Join(w.RunDir, "originals"))
	if err != nil {
		return err
	}
	runs := filepath.Dir(w.RunDir)
	entries, err := os.ReadDir(runs)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var all int64
	for _, e := range entries {
		if e.IsDir() {
			n, err := originalsSize(filepath.Join(runs, e.Name(), "originals"))
			if err != nil {
				return err
			}
			all += n
		}
	}
	w.originalsSeen, w.originalsRun, w.originalsAll = true, run, all
	return nil
}

// originalsSize totals dir's regular files; a missing dir or a file pruned meanwhile counts as none.
func originalsSize(dir string) (int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}
	var total int64
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		total += info.Size()
	}
	return total, nil
}

func originalsCap(name string, def int64) int64 {
	if n, err := strconv.ParseInt(os.Getenv(name), 10, 64); err == nil && n >= 0 {
		return n
	}
	return def
}

// CopyOriginals copies from's hash-named originals into to past the caps, since the
// continued conversation already cites them; it counts the files it skips.
func CopyOriginals(from, to string) (copied, skipped int, err error) {
	src, err := (&Workspace{RunDir: from}).originals(false)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	defer src.Close()
	entries, err := fs.ReadDir(src.FS(), ".")
	if err != nil {
		return 0, 0, err
	}
	var dst *os.Root
	defer func() {
		if dst != nil {
			dst.Close()
		}
	}()
	// A lowered cap must not drop originals a run stored under the default.
	limit := max(originalsCap("HRN_ORIGINALS_RUN_MAX", defaultOriginalsRunMax), defaultOriginalsRunMax)
	originalWrites.Lock()
	defer originalWrites.Unlock()
	for _, e := range entries {
		handle, ok := strings.CutSuffix(e.Name(), ".txt")
		if !ok || !originalHandle.MatchString(handle) {
			continue
		}
		text, ok := "", e.Type().IsRegular()
		if ok {
			text, ok = readOwnOriginal(src, e.Name(), handle, limit)
		}
		if !ok {
			skipped++
			continue
		}
		if dst == nil {
			if dst, err = (&Workspace{RunDir: to}).originals(true); err != nil {
				return copied, skipped, err
			}
		}
		if _, err := dst.Lstat(e.Name()); err == nil {
			continue // never overwrite what the new run already holds
		}
		if err := writeOriginal(dst, e.Name(), text, io.WriteString); err != nil {
			return copied, skipped, err
		}
		copied++
	}
	return copied, skipped, nil
}

// readOwnOriginal reads name if it is a regular file no larger than limit
// and its content hashes to handle.
func readOwnOriginal(root *os.Root, name, handle string, limit int64) (string, bool) {
	f, err := root.Open(name)
	if err != nil {
		return "", false
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return "", false
	}
	data, err := io.ReadAll(io.LimitReader(f, min(limit, 1<<62)+1))
	if err != nil || int64(len(data)) > limit || originalHandleOf(string(data)) != handle {
		return "", false
	}
	return string(data), true
}

// ReadOriginal is added to the API tool set only when shaping is enabled.
func ReadOriginal() Tool {
	type in struct {
		Handle  string `json:"handle"`
		Range   string `json:"range"`
		Lines   string `json:"lines"`
		Pointer string `json:"pointer"`
	}
	return Tool{
		Name:        "read_original",
		Description: "Read a saved original tool result by its 16-character hex handle: the whole original, or one part given by range, lines or pointer.",
		Properties: map[string]any{
			"handle":  str("The 16-character hex handle."),
			"range":   str("Optional byte range a-b, zero-based, end exclusive."),
			"lines":   str("Optional line range a-b, 1-based, inclusive."),
			"pointer": str("Optional RFC 6901 JSON pointer such as /items/3/name, for an original that is one JSON value; ~1 is / and ~0 is ~ in a key."),
		},
		Required: []string{"handle"}, Effect: EffectRead, Summary: summaryOf("handle"),
		Run: func(ctx context.Context, w *Workspace, raw json.RawMessage) (string, error) {
			args, err := decode[in](raw)
			if err != nil {
				return "", err
			}
			if !originalHandle.MatchString(args.Handle) {
				return "", fmt.Errorf("invalid original handle %q: want 16 lowercase hex characters", args.Handle)
			}
			selectors := 0
			for _, s := range []string{args.Range, args.Lines, args.Pointer} {
				if s != "" {
					selectors++
				}
			}
			if selectors > 1 {
				return "", fmt.Errorf("original %s: give at most one of range, lines and pointer", args.Handle)
			}
			root, err := w.originals(false)
			if err != nil {
				return "", fmt.Errorf("original %s: %w", args.Handle, err)
			}
			defer root.Close()
			f, err := root.Open(args.Handle + ".txt")
			if err != nil {
				return "", fmt.Errorf("original %s: %w", args.Handle, err)
			}
			defer f.Close()
			info, err := f.Stat()
			if err != nil {
				return "", err
			}
			if !info.Mode().IsRegular() {
				return "", fmt.Errorf("original %s is not a regular file", args.Handle)
			}
			a, b := int64(0), info.Size()
			if args.Range != "" {
				m := originalRange.FindStringSubmatch(args.Range)
				if m == nil {
					return "", fmt.Errorf("invalid original range %q: want a-b", args.Range)
				}
				var e1, e2 error
				a, e1 = strconv.ParseInt(m[1], 10, 64)
				b, e2 = strconv.ParseInt(m[2], 10, 64)
				if e1 != nil || e2 != nil || a > b || b > info.Size() {
					return "", fmt.Errorf("invalid original range %q for %d bytes", args.Range, info.Size())
				}
			}
			data, err := io.ReadAll(io.NewSectionReader(f, a, b-a))
			if err != nil {
				return "", err
			}
			var part string
			switch {
			case args.Lines != "":
				part, err = originalLines(data, args.Lines)
			case args.Pointer != "":
				part, err = originalPointer(ctx, data, args.Pointer)
			default:
				part = string(data)
			}
			if err != nil {
				return "", fmt.Errorf("original %s: %w", args.Handle, err)
			}
			return part, nil
		},
	}
}

// originalLines returns lines a to b with their line endings; a final line
// without a newline still counts.
func originalLines(data []byte, spec string) (string, error) {
	total := bytes.Count(data, []byte("\n"))
	if len(data) > 0 && data[len(data)-1] != '\n' {
		total++
	}
	m := originalRange.FindStringSubmatch(spec)
	if m == nil {
		return "", fmt.Errorf("invalid lines %q: want a-b, 1-based and inclusive", spec)
	}
	a, e1 := strconv.Atoi(m[1])
	b, e2 := strconv.Atoi(m[2])
	if e1 != nil || e2 != nil || a < 1 || a > b || b > total {
		return "", fmt.Errorf("lines %q are not within the original's %d lines", spec, total)
	}
	start := 0
	for range a - 1 {
		start += bytes.IndexByte(data[start:], '\n') + 1
	}
	end := start
	for range b - a + 1 {
		i := bytes.IndexByte(data[end:], '\n')
		if i < 0 {
			end = len(data)
			break
		}
		end += i + 1
	}
	return string(data[start:end]), nil
}

// originalPointer resolves an RFC 6901 pointer in one pass over data and returns
// the value's own bytes, only compacted, so numbers keep the spelling they had.
func originalPointer(ctx context.Context, data []byte, pointer string) (string, error) {
	if !json.Valid(data) {
		return "", fmt.Errorf("pointer %q: the original is not one JSON value", pointer)
	}
	if !strings.HasPrefix(pointer, "/") {
		return "", fmt.Errorf("invalid pointer %q: want /key/index as in RFC 6901", pointer)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber() // a number too large for float64 is still a valid value in the way
	skip := func() error { var v json.RawMessage; return dec.Decode(&v) }
	at, walked := "the root", ""
	for token := range strings.SplitSeq(pointer[1:], "/") {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		key, ok := unescapePointer(token)
		if !ok {
			return "", fmt.Errorf("invalid pointer %q: ~ must be followed by 0 or 1", pointer)
		}
		open, err := dec.Token()
		if err != nil {
			return "", err
		}
		switch open {
		case json.Delim('{'):
			found := false
			for !found && dec.More() {
				name, err := dec.Token()
				if err != nil {
					return "", err
				}
				if found = name == key; !found {
					if err := skip(); err != nil {
						return "", err
					}
				}
			}
			if !found {
				return "", fmt.Errorf("pointer %q: no key %q in the object at %s", pointer, key, at)
			}
		case json.Delim('['):
			want, err := strconv.Atoi(key)
			if !pointerIndex.MatchString(key) || err != nil {
				want = -1
			}
			n := 0
			for ; n != want && dec.More(); n++ {
				if err := skip(); err != nil {
					return "", err
				}
			}
			if n != want || !dec.More() {
				return "", fmt.Errorf("pointer %q: no index %q in the %d-element array at %s", pointer, key, n, at)
			}
		default:
			return "", fmt.Errorf("pointer %q: the value at %s is not an object or array", pointer, at)
		}
		walked += "/" + token
		at = walked
	}
	var value json.RawMessage
	if err := dec.Decode(&value); err != nil {
		return "", err
	}
	var out bytes.Buffer
	if err := json.Compact(&out, value); err != nil {
		return "", err
	}
	return out.String(), nil
}

// unescapePointer turns ~1 into / before ~0 into ~, the order RFC 6901 requires.
func unescapePointer(token string) (string, bool) {
	for i := 0; i < len(token); i++ {
		if token[i] == '~' && (i+1 == len(token) || (token[i+1] != '0' && token[i+1] != '1')) {
			return "", false
		}
	}
	return strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~"), true
}
