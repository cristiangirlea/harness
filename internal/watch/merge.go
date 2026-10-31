package watch

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cristiangirlea/harness/internal/batch"
)

type mergeGrant struct {
	token                                       [16]byte
	client, batchID, task, branch, head, target string
	expires                                     time.Time
	row                                         actionRow
}

func (s *server) mergeRefuse(w http.ResponseWriter, r *http.Request, client, reason string, status int) {
	s.writeRefusal(client, r.URL.Path, reason)
	http.Error(w, reason, status)
}

func (s *server) askMerge(w http.ResponseWriter, r *http.Request) {
	client := s.actionClient(w, r)
	refuse := func(reason string, status int) { s.mergeRefuse(w, r, client, reason, status) }
	if !s.mayChangeBackends(w, r) || !s.identity(r).Admin {
		refuse("local requests only", http.StatusForbidden)
		return
	}
	id, task := r.PathValue("id"), r.PathValue("task")
	if !idRE.MatchString(id) {
		refuse("batch not found", http.StatusNotFound)
		return
	}
	res, err := batch.LoadResultFor(s.who(r), id)
	if err != nil {
		refuse("batch not found", http.StatusNotFound)
		return
	}
	tr, err := res.Find(task)
	if err != nil {
		refuse("task not found", http.StatusNotFound)
		return
	}
	if tr.Status != batch.StatusPass {
		refuse("task did not pass its gates", http.StatusConflict)
		return
	}
	if tr.Commit == "" || tr.Branch == "" {
		refuse("task changed nothing", http.StatusConflict)
		return
	}
	head, err := batch.BranchHead(r.Context(), res, task)
	if err != nil {
		refuse("branch could not be read", http.StatusConflict)
		return
	}
	target, err := batch.TargetBranch(r.Context(), res)
	if err != nil {
		refuse("target branch could not be read", http.StatusConflict)
		return
	}
	var g mergeGrant
	if _, err = rand.Read(g.token[:]); err != nil {
		http.Error(w, "Could not create confirmation", 500)
		return
	}
	var idBytes [16]byte
	if _, err = rand.Read(idBytes[:]); err != nil {
		http.Error(w, "Could not create confirmation", 500)
		return
	}
	g.client, g.batchID, g.task, g.branch, g.head, g.target = client, id, task, tr.Branch, head, target
	g.expires = s.mergeNow().Add(2 * time.Minute)
	g.row = actionRow{Time: time.Now().UTC().Format(time.RFC3339Nano), ID: hex.EncodeToString(idBytes[:]), Client: client, Action: "merge", Target: id + "/" + task, Old: map[string]string{"branch": tr.Branch, "head": head, "target": target}, Result: "intent"}
	s.mergeMu.Lock()
	defer s.mergeMu.Unlock()
	if s.mergeBusy[filepath.Clean(res.Dir)] {
		refuse("batch is busy", http.StatusConflict)
		return
	}
	if err := auditAppender(g.row); err != nil {
		http.Error(w, "Action audit could not be written", 500)
		return
	}
	now := s.mergeNow()
	kept := s.mergeGrants[:0]
	for _, v := range s.mergeGrants {
		if now.Before(v.expires) {
			kept = append(kept, v)
		}
	}
	s.mergeGrants = append(kept, g)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"token": hex.EncodeToString(g.token[:]), "task": task, "branch": tr.Branch, "head": head, "target": target})
}

func (s *server) mergeNow() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}

func (s *server) confirmMerge(w http.ResponseWriter, r *http.Request) {
	client := s.actionClient(w, r)
	refuse := func(reason string, status int) { s.mergeRefuse(w, r, client, reason, status) }
	if !s.mayChangeBackends(w, r) || !s.identity(r).Admin {
		refuse("local requests only", http.StatusForbidden)
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	d.DisallowUnknownFields()
	if d.Decode(&body) != nil || d.Decode(new(any)) != io.EOF {
		refuse("invalid confirmation", http.StatusBadRequest)
		return
	}
	raw, err := hex.DecodeString(body.Token)
	if err != nil || len(raw) != 16 {
		refuse("invalid confirmation", http.StatusForbidden)
		return
	}
	s.mergeMu.Lock()
	var g mergeGrant
	found := false
	for i, v := range s.mergeGrants {
		if subtle.ConstantTimeCompare(raw, v.token[:]) == 1 {
			g = v
			s.mergeGrants = append(s.mergeGrants[:i], s.mergeGrants[i+1:]...)
			found = true
			break
		}
	}
	if !found {
		s.mergeMu.Unlock()
		refuse("invalid confirmation", http.StatusForbidden)
		return
	}
	if g.client != client || g.batchID != r.PathValue("id") || g.task != r.PathValue("task") || !s.mergeNow().Before(g.expires) {
		s.mergeMu.Unlock()
		refuse("invalid or expired confirmation", http.StatusForbidden)
		return
	}
	res, err := batch.LoadResultFor(s.who(r), g.batchID)
	if err != nil {
		s.mergeMu.Unlock()
		refuse("batch not found", http.StatusNotFound)
		return
	}
	repo := filepath.Clean(res.Dir)
	if s.mergeBusy[repo] {
		s.mergeMu.Unlock()
		refuse("batch is busy", http.StatusConflict)
		return
	}
	if s.mergeBusy == nil {
		s.mergeBusy = make(map[string]bool)
	}
	s.mergeBusy[repo] = true
	s.mergeMu.Unlock()
	defer func() { s.mergeMu.Lock(); delete(s.mergeBusy, repo); s.mergeMu.Unlock() }()
	if err == nil {
		var tr batch.TaskResult
		tr, err = res.Find(g.task)
		if err == nil && (tr.Status != batch.StatusPass || tr.Commit == "" || tr.Branch != g.branch) {
			err = fmt.Errorf("task is no longer eligible")
		}
	}
	if err == nil {
		var target string
		target, err = batch.TargetBranch(r.Context(), res)
		if err == nil && target != g.target {
			err = fmt.Errorf("target branch changed")
		}
	}
	var result []batch.MergeResult
	if err == nil {
		run := batch.MergeAt
		if s.mergeRun != nil {
			run = s.mergeRun
		}
		result, err = run(r.Context(), res, g.task, g.head)
	}
	outcome := ""
	if err != nil {
		if strings.Contains(err.Error(), "the branch moved") {
			outcome = "the branch moved"
		} else {
			outcome = "merge failed: " + s.cleanMergeError(err)
		}
	} else if len(result) != 1 {
		outcome = "merge failed"
	} else if result[0].Skipped != "" {
		outcome = result[0].Skipped
	} else {
		outcome = "merged " + result[0].Commit
	}
	if outcome == "the branch moved" {
		s.writeRefusal(client, r.URL.Path, outcome)
	}
	g.row.Result = outcome
	if e := auditAppender(g.row); e != nil {
		fmt.Fprintln(os.Stderr, "merge outcome not audited")
	}
	if err != nil || !strings.HasPrefix(outcome, "merged ") {
		message := outcome
		if strings.HasPrefix(message, "merge failed:") {
			message = "merge failed"
		}
		http.Error(w, message, http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"commit": result[0].Commit})
}

func (s *server) cleanMergeError(err error) string {
	text := err.Error()
	if s.redact != nil {
		text = s.redact.Replace(text)
	}
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r", " "), "\n", " ")
}
