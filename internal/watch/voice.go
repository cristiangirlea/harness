package watch

import (
	"bufio"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/batch"
)

// Recording Ada's voice (#83, #91): /voice shows the lines to read, one at a
// time, and the page records them in the browser and saves them into a folder
// the person picks. Nothing is sent back here, so hrn watch stays read-only.
// The page is for this computer only: it uses the microphone, and the takes
// are someone's voice.

// voiceLine is one line to record. N is its number in the files' names
// (0001.wav) and in metadata.csv, so it must not change for a line once
// people have recorded it: the lines are made in a fixed order.
type voiceLine struct {
	N    int
	Text string
}

// voiceLines are the lines: Ada's own sentences, made by the functions she
// speaks with over many backends, places, tasks and durations, so the voice is
// best at what she says, mixed with everyday lines covering English's sounds.
var voiceLines = sync.OnceValue(func() []voiceLine {
	var everyday []string
	sc := bufio.NewScanner(strings.NewReader(mustAsset("assets/voice-lines.txt")))
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" && !strings.HasPrefix(l, "#") {
			everyday = append(everyday, l)
		}
	}
	ada := adaLines()
	// One everyday line, then three of Ada's, so every session has both.
	var out []voiceLine
	seen := map[string]bool{}
	add := func(s string) {
		if s = strings.ReplaceAll(s, "|", ","); s != "" && !seen[s] {
			seen[s] = true
			out = append(out, voiceLine{len(out) + 1, s})
		}
	}
	for i := 0; i < len(everyday) || 3*i < len(ada); i++ {
		if i < len(everyday) {
			add(everyday[i])
		}
		for j := 3 * i; j < 3*i+3 && j < len(ada); j++ {
			add(ada[j])
		}
	}
	return out
})

func mustAsset(name string) string {
	data, err := assets.ReadFile(name)
	if err != nil {
		panic(err)
	}
	return string(data)
}

