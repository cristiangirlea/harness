package tools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"

	"github.com/cristiangirlea/harness/internal/shape"
)

var originalHandle = regexp.MustCompile(`^[0-9a-f]{16}$`)
var originalRange = regexp.MustCompile(`^(\d+)-(\d+)$`)

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
		return Truncate(text)
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
	hash := sha256.Sum256([]byte(text))
	handle := fmt.Sprintf("%x", hash)[:16]
	root, err := w.originals(true)
	if err != nil {
		return "", err
	}
	defer root.Close()
	name := handle + ".txt"
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if os.IsExist(err) {
		data, readErr := root.ReadFile(name)
		if readErr != nil {
			return "", readErr
		}
		if string(data) != text {
			return "", fmt.Errorf("original handle %s already holds different bytes", handle)
		}
		return handle, nil
	}
	if err != nil {
		return "", err
	}
	_, writeErr := io.WriteString(f, text)
	closeErr := f.Close()
	if writeErr != nil {
		return "", writeErr
	}
	return handle, closeErr
}

func readOriginal() Tool {
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
