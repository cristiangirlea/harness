package watch

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
	"github.com/cristiangirlea/harness/internal/mask"
	"github.com/cristiangirlea/harness/internal/runlog"
)

type say struct {
	Kind string `json:"kind"` // started, working, task, done, failed or status
	Text string `json:"text"`
	Ref  string `json:"ref,omitempty"`
	URL  string `json:"url,omitempty"`
}

type botKind struct {
	Name, Label   string
	Speak, Notify bool
}

var botKinds = []botKind{
	{"started", "Something starts", true, false},
	{"working", "What an agent is doing", false, false},
	{"task", "A batch task finishes", true, false},
	{"done", "A run or batch is done", true, true},
	{"failed", "A run or batch stops short", true, true},
	{"status", "What is going, when that changes", false, false},
}

// Wait for batch ownership to be saved so attempts are not announced twice.
const settle = 3 * time.Second

type botItem struct {
	ID, Kind   string // Kind is run or batch
	Who, Where string
	What       string
	Going      bool
	Stopped    bool // quiet without a recorded ending
	Started    time.Time
	ended      time.Time
	Doing      string // the latest activity of a run, or of a batch's running task
	ending     func() (kind, text string)
	tasks      []botTask
}

type botTask struct {
	ID, What string
	Reason   string
	Status   batch.Status
}

// Keep snapshots newest first; announcements traverse them in reverse.
type botSnapshot struct{ items []botItem }

func (s botSnapshot) find(id string) (botItem, bool) {
	for _, it := range s.items {
		if it.ID == id {
			return it, true
		}
	}
	return botItem{}, false
}

func botLook(runs []runlog.Record, batches []batch.Result, doing map[string]string, now time.Time) botSnapshot {
	owner := batchOf(batches)
	var snap botSnapshot
	for _, b := range batches {
		it := botItem{ID: b.ID, Kind: "batch", Where: place(b.Dir), Going: batchGoing(b), Started: b.Started}
		it.Stopped = !it.Going && batchUnfinished(b)
		if !batchUnfinished(b) {
			it.ended = b.Started.Add(b.Duration)
		}
		it.Who = "a batch of " + count(len(b.Tasks), "task")
		for _, t := range b.Tasks {
			it.tasks = append(it.tasks, botTask{ID: t.ID, What: taskName(t), Reason: taskReason(t), Status: t.Status})
			if t.Status == batch.StatusRunning && len(t.Attempts) > 0 {
				last := t.Attempts[len(t.Attempts)-1]
				if d := doing[last.RunID]; d != "" {
					it.Doing = backendName(last.Backend) + " is " + d
				}
			}
		}
		res := b
		it.ending = func() (string, string) { return batchEnding(res) }
		snap.items = append(snap.items, it)
	}
	for _, rec := range runs {
		if _, ofBatch := owner[rec.ID]; ofBatch || (!rec.Done && now.Sub(rec.Started) < settle) {
			continue
		}
		it := botItem{ID: rec.ID, Kind: "run", Who: backendName(rec.Backend), Where: place(rec.Task.Dir),
			What: topic(rec.Task.Prompt), Going: !rec.Done, Started: rec.Started}
		it.Stopped = !runStopped(rec).IsZero()
		it.Going = it.Going && !it.Stopped
		if it.Going {
			it.Doing = doing[rec.ID]
		}
		r := rec
		it.ending = func() (string, string) { return runEnding(backendName(r.Backend), place(r.Task.Dir), r.Result) }
		snap.items = append(snap.items, it)
	}
	slices.SortStableFunc(snap.items, func(a, b botItem) int { return b.Started.Compare(a.Started) })
	return snap
}

// The first look gives status only so old endings are not announced as news.
func announce(prev, cur botSnapshot, first bool) []say {
	status := say{Kind: "status", Text: statusOf(cur)}
	if first {
		return []say{status}
	}
	var out []say
	for i := len(cur.items) - 1; i >= 0; i-- {
		it := cur.items[i]
		if it.Stopped {
			continue // silence is neither a start nor a recorded ending
		}
		ref, url := it.ID, "/"+it.Kind+"/"+it.ID
		was, seen := prev.find(it.ID)
		switch {
		case !seen && it.Going:
			out = append(out, say{"started", startedOf(it), ref, url})
		case it.Going && it.Doing != "" && it.Doing != was.Doing:
			out = append(out, say{"working", workingOf(it), ref, url})
		}
		for n, t := range it.tasks {
			if t.Status == batch.StatusRunning || t.Status == "" {
				continue
			}
			if seen && n < len(was.tasks) && was.tasks[n].Status == t.Status {
				continue
			}
			if !seen && !it.Going {
				continue // the ending says how the tasks went
			}
			out = append(out, say{"task", taskOf(it, n), ref, url})
		}
		if !it.Going && (!seen || was.Going || was.Stopped) {
			kind, text := it.ending()
			out = append(out, say{kind, text, ref, url})
		}
	}
	if status.Text != statusOf(prev) {
		out = append(out, status)
	}
	for i := range out {
		out[i].Text = mask.Text(out[i].Text)
	}
	return out
}

