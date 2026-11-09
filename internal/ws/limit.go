package ws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/cristiangirlea/harness/internal/runpaths"
)

// SlotLimit keeps the source of a repository's slot ceiling for diagnostics.
type SlotLimit struct {
	Max  int
	Path string
}

func (l SlotLimit) String() string {
	if l.Path == "" {
		return fmt.Sprintf("at most %d slots per repository", l.Max)
	}
	return fmt.Sprintf("this repository allows %d slots (%s)", l.Max, l.Path)
}

// MaxError marks a --max above what the repository allows, so callers can treat it as misuse.
type MaxError struct {
	Max   int
	Limit SlotLimit
}

func (e *MaxError) Error() string { return fmt.Sprintf("--max %d: %s", e.Max, e.Limit) }

// ReadLimit resolves the main checkout so all its worktrees share one limit.
func ReadLimit(ctx context.Context, repo string) (SlotLimit, error) {
	if repo == "" {
		repo = "."
	}
	main, err := MainCheckout(ctx, repo)
	if err != nil {
		return SlotLimit{}, err
	}
	return readLimit(main)
}

func readLimit(main string) (SlotLimit, error) {
	for _, path := range []string{filepath.Join(main, ".hrn", "ws.json"), filepath.Join(runpaths.Home(), "ws.json")} {
		data, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return SlotLimit{}, fmt.Errorf("%s: %w", path, err)
		}
		var settings struct {
			MaxSlots int `json:"max_slots"`
		}
		if err := json.Unmarshal(data, &settings); err != nil {
			return SlotLimit{}, fmt.Errorf("%s: %w", path, err)
		}
		if settings.MaxSlots < 1 || settings.MaxSlots > Limit {
			return SlotLimit{}, fmt.Errorf("%s: max_slots must be an integer from 1 to %d", path, Limit)
		}
		return SlotLimit{Max: settings.MaxSlots, Path: path}, nil
	}
	return SlotLimit{Max: Limit}, nil
}
