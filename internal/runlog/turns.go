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

// CompactionRecorded and CompactionProbable name recorded and probable (unpersisted inference) sources.
const (
	CompactionRecorded = "recorded"
	CompactionProbable = "probable"
)

// Turn is one model call of a run, from its usage event.
type Turn struct {
	N    int       `json:"turn"`
	Time time.Time `json:"t"`
	agent.Usage
	CacheReadSoFar int64 `json:"cache_read_so_far,omitempty"`
	// Compaction is recorded, probable or empty when none was observed.
	Compaction string `json:"compaction,omitempty"`
	// Trigger and CompactedFrom apply only to recorded compactions.
	Trigger       string `json:"trigger,omitempty"`
	CompactedFrom int64  `json:"compacted_from,omitempty"`
}

// Turns returns model calls in usage-event order.
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

// TurnsOf skips malformed event lines; authoritative totals remain in result.json.
func TurnsOf(id string) ([]Turn, error) {
	if err := validID(id); err != nil {
		return nil, err
	}
	if _, err := ResolveHome(); err != nil {
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

// UsageEvents reads valid usage and compaction events.
func UsageEvents(r io.Reader) ([]agent.Event, error) {
	// Raw tool input can match these compact-JSON markers; verify the decoded kind too.
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
