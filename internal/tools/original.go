package tools

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/cristiangirlea/harness/internal/shape"
)

var originalHandle = regexp.MustCompile(`^[0-9a-f]{16}$`)
var originalRange = regexp.MustCompile(`^(\d+)-(\d+)$`)

// Serialize check/publish to prevent concurrent overwrites.
var originalWrites sync.Mutex

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

// The second root confines replaced-file links to originals too.
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
	hash := sha256.Sum256([]byte(text))
	handle := fmt.Sprintf("%x", hash)[:16]
	root, err := w.originals(true)
	if err != nil {
		return "", err
	}
	defer root.Close()
	name := handle + ".txt"
	originalWrites.Lock()
	defer originalWrites.Unlock()
	data, err := root.ReadFile(name)
	if err == nil {
		if string(data) != text {
			return "", fmt.Errorf("original handle %s already holds different bytes", handle)
		}
		return handle, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	temp := ".original-" + rand.Text() + ".tmp"
	f, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", err
	}
	defer root.Remove(temp)
	n, writeErr := write(f, text)
	closeErr := f.Close()
	if writeErr != nil {
		return "", writeErr
	}
	if n != len(text) {
		return "", io.ErrShortWrite
	}
	if closeErr != nil {
		return "", closeErr
	}
	if err := root.Rename(temp, name); err != nil {
		return "", err
	}
	return handle, nil
}

// CopyOriginals preserves resumed handles using hash-verified regular files.
func CopyOriginals(fromRunDir, toRunDir string) error {
	src, err := (&Workspace{RunDir: fromRunDir}).originals(false)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer src.Close()
	entries, err := fs.ReadDir(src.FS(), ".")
	if err != nil {
		return err
	}
	dst := &Workspace{RunDir: toRunDir}
	for _, e := range entries {
		handle, ok := strings.CutSuffix(e.Name(), ".txt")
		if !ok || !originalHandle.MatchString(handle) || !e.Type().IsRegular() {
			continue
		}
		data, err := src.ReadFile(e.Name())
		if err != nil {
			return err
		}
		if fmt.Sprintf("%x", sha256.Sum256(data))[:16] != handle {
			continue
		}
		if _, err := dst.storeOriginal(string(data)); err != nil {
			return err
		}
	}
	return nil
}

// ReadOriginal supplies saved output for shaping.
func ReadOriginal() Tool {
	type in struct {
		Handle string `json:"handle"`
		Range  string `json:"range"`
	}
	return Tool{
		Name:        "read_original",
		Description: "Read a saved original tool result by its 16-character hex handle. Optional range a-b uses zero-based byte offsets, start inclusive and end exclusive. Without a range returns the whole original.",
		Properties:  map[string]any{"handle": str("The 16-character hex handle."), "range": str("Optional byte range a-b, end exclusive.")},
		Required:    []string{"handle"}, Effect: EffectRead, Summary: summaryOf("handle"),
		Run: func(_ context.Context, w *Workspace, raw json.RawMessage) (string, error) {
			args, err := decode[in](raw)
			if err != nil {
				return "", err
			}
			if !originalHandle.MatchString(args.Handle) {
				return "", fmt.Errorf("invalid original handle %q: want 16 lowercase hex characters", args.Handle)
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
			return string(data), err
		},
	}
}