func startedOf(it botItem) string {
	if it.Kind == "batch" {
		return sentence(it.Who + " has started" + in(it.Where))
	}
	text := it.Who + " has started" + in(it.Where)
	if it.What != "" {
		text += ": " + it.What
	}
	return sentence(text)
}

func workingOf(it botItem) string {
	if it.Kind == "batch" {
		return sentence("In the batch" + in(it.Where) + ", " + it.Doing)
	}
	return sentence(it.Who + " is " + it.Doing + in(it.Where))
}

func taskOf(it botItem, n int) string {
	t := it.tasks[n]
	how := map[batch.Status]string{batch.StatusPass: "passed", batch.StatusFail: "failed", batch.StatusError: "hit a problem", batch.StatusCanceled: "was cancelled"}[t.Status]
	if how == "" {
		how = "ended: " + string(t.Status)
	}
	text := fmt.Sprintf("Task %s of %s%s %s", number(n+1), number(len(it.tasks)), in(it.Where), how)
	if t.What != "" {
		text += ": " + t.What
	}
	if t.Status == batch.StatusFail && t.Reason != "" {
		text += ". " + t.Reason
	}
	return sentence(text)
}

// taskReason is the recorded cause of a task's last attempt, never an earlier one or a guess.
func taskReason(t batch.TaskResult) string {
	why := t.Error
	if n := len(t.Attempts); n > 0 {
		a := t.Attempts[n-1]
		for _, g := range a.Gates {
			if g.ExitCode != 0 {
				return "Gate " + g.Command + " " + g.Brief() + "."
			}
		}
		for _, w := range []string{t.Error, a.Result.Error, a.Error} {
			if w != "" {
				why = w
			}
		}
	}
	if first, _ := firstLine(why); first != "" {
		return "The attempt failed: " + words(first, 16) + "."
	}
	return ""
}

func statusOf(s botSnapshot) string {
	var going []string
	for _, it := range s.items {
		if !it.Going {
			continue
		}
		p := it.Who + in(it.Where)
		if it.Doing != "" {
			p += ", " + it.Doing
		}
		going = append(going, p)
	}
	switch len(going) {
	case 0:
		return "Nothing is running."
	case 1:
		return sentence("One thing is going: " + going[0])
	}
	last := len(going) - 1
	return sentence(number(len(going)) + " things are going: " + strings.Join(going[:last], "; ") + "; and " + going[last])
}

func runEnding(who, where string, res agent.Result) (kind, text string) {
	after := " after " + spoken(res.Duration)
	at := in(where)
	switch res.Status {
	case agent.StatusOK:
		return "done", sentence(who+" has finished"+at) + " It took " + spoken(res.Duration) + "."
	case agent.StatusError:
		text = who + " stopped with an error" + at + after
		if why, _ := firstLine(res.Error); why != "" {
			text += ": " + words(why, 12)
		}
		return "failed", sentence(text)
	case agent.StatusMaxTurns:
		return "failed", sentence(who + " ran out of turns" + at + after)
	case agent.StatusTimeout:
		return "failed", sentence(who + " ran out of time" + at + after)
	case agent.StatusBudget:
		return "failed", sentence(who + " reached its budget" + at + after)
	case agent.StatusCanceled:
		if where == "" {
			return "failed", sentence(who + "'s run was cancelled" + after)
		}
		return "failed", sentence(who + "'s run in " + where + " was cancelled" + after)
	case agent.StatusRefused:
		return "failed", sentence(who + " turned the task down" + at)
	case agent.StatusLimited:
		return "failed", sentence(who + " hit its usage limit" + at + after)
	}
	return "failed", sentence(who + " stopped" + at + after + ": " + string(res.Status))
}