// adaLines are Ada's sentences, from her own wording functions.
func adaLines() []string {
	backends := []string{"claude", "codex", "gemini", "opencode", "api"}
	places := []string{"harness", "tidedesk", "jobpilot", "website", "infra", "rendezvous", "commercial"}
	tasks := []string{
		"add the budget guard", "fix the flaky clipboard test", "translate the settings page",
		"name a batch's runs by their task", "record Ada's voice", "check the licence on startup",
		"speed up the screen capture", "add a dark theme to the website", "move the gates to bash",
		"review the security findings", "pin the base images", "look up the employer links",
		"write the release notes", "tidy the README", "split the rendezvous service",
		"show the cost on the watch page", "retry a failed gate once", "map the whole workflow",
		"test the keyboard switch", "add German questions", "catch a batch mid-save",
		"warn before the disk fills up", "teach Ada to listen", "make the phone page faster",
	}
	minutes := []time.Duration{12 * time.Second, 45 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute,
		7 * time.Minute, 12 * time.Minute, 20 * time.Minute, 35 * time.Minute, time.Hour, 65 * time.Minute, 150 * time.Minute}
	type call struct{ tool, summary string }
	calls := []call{
		{"Read", "internal/watch/server.go"}, {"Edit", "internal/watch/bot.go"}, {"Read", "README.md"},
		{"Edit", "src/main.rs"}, {"Read", "Cargo.toml"}, {"Write", "package.json"}, {"Edit", "dist/index.html"},
		{"Edit", "styles.css"}, {"Read", "AGENTS.md"}, {"Edit", "internal/tools/guard.go"},
		{"Bash", "go test ./..."}, {"Bash", "cargo build --release"}, {"Bash", "npm run build"}, {"Bash", "git status"},
		{"Bash", "make check"}, {"PowerShell", "Get-ChildItem"}, {"Grep", "func announce"}, {"WebFetch", "https://example.com"},
		{"Task", "review the diff"}, {"TodoWrite", ""}, {"Bash", "docker build ."}, {"Bash", "kubectl get pods"},
	}
	var out []string
	pick := func(i int, xs []string) string { return xs[i%len(xs)] }
	who := func(i int) string { return backendName(pick(i, backends)) }
	place := func(i int) string { return pick(i*3+1, places) }
	// First, the examples the README gives of what she says, by the same
	// functions, so the most common lines are recorded early.
	_, finished := runEnding("Claude", "harness", agent.Result{Status: agent.StatusOK, Duration: 4 * time.Minute})
	four := botItem{Kind: "batch", Where: "tidedesk"}
	for k := 0; k < 4; k++ {
		four.tasks = append(four.tasks, botTask{What: "fix the flaky test", Status: batch.StatusRunning})
	}
	four.tasks[1].Status = batch.StatusPass
	ended := batch.Result{Dir: "/work/tidedesk", Duration: 12 * time.Minute, Tasks: []batch.TaskResult{
		{Status: batch.StatusPass}, {Status: batch.StatusPass}, {Status: batch.StatusPass}, {Status: batch.StatusFail}}}
	_, batchDone := batchEnding(ended)
	out = append(out,
		startedOf(botItem{Kind: "run", Who: "Claude", Where: "harness", What: "add the budget guard"}),
		workingOf(botItem{Kind: "run", Who: "Claude", Where: "harness", Doing: activity("Edit", "internal/watch/bot.go")}),
		taskOf(four, 1), finished, batchDone,
		statusOf(botSnapshot{items: []botItem{
			{Who: "Claude", Where: "harness", Doing: "editing bot.go", Going: true},
			{Who: "a batch of four tasks", Where: "tidedesk", Going: true}}}),
	)
	// Starts.
	for i := 0; i < 60; i++ {
		out = append(out, startedOf(botItem{Kind: "run", Who: who(i), Where: place(i), What: tasks[i%len(tasks)]}))
	}
	// What an agent is doing, alone and in a batch.
	for i := 0; i < 90; i++ {
		c := calls[i%len(calls)]
		doing := activity(c.tool, c.summary)
		if i%5 == 4 {
			out = append(out, workingOf(botItem{Kind: "batch", Where: place(i), Doing: who(i) + " is " + doing}))
			continue
		}
		out = append(out, workingOf(botItem{Kind: "run", Who: who(i), Where: place(i), Doing: doing}))
	}
	// How a run ended.
	statuses := []agent.Status{agent.StatusOK, agent.StatusError, agent.StatusMaxTurns, agent.StatusTimeout,
		agent.StatusBudget, agent.StatusCanceled, agent.StatusRefused, agent.StatusLimited}
	errs := []string{"exit status 1", "the build failed", "a test timed out", "the network went away"}
	for i := 0; i < 72; i++ {
		res := agent.Result{Status: statuses[i%len(statuses)], Duration: minutes[(i*5)%len(minutes)]}
		if res.Status == agent.StatusError {
			res.Error = errs[(i/len(statuses))%len(errs)]
		}
		_, text := runEnding(who(i/len(statuses)), place(i), res)
		out = append(out, text)
	}
	// Batches: starting, a task finishing, and how they ended.
	taskStatuses := []batch.Status{batch.StatusPass, batch.StatusFail, batch.StatusPass, batch.StatusError, batch.StatusCanceled}
	for n := 1; n <= 12; n++ {
		out = append(out, startedOf(botItem{Kind: "batch", Who: "a batch of " + count(n, "task"), Where: place(n)}))
		out = append(out, startedOf(botItem{Kind: "batch", Who: "a batch of " + count(n, "task"), Where: place(n + 2)}))
	}
	for i := 0; i < 64; i++ {
		m := 2 + i%7
		it := botItem{Kind: "batch", Where: place(i)}
		for k := 0; k < m; k++ {
			it.tasks = append(it.tasks, botTask{What: tasks[(i+k)%len(tasks)], Status: batch.StatusRunning})
		}
		k := i % m
		it.tasks[k].Status = taskStatuses[i%len(taskStatuses)]
		out = append(out, taskOf(it, k))
	}
	for i := 0; i < 32; i++ {
		total := 1 + i%9
		pass := (i * 7) % (total + 1)
		b := batch.Result{Dir: "/work/" + place(i), Duration: minutes[(i*3)%len(minutes)]}
		for k := 0; k < total; k++ {
			st := batch.StatusFail
			if k < pass {
				st = batch.StatusPass
			}
			b.Tasks = append(b.Tasks, batch.TaskResult{Status: st})
		}
		_, text := batchEnding(b)
		out = append(out, text)
	}
	// What is going.
	out = append(out, statusOf(botSnapshot{}))
	for i := 0; i < 36; i++ {
		var snap botSnapshot
		for k := 0; k <= i%3; k++ { // up to three, or the line is too long for one breath
			j := i + k
			it := botItem{Kind: "run", Who: who(j), Where: place(j), Going: true}
			if k%2 == 1 {
				it = botItem{Kind: "batch", Who: "a batch of " + count(2+j%5, "task"), Where: place(j), Going: true}
			}
			if (i+k)%3 == 0 {
				it.Doing = activity(calls[j%len(calls)].tool, calls[j%len(calls)].summary)
			}
			snap.items = append(snap.items, it)
		}
		out = append(out, statusOf(snap))
	}
	// Her replies on the page (assets/bot.js).
	out = append(out,
		"I'm Ada. I'll tell you what hrn is doing.",
		"Hello, I'm Ada. This is how I sound. I'll tell you when something starts, and when it's done.",
		"I'll keep quiet until you unmute me.", "Quiet for an hour.", "Quiet for 15 minutes.",
		"Right, I'll tell you again.",
		"Sorry, I didn't catch that. You can say status, skip, quiet, unmute or repeat.",
	)
	return out
}

type voicePage struct {
	page
	Lines []voiceLine
}

// voice serves the recording page to this computer only.
func (s *server) voice(w http.ResponseWriter, r *http.Request) {
	if !fromThisComputer(r) {
		s.page(w, http.StatusForbidden, "missing", struct {
			page
			Message string
		}{page{Title: "This computer only"}, "Recording Ada's voice works only on the computer hrn watch runs on: it uses the microphone, and the recordings are private."})
		return
	}
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(self), geolocation=()")
	s.page(w, http.StatusOK, "voice", voicePage{page: page{Title: "Record Ada's voice", Home: homeFor(r), Script: "voice.js"}, Lines: voiceLines()})
}
