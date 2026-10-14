package watch

import (
	"encoding/json"
	"fmt"
	"net/http"
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

// The bot: a small page that says, in plain UK English, what hrn is doing.
// Its stream looks at the runs and batches as the index does, every two
// seconds, and turns each change into a sentence; the page decides which ones
// to speak or to show as a notification. The sentences are made here, so
// they are tested and pass through the same redaction as every page. Nothing
// the bot hears or shows acts on hrn: that waits for #38.

// say is one thing the bot may say.
type say struct {
	Kind string `json:"kind"`          // one of botKinds
	Text string `json:"text"`          // the sentence
	Ref  string `json:"ref,omitempty"` // the run or batch it is about
	URL  string `json:"url,omitempty"` // that run's or batch's page
}

// botKind is one kind of message, with whether the page speaks it and
// notifies it until the person says otherwise.
type botKind struct {
	Name, Label   string
	Speak, Notify bool
}

// botKinds are the kinds, in the order the settings show them. A status is
// kept for "what are you doing?" and shown, not spoken on its own.
var botKinds = []botKind{
	{"started", "Something starts", true, false},
	{"working", "What an agent is doing", false, false},
	{"task", "A batch task finishes", true, false},
	{"done", "A run or batch is done", true, true},
	{"failed", "A run or batch stops short", true, true},
	{"status", "What is going, when that changes", false, false},
}

// settle is how long a new run is held back before it is announced: a batch
// creates its attempt's run record a moment before it saves its own record
// naming it, and the run would otherwise be announced as well as the batch.
const settle = 3 * time.Second

// botItem is one run or batch as the bot sees it at one look.
type botItem struct {
	ID, Kind   string // Kind is run or batch
	Who, Where string // who is working (a backend, or "a batch of four tasks"), and in which workspace
	What       string // the task, in a few words
	Going      bool
	Started    time.Time
	Doing      string // the latest activity of a run, or of a batch's running task
	ending     func() (kind, text string)
	tasks      []botTask
}

type botTask struct {
	ID, What string
	Status   batch.Status
}

// botSnapshot is everything at one look, newest first.
type botSnapshot struct{ items []botItem }

func (s botSnapshot) find(id string) (botItem, bool) {
	for _, it := range s.items {
		if it.ID == id {
			return it, true
		}
	}
	return botItem{}, false
}

// botLook makes a snapshot from the records. doing holds each running run's
// latest activity. A batch's own runs are left to the batch.
func botLook(runs []runlog.Record, batches []batch.Result, doing map[string]string, now time.Time) botSnapshot {
	owner := batchOf(batches)
	var snap botSnapshot
	for _, b := range batches {
		it := botItem{ID: b.ID, Kind: "batch", Where: place(b.Dir), Going: batchGoing(b), Started: b.Started}
		it.Who = "a batch of " + count(len(b.Tasks), "task")
		for _, t := range b.Tasks {
			it.tasks = append(it.tasks, botTask{t.ID, taskName(t), t.Status})
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

// announce is what changed between two looks, oldest first, then the status
// when it reads differently. The first look says only the status: what ended
// before the bot was opened is history, not news.
func announce(prev, cur botSnapshot, first bool) []say {
	status := say{Kind: "status", Text: statusOf(cur)}
	if first {
		return []say{status}
	}
	var out []say
	for i := len(cur.items) - 1; i >= 0; i-- {
		it := cur.items[i]
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
		if !it.Going && (!seen || was.Going) {
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
	return sentence(text)
}

// statusOf is the answer to "what are you doing?".
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

// runEnding says how a run ended, and whether that is news to be glad of.
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

// batchEnding says how a batch went, counting the tasks that passed.
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

// taskName is what a batch task is called: its id, unless hrn made that up
// (t1, t2, ... for a spec that gave none), then the start of its prompt. A
// batch's prompts often open with the same paragraph; their ids differ.
func taskName(t batch.TaskResult) string {
	rest, made := strings.CutPrefix(t.ID, "t")
	if n, err := strconv.Atoi(rest); made && err == nil && n > 0 && strconv.Itoa(n) == rest || t.ID == "" {
		return topic(t.Prompt)
	}
	return t.ID
}

// activity is what a tool call shows an agent doing, in a few words. The
// tools' names differ by backend; the summary is the call's one line.
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

// commandWords is a command line as one would say it: the program and what
// it was asked to do, without paths, settings or a cd in front.
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

// skipPast drops a cd and its directory, up to the && or ; after it.
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

// baseOf is the last element of the first word, a path in either style.
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

// backendName is a backend as people say it.
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

// place is a workspace by its folder's name.
func place(dir string) string { return baseOf([]string{dir}) }

func in(where string) string {
	if where == "" {
		return ""
	}
	return " in " + where
}

// topic is a task in a few words: its first line, without markdown in front
// or a full stop behind, starting small unless it starts with a name, an
// acronym or "I".
func topic(prompt string) string {
	line, _ := firstLine(strings.TrimSpace(prompt))
	line = strings.TrimLeft(line, "#*->` \t")
	line = strings.TrimRight(words(line, 12), ".!:; ")
	r, size := utf8.DecodeRuneInString(line)
	if next, _ := utf8.DecodeRuneInString(line[size:]); size > 0 && unicode.IsUpper(r) && !unicode.IsUpper(next) && !(r == 'I' && !unicode.IsLetter(next)) {
		line = string(unicode.ToLower(r)) + line[size:]
	}
	return line
}

// words keeps the first n words of s, marking a cut with an ellipsis.
func words(s string, n int) string {
	f := strings.Fields(s)
	if len(f) <= n {
		return strings.Join(f, " ")
	}
	return strings.Join(f[:n], " ") + "…"
}

// sentence starts with a capital and ends with a full stop.
func sentence(s string) string {
	s = capital(strings.TrimSpace(mask.Text(s)))
	if s != "" && !strings.ContainsAny(s[len(s)-1:], ".?!") && !strings.HasSuffix(s, "…") {
		s += "."
	}
	return s
}

// capital starts s with a capital letter.
func capital(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if size == 0 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}

// spoken is a duration as one would say it.
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

// botNow is the visible part of the bot page: what is going.
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

// look reads the records as the index does and asks each running run what it
// is doing, keeping one follower per running run between looks.
type botWatcher struct {
	s         *server
	followers map[string]*Follower
	doing     map[string]string
	batches   []batch.Result // the last look's, cleaned
}

func (s *server) newBotWatcher() *botWatcher {
	return &botWatcher{s: s, followers: map[string]*Follower{}, doing: map[string]string{}}
}

// keepUnread keeps the last look's batches that this look could not read. A
// running batch rewrites its record as each task moves on, and on Windows a
// read can land on the rename: the batch would vanish for one look, then it
// and its runs would be announced again as new. A batch removed for good is
// not kept, as then every record reads.
func keepUnread(prev, cur []batch.Result, unreadable int) []batch.Result {
	if unreadable == 0 {
		return cur
	}
	have := map[string]bool{}
	for _, b := range cur {
		have[b.ID] = true
	}
	for _, b := range prev {
		if !have[b.ID] {
			cur = append(cur, b)
		}
	}
	return cur
}

func (b *botWatcher) look() botSnapshot {
	runs, _ := runlog.List(b.s.opt.Runs)
	// Every batch on disk: an attempt of an old batch is still its run.
	batches, unreadable, _ := batch.List()
	if unreadable > 0 {
		// As the index does: most likely one caught mid-save. Look again.
		time.Sleep(50 * time.Millisecond)
		batches, unreadable, _ = batch.List()
	}
	going := map[string]bool{}
	for i, rec := range runs {
		runs[i] = b.s.cleanRecord(rec)
		if rec.Done {
			continue
		}
		going[rec.ID] = true
		f := b.followers[rec.ID]
		if f == nil {
			var err error
			if f, _, err = NewFollower(rec.ID, 0); err != nil {
				continue
			}
			b.followers[rec.ID] = f
		}
		lines, _, _, _ := f.Poll()
		for _, l := range lines {
			if l.Err == nil && l.Event.Kind == agent.EventToolCall {
				e := b.s.cleanEvent(l.Event)
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
	for i, res := range batches {
		batches[i] = b.s.cleanBatch(res)
	}
	b.batches = keepUnread(b.batches, batches, unreadable)
	return botLook(runs, b.batches, b.doing, time.Now())
}

func (s *server) bot(w http.ResponseWriter, r *http.Request) {
	// The page may ask for the microphone, to listen for its few commands.
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(self), geolocation=()")
	snap := s.newBotWatcher().look()
	s.page(w, http.StatusOK, "bot", botPage{page: page{Title: "Ada", Home: s.homeFor(r), Script: "bot.js"}, Now: botNowOf(snap), Kinds: botKinds, Local: s.fromThisComputer(r)})
}

// streamBot sends the visible part when it changes, and each thing to say as
// JSON, blanked of secrets like every page.
func (s *server) streamBot(w http.ResponseWriter, r *http.Request) {
	st := s.startStream(w)
	b := s.newBotWatcher()
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
		for _, m := range announce(prev, cur, first) {
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