func batchEnding(b batch.Result) (kind, text string) {
	pass := 0
	for _, t := range b.Tasks {
		if t.Status == batch.StatusPass {
			pass++
		}
	}
	n := len(b.Tasks)
	head := "The batch" + in(place(b.Dir)) + " is done"
	took := " It took " + spoken(b.Duration) + "."
	switch {
	case pass == n && n == 2:
		return "done", head + ": both tasks passed." + took
	case pass == n:
		return "done", head + ": all " + count(n, "task") + " passed." + took
	case pass == 0:
		return "failed", head + ", and none of its " + count(n, "task") + " passed." + took
	}
	return "failed", head + ": " + number(pass) + " of " + count(n, "task") + " passed." + took
}

// Prefer explicit IDs because batch prompts often share an opening paragraph.
func taskName(t batch.TaskResult) string {
	rest, made := strings.CutPrefix(t.ID, "t")
	if n, err := strconv.Atoi(rest); made && err == nil && n > 0 && strconv.Itoa(n) == rest || t.ID == "" {
		return topic(t.Prompt)
	}
	return t.ID
}

func activity(tool, summary string) string {
	tool, summary = mask.Text(tool), mask.Text(summary)
	t := strings.ToLower(tool)
	if _, after, ok := strings.Cut(t, "mcp__"); ok { // mcp__server__tool
		if i := strings.LastIndex(after, "__"); i >= 0 {
			after = after[i+2:]
		}
		return "using " + strings.ReplaceAll(after, "_", " ")
	}
	file := baseOf(strings.Fields(summary))
	if file == "" {
		file = "a file"
	}
	switch t {
	case "read", "read_file", "view", "notebookread", "read_many_files":
		return "reading " + file
	case "edit", "multiedit", "write", "notebookedit", "write_file", "edit_file", "apply_patch", "file_change", "create_file", "replace":
		return "editing " + file
	case "grep", "glob", "ls", "list_files", "list_directory", "search", "search_file_content", "find", "codebase_search", "file_search":
		return "searching the code"
	case "bash", "powershell", "run_command", "shell", "command_execution", "exec_command", "local_shell", "run_shell_command":
		return "running " + commandWords(summary)
	case "webfetch", "websearch", "web_search", "web_fetch", "fetch", "google_web_search":
		return "looking something up online"
	case "task", "agent":
		return "handing part of the job to a helper"
	case "todowrite", "update_plan", "todo", "todoread":
		return "updating its to-do list"
	}
	return "using " + strings.ReplaceAll(t, "_", " ")
}

func commandWords(line string) string {
	fields := strings.Fields(mask.Text(line))
	for len(fields) > 0 {
		f := fields[0]
		switch {
		case f == "cd" || f == "pushd" || f == "Set-Location":
			fields = skipPast(fields)
			continue
		case strings.Contains(f, "=") && !strings.HasPrefix(f, "-"):
			fields = fields[1:] // FOO=bar in front
			continue
		}
		break
	}
	if len(fields) == 0 {
		return "a command"
	}
	prog := strings.TrimSuffix(strings.ToLower(baseOf(fields[:1])), ".exe")
	if len(fields) > 1 && plainWord(fields[1]) {
		return prog + " " + fields[1]
	}
	return prog
}

func skipPast(fields []string) []string {
	for i, f := range fields {
		if f == "&&" || f == ";" || strings.HasSuffix(f, ";") && i > 0 {
			return fields[i+1:]
		}
	}
	return nil
}

func plainWord(s string) bool {
	if s == "" || utf8.RuneCountInString(s) > 20 {
		return false
	}
	for _, r := range s {
		if !unicode.IsLetter(r) && r != '-' || r == '-' && strings.HasPrefix(s, "-") {
			return false
		}
	}
	return true
}

