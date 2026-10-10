// Package watch follows hrn's run and batch records while they are written:
// a tail of a run's events.jsonl for hrn show --follow, and a local web page
// for hrn watch that streams the same timeline with Server-Sent Events.
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

// Line is one line of a run's events.jsonl: its event, or why it could not be
// read. End is the byte offset just past it, where reading resumes.
type Line struct {
	Event agent.Event
	Err   error
	End   int64
}

// Tail reads the lines appended to a file since the last read. It opens the
// file for each read and closes it again: an open handle on Windows would stop
// hrn runs prune or batch rm from removing the run.
type Tail struct {
	Path   string
	Offset int64 // just past the last line read
}

// Read returns the lines completed since the last read. A last line without
// its newline is still being written and is left for a later read. A file that
// does not exist yet reads as empty.
func (t *Tail) Read() ([]Line, error) { return t.read(false) }

// Finish is Read for a run that has ended: a last line without its newline
// will never be completed, so it is returned too, as whatever it parses to.
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

// NewFollower starts reading run id's events at byte offset from, which is 0
// or the End of a line read before. It returns the run's record as it stands.
func NewFollower(id string, from int64) (*Follower, runlog.Record, error) {
	rec, err := load(id)
	if err != nil {
		return nil, rec, err
	}
	f := &Follower{id: id, tail: Tail{Path: filepath.Join(runlog.Root(), id, "events.jsonl"), Offset: from}}
	return f, rec, nil
}

// load is runlog.Load for a run that may be finishing. Close writes
// result.json in place, so a reader can catch it half written (on Windows,
// empty): that run has not finished yet, and its record is in task.json.
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
	return running, nil
}

// ID is the run being followed.
func (f *Follower) ID() string { return f.id }

// Offset is where the next Poll starts reading.
func (f *Follower) Offset() int64 { return f.tail.Offset }

// Poll returns the lines written since the last poll, and the run's record.
// done is true once the run has saved its result; every line it wrote has
// been returned by then.
func (f *Follower) Poll() (lines []Line, rec runlog.Record, done bool, err error) {
	// Checked before reading: a run closes its events file before it writes
	// result.json, so a run seen done has every line on disk. A result.json
	// still being written does not parse, and counts as not done yet.
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

// Follow calls emit with each event of run id from byte offset from on, as
// the run writes them, checking every interval. It returns the run's record
// once the run has saved its result, or ctx's error when ctx ends first.
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
