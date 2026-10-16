package runlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
)

// How a turn came after a compaction: the backend said so, or the context
// fell by more than half from the turn before, which is a reading of the
// numbers and never written to a record.
const (
	CompactionRecorded = "recorded"
	CompactionProbable = "probable"
)

// Turn is one model call of a run, from its usage event.
type Turn struct {
	N    int       `json:"turn"`
	Time time.Time `json:"t"`
	agent.Usage
	// CacheReadSoFar is the cache reads of this turn and every turn before.
	CacheReadSoFar int64 `json:"cache_read_so_far,omitempty"`
	// Compaction says whether the context was compacted before this turn.
	Compaction string `json:"compaction,omitempty"`
	// Trigger and CompactedFrom are a recorded compaction's cause and the
	// context it started from.
	Trigger       string `json:"trigger,omitempty"`
	CompactedFrom int64  `json:"compacted_from,omitempty"`
}

// Turns reads a run's events as its model calls, in order. A run whose
// backend writes no usage events, or recorded before they existed, has none.
func Turns(events []agent.Event) []Turn {
	var turns []Turn
	var soFar int64
	var pending *agent.Event // a compaction not yet followed by a turn
	for i, e := range events {
		switch {
		case e.Kind == agent.EventCompaction:
			pending = &events[i]
		case e.Kind == agent.EventUsage && e.Usage != nil:
			soFar += e.Usage.CacheRead
			t := Turn{N: len(turns) + 1, Time: e.Time, Usage: *e.Usage, CacheReadSoFar: soFar}
			switch {
			case pending != nil:
				t.Compaction, t.Trigger = CompactionRecorded, pending.Text
				if pending.Usage != nil {
					t.CompactedFrom = pending.Usage.ContextTokens
				}
				pending = nil
			case len(turns) > 0 && t.ContextTokens < turns[len(turns)-1].ContextTokens/2:
				t.Compaction = CompactionProbable
			}
			turns = append(turns, t)
		}
	}
	return turns
}

// TurnsOf reads run id's turns from its events.jsonl, decoding only the
// usage and compaction lines. A line that does not decode is skipped: the
// turns are a view, and the record's totals stay in result.json.
func TurnsOf(id string) ([]Turn, error) {
	if err := validID(id); err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(Root(), id, "events.jsonl"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	events, err := UsageEvents(f)
	if err != nil {
		return nil, err
	}
	return Turns(events), nil
}

// UsageEvents reads the usage and compaction events of an events.jsonl
// stream, decoding no other line; one that does not decode is skipped.
func UsageEvents(r io.Reader) ([]agent.Event, error) {
	// hrn writes compact JSON with every quote inside a text escaped, so these
	// strings appear as written only in an event of these kinds or a tool's
	// input, which is recorded as given; the kind is checked once decoded.
	usage, compaction := []byte(`"kind":"usage"`), []byte(`"kind":"compaction"`)
	var events []agent.Event
	br := bufio.NewReader(r)
	for {
		line, readErr := br.ReadBytes('\n')
		if readErr != nil && readErr != io.EOF {
			return nil, readErr
		}
		if bytes.Contains(line, usage) || bytes.Contains(line, compaction) {
			var e agent.Event
			if json.Unmarshal(line, &e) == nil && (e.Kind == agent.EventUsage || e.Kind == agent.EventCompaction) {
				events = append(events, e)
			}
		}
		if readErr == io.EOF {
			return events, nil
		}
	}
}