func baseOf(words []string) string {
	if len(words) == 0 {
		return ""
	}
	w := strings.TrimRight(words[0], `/\`)
	if i := strings.LastIndexAny(w, `/\`); i >= 0 {
		w = w[i+1:]
	}
	return w
}

func backendName(b string) string {
	switch strings.ToLower(b) {
	case "claude":
		return "Claude"
	case "codex":
		return "Codex"
	case "gemini":
		return "Gemini"
	case "opencode":
		return "OpenCode"
	case "api":
		return "the API loop"
	case "":
		return "an agent"
	}
	return b
}

func place(dir string) string { return baseOf([]string{dir}) }

func in(where string) string {
	if where == "" {
		return ""
	}
	return " in " + where
}

func topic(prompt string) string {
	line, _ := firstLine(strings.TrimSpace(prompt))
	line = strings.TrimLeft(line, "#*->` \t")
	line = strings.TrimRight(words(line, 12), ".!:; ")
	r, size := utf8.DecodeRuneInString(line)
	next, _ := utf8.DecodeRuneInString(line[size:])
	word := unicode.IsLower(next) || r != 'I' && (len(line) == size || unicode.IsSpace(next))
	if size > 0 && unicode.IsUpper(r) && word {
		line = string(unicode.ToLower(r)) + line[size:]
	}
	return line
}

func words(s string, n int) string {
	f := strings.Fields(s)
	if len(f) <= n {
		return strings.Join(f, " ")
	}
	return strings.Join(f[:n], " ") + "…"
}

func sentence(s string) string {
	s = capital(strings.TrimSpace(mask.Text(s)))
	if s != "" && !strings.ContainsAny(s[len(s)-1:], ".?!") && !strings.HasSuffix(s, "…") {
		s += "."
	}
	return s
}

func capital(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if size == 0 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}

func spoken(d time.Duration) string {
	s := int(d.Round(time.Second) / time.Second)
	switch {
	case s <= 0:
		return "under a second"
	case s < 60:
		return count(s, "second")
	case s < 3600:
		return count(s/60, "minute")
	}
	h, m := s/3600, s/60%60
	if m == 0 {
		return count(h, "hour")
	}
	return count(h, "hour") + " and " + count(m, "minute")
}

func count(n int, unit string) string {
	if n == 1 {
		if unit == "hour" {
			return "an hour"
		}
		return "a " + unit
	}
	return number(n) + " " + unit + "s"
}

func number(n int) string {
	names := []string{"no", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten", "eleven", "twelve"}
	if n >= 0 && n < len(names) {
		return names[n]
	}
	return strconv.Itoa(n)
}

type botNow struct {
	Going []botRow
}

type botRow struct {
	ID, URL, Kind, Who, Where, What, Doing, Since string
}

func botNowOf(s botSnapshot) botNow {
	var now botNow
	for _, it := range s.items {
		if it.Going {
			now.Going = append(now.Going, botRow{it.ID, "/" + it.Kind + "/" + it.ID, it.Kind, capital(it.Who), it.Where,
				it.What, it.Doing, it.Started.Format(time.RFC3339Nano)})
		}
	}
	return now
}

type botPage struct {
	page
	Now   botNow
	Local bool // on this computer, where recording her voice works
	Kinds []botKind
}

type botWatcher struct {
	s         *server
	request   *http.Request
	followers map[string]*Follower
	doing     map[string]string
	paused    map[string]int64 // where each stopped run's transcript ended
	batches   []batch.Result   // the last look's, cleaned
	looked    bool
	began     time.Time
	late      map[string]bool // batches on disk the first look could not read
}

func (s *server) newBotWatcher(r *http.Request) *botWatcher {
	return &botWatcher{s: s, request: r, followers: map[string]*Follower{}, doing: map[string]string{}, paused: map[string]int64{}, late: map[string]bool{}}
}

// Retain unreadable files across Windows renames to avoid false new-batch announcements.
func keepUnread(prev, cur []batch.Result, onDisk map[string]bool) []batch.Result {
	have := map[string]bool{}
	for _, b := range cur {
		have[b.ID] = true
	}
	for _, b := range prev {
		if !have[b.ID] && onDisk[b.ID] {
			cur = append(cur, b)
		}
	}
	return cur
}

// Initially unreadable batches predate the stream; only later endings are news.
func (b *botWatcher) baseline(prev, cur botSnapshot) botSnapshot {
	if len(b.late) == 0 {
		return prev
	}
	items := slices.Clip(prev.items)
	for id := range b.late {
		it, ok := cur.find(id)
		if !ok {
			continue
		}
		delete(b.late, id)
		if _, seen := prev.find(id); !seen {
			if !it.Going && !it.Stopped && !it.ended.Before(b.began) {
				it.Going = true // so that its ending is said
			}
			items = append(items, it)
		}
	}
	return botSnapshot{items: items}
}

func unread(batches []batch.Result) map[string]bool {
	entries, _ := os.ReadDir(filepath.Join(runlog.Home(), "batches"))
	read := map[string]bool{}
	for _, b := range batches {
		read[b.ID] = true
	}
	out := map[string]bool{}
	for _, e := range entries {
		if id, ok := strings.CutSuffix(e.Name(), ".json"); ok && !e.IsDir() && !read[id] {
			out[id] = true
		}
	}
	return out
}

func (b *botWatcher) look() botSnapshot {
	who := b.s.who(b.request)
	runs, _ := runlog.ListFor(who, b.s.opt.Runs)
	// Every batch on disk: an attempt of an old batch is still its run.
	batches, unreadable, _ := b.s.listBatches()
	if unreadable > 0 {
		// Retry transient Windows rename failures before reporting an unreadable batch.
		time.Sleep(50 * time.Millisecond)
		batches, unreadable, _ = b.s.listBatches()
	}
	visible := batches[:0]
	for _, res := range batches {
		if who.CanRead(res.Owner) {
			visible = append(visible, res)
		}
	}
	batches = visible
	var onDisk map[string]bool
	if unreadable > 0 {
		onDisk = unread(batches)
	}
	if !b.looked {
		b.began = time.Now()
		if unreadable > 0 {
			b.late = maps.Clone(onDisk)
		}
	}
	b.looked = true
	vw := viewer{home: b.s.home, system: b.s.system}
	going, paused := map[string]bool{}, map[string]int64{}
	for i, rec := range runs {
		sh := vw.shortener(rec.Task.Dir)
		runs[i] = sh.record(b.s.cleanRecord(rec))
		if rec.Done {
			continue
		}
		if !runStopped(rec).IsZero() {
			// Resume at the stopped offset so old activity is not presented as current.
			paused[rec.ID] = b.stoppedAt(rec.ID)
			continue
		}
		going[rec.ID] = true
		f := b.followers[rec.ID]
		if f == nil {
			var err error
			if f, _, err = NewFollower(rec.ID, b.paused[rec.ID]); err != nil {
				continue
			}
			b.followers[rec.ID] = f
		}
		lines, _, _, _ := f.Poll()
		for _, l := range lines {
			if l.Err == nil && l.Event.Kind == agent.EventToolCall {
				e := sh.event(b.s.cleanEvent(l.Event))
				b.doing[rec.ID] = activity(e.Tool, e.Text)
			}
		}
	}
	for id := range b.followers {
		if !going[id] {
			delete(b.followers, id)
			delete(b.doing, id)
		}
	}
	b.paused = paused
	for i, res := range batches {
		batches[i] = vw.batch(res) // listBatches cleaned them already
	}
	b.batches = keepUnread(b.batches, batches, onDisk)
	return botLook(runs, b.batches, b.doing, time.Now())
}

func (b *botWatcher) stoppedAt(id string) int64 {
	if f := b.followers[id]; f != nil {
		return f.Offset()
	}
	if at, ok := b.paused[id]; ok {
		return at
	}
	if info, err := os.Stat(filepath.Join(runlog.Root(), id, "events.jsonl")); err == nil {
		return info.Size()
	}
	return 0
}

func (s *server) bot(w http.ResponseWriter, r *http.Request) {
	// The page may ask for the microphone, to listen for its few commands.
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(self), geolocation=()")
	snap := s.newBotWatcher(r).look()
	s.page(w, http.StatusOK, "bot", botPage{page: page{Title: "Ada", Home: s.homeFor(r), Script: "bot.js"}, Now: botNowOf(snap), Kinds: botKinds, Local: s.fromThisComputer(r)})
}

// Redact spoken JSON too: it bypasses the page template's masking.
func (s *server) streamBot(w http.ResponseWriter, r *http.Request) {
	st := s.startStream(w)
	b := s.newBotWatcher(r)
	var prev botSnapshot
	first, last := true, ""
	s.loop(r, st, 4*s.opt.Poll, func() (bool, error) {
		cur := b.look()
		html, err := s.render("botnow", botNowOf(cur))
		if err != nil {
			return true, s.problem(r, st, err)
		}
		if html != last {
			last = html
			if err := st.send("replace", "", html); err != nil {
				return true, err
			}
		}
		for _, m := range announce(b.baseline(prev, cur), cur, first) {
			m.Text = s.clean(m.Text)
			data, _ := json.Marshal(m)
			text := string(data)
			if s.redact != nil { // a backstop: the records were blanked already
				text = s.redact.Replace(text)
			}
			if err := st.send("say", "", text); err != nil {
				return true, err
			}
		}
		prev, first = cur, false
		return false, nil
	})
}
