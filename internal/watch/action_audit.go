package watch

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/prefs"
	"github.com/cristiangirlea/harness/internal/runlog"
)

type actionRow struct {
	Time   string       `json:"time"`
	ID     string       `json:"id,omitempty"`
	Client string       `json:"client"`
	Action string       `json:"action"`
	Target string       `json:"target,omitempty"`
	Old    any          `json:"old,omitempty"`
	New    *prefs.Prefs `json:"new,omitempty"`
	Result string       `json:"result"`
	Reason string       `json:"reason,omitempty"`
}

func newIntent(client string, old any, next prefs.Prefs) (actionRow, error) {
	var id [16]byte
	_, err := rand.Read(id[:])
	return actionRow{Time: time.Now().UTC().Format(time.RFC3339Nano), ID: hex.EncodeToString(id[:]), Client: client, Action: "backends", Target: "backends.json", Old: old, New: &next, Result: "intent"}, err
}

var auditAppender = appendAction

func appendAction(row actionRow) error {
	b, err := json.Marshal(row)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(runlog.Home(), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(runlog.Home(), "watch-audit.jsonl"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0600)
	if err != nil {
		return err
	}
	n, err := f.Write(append(b, '\n'))
	if err == nil && n != len(b)+1 {
		err = io.ErrShortWrite
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

func (s *server) actionClient(w http.ResponseWriter, r *http.Request) string {
	if aw, ok := w.(*accessWriter); ok && aw.email != "" {
		return strings.ToLower(aw.email)
	}
	if s.opt.Public == nil && s.fromThisComputer(r) && r.Header.Get("Forwarded") == "" && r.Header.Get("X-Forwarded-For") == "" && r.Header.Get("Cf-Connecting-Ip") == "" {
		return "loopback"
	}
	return "unverified"
}

// admitted orders requests by sequence, since a coarse clock gives equal times.
type admitted struct {
	at  time.Time
	seq uint64
}

func (s *server) allowAction(client string) (ok, first bool) {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	if s.rates == nil {
		s.rates = make(map[string][]admitted)
		s.noted = make(map[string]uint64)
	}
	now := time.Now()
	if s.clock != nil {
		now = s.clock()
	}
	for k, reqs := range s.rates {
		j := 0
		for _, r := range reqs {
			if now.Sub(r.at) < time.Minute {
				reqs[j] = r
				j++
			}
		}
		if j == 0 {
			delete(s.rates, k)
			delete(s.noted, k)
		} else {
			s.rates[k] = reqs[:j]
		}
	}
	reqs := s.rates[client]
	if len(reqs) >= 10 {
		// A new window starts once every request seen at the last note has expired.
		first := reqs[0].seq > s.noted[client]
		if first {
			s.noted[client] = reqs[len(reqs)-1].seq
		}
		return false, first
	}
	s.rateSeq++
	s.rates[client] = append(reqs, admitted{at: now, seq: s.rateSeq})
	return true, false
}

func (s *server) writeRefusal(client, path, reason string) {
	action := "unknown"
	if path == "/backends" {
		action = "backends"
	}
	_ = auditAppender(actionRow{Time: time.Now().UTC().Format(time.RFC3339Nano), Client: client, Action: action, Result: "refused", Reason: reason})
}

func (s *server) refuseAction(w http.ResponseWriter, r *http.Request, client, reason, message string, status int) {
	if r.Method == http.MethodPost {
		ok, first := s.allowAction(client)
		if !ok {
			reason, message, status = "rate limit", "Too many requests", http.StatusTooManyRequests
		}
		if ok || first {
			s.writeRefusal(client, r.URL.Path, reason)
		}
	}
	http.Error(w, message, status)
}

func recentActions() []actionRow {
	f, err := os.Open(filepath.Join(runlog.Home(), "watch-audit.jsonl"))
	if err != nil {
		return nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil
	}
	if info.Size() > 256<<10 {
		if _, err := f.Seek(info.Size()-(256<<10)-1, io.SeekStart); err != nil {
			return nil
		}
		var previous [1]byte
		if _, err := io.ReadFull(f, previous[:]); err != nil {
			return nil
		}
		if previous[0] != '\n' {
			reader := bufio.NewReader(f)
			if _, err := reader.ReadString('\n'); err != nil {
				return nil
			}
			return scanRecentActions(reader)
		}
	}
	return scanRecentActions(f)
}

func scanRecentActions(r io.Reader) []actionRow {
	var rows []actionRow
	positions := make(map[string]int)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		var row actionRow
		if json.Unmarshal(scanner.Bytes(), &row) != nil {
			continue
		}
		if row.Result == "intent" {
			row.Result = "interrupted"
			positions[row.ID] = len(rows)
			rows = append(rows, row)
		} else if i, ok := positions[row.ID]; row.ID != "" && ok {
			rows[i].Result = row.Result
		}
	}
	var recent []actionRow
	for i := len(rows) - 1; i >= 0 && len(recent) < 10; i-- {
		recent = append(recent, rows[i])
	}
	return recent
}
