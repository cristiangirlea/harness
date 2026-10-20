// Package watch follows live run and batch records for the CLI and web timeline.
package watch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/runlog"
)

// Line holds an events.jsonl event or parse error, with End as the next byte offset.
type Line struct {
	Event agent.Event
	Err   error
	End   int64
}

// Tail reads appended lines, reopening each time so Windows handles cannot block run removal.
type Tail struct {
	Path   string
	Offset int64 // byte offset just past the last line read
}

// Read returns completed lines, treating a missing file as empty.
// An unterminated line waits for its writer to finish it.
func (t *Tail) Read() ([]Line, error) { return t.read(false) }

// Finish reads all remaining lines, including an unterminated line that can no longer be completed.
func (t *Tail) Finish() ([]Line, error) { return t.read(true) }

func (t *Tail) read(final bool) ([]Line, error) {
	f, err := os.Open(t.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() < t.Offset {
		return nil, fmt.Errorf("%s is shorter than the %d bytes already read from it", t.Path, t.Offset)
	}
	buf := make([]byte, info.Size()-t.Offset)
	n, err := f.ReadAt(buf, t.Offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	buf = buf[:n]
	var lines []Line
	for len(buf) > 0 {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 && !final {
			break
		}
		raw := buf
		if i >= 0 {
			raw, buf = buf[:i], buf[i+1:]
			t.Offset += int64(i + 1)
		} else {
			buf = nil
			t.Offset += int64(len(raw))
		}
		raw = bytes.TrimSpace(raw) // also a \r left by an editor
		if len(raw) == 0 {
			continue
		}
		l := Line{End: t.Offset}
		if err := json.Unmarshal(raw, &l.Event); err != nil {
			l.Err = fmt.Errorf("unreadable line at byte %d: %v", t.Offset-int64(len(raw)), err)
		}
		lines = append(lines, l)
	}
	return lines, nil
}

// Follower reads a run's events as the run writes them.
type Follower struct {
	id   string
	tail Tail
}

// NewFollower returns the current record and a follower starting at byte offset from (zero or a prior Line.End).
func NewFollower(id string, from int64) (*Follower, runlog.Record, error) {
	rec, err := load(id)
	if err != nil {
		return nil, rec, err
	}
	f := &Follower{id: id, tail: Tail{Path: filepath.Join(runlog.Root(), id, "events.jsonl"), Offset: from}}
	return f, rec, nil
}

// In-place result.json writes may be partial or empty on Windows; use task.json until they parse.
func load(id string) (runlog.Record, error) {
	rec, err := runlog.Load(id) // also refuses an id that leaves the runs directory
	if err == nil || !rec.Done {
		return rec, err
	}
	data, terr := os.ReadFile(filepath.Join(runlog.Root(), id, "task.json"))
	if terr != nil {
		return runlog.Record{}, err
	}
	var running runlog.Record
	if json.Unmarshal(data, &running) != nil {
		return runlog.Record{}, err
	}
	running.ID = id
	return running, nil
}

// ID is the run being followed.
func (f *Follower) ID() string { return f.id }

// Offset is the byte position where the next Poll starts reading.
func (f *Follower) Offset() int64 { return f.tail.Offset }

// Poll returns new lines and the record, with done true only after every line has been returned.
func (f *Follower) Poll() (lines []Line, rec runlog.Record, done bool, err error) {
	// Check completion first: events close before result.json is written, so done guarantees all lines are on disk.
	rec, err = load(f.id)
	if errors.Is(err, os.ErrNotExist) {
		return nil, rec, false, fmt.Errorf("run %s was removed", f.id)
	}
	done = err == nil && rec.Done
	if done {
		lines, err = f.tail.Finish()
	} else {
		lines, err = f.tail.Read()
	}
	return lines, rec, done, err
}

// Follow emits events from byte offset from at interval every until completion or ctx cancellation.
// It returns the final record or ctx's error if cancellation wins.
func Follow(ctx context.Context, id string, from int64, every time.Duration, emit func(Line)) (runlog.Record, error) {
	f, rec, err := NewFollower(id, from)
	if err != nil {
		return rec, err
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		lines, rec, done, err := f.Poll()
		for _, l := range lines {
			emit(l)
		}
		if err != nil || done {
			return rec, err
		}
		select {
		case <-ctx.Done():
			return rec, ctx.Err()
		case <-tick.C:
		}
	}
}
