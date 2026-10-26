package ws

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// ghWorld answers the gh calls the issue lease makes, from canned replies.
type ghWorld struct {
	prs      string            // pr list --search
	headPRs  string            // pr list --head
	comments string            // issue view --json comments
	after    string            // issue view once a comment was posted, when set
	branches map[string]string // branch: its last commit date; absent answers 404
	state    string            // pr view --json state
	fail     error             // every call fails with it
	headFail error             // pr list --head fails with it
	bodyFail string            // a comment whose body starts with it fails
	login    string            // the account gh acts as; default fakeLogin

	mu     sync.Mutex
	calls  [][]string
	bodies []string // what each --body-file held
}

func useGH(t *testing.T, w *ghWorld) *ghWorld {
	t.Helper()
	old := GH
	GH = w.run
	t.Cleanup(func() { GH = old })
	return w
}

func (w *ghWorld) run(_ context.Context, args ...string) ([]byte, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, args)
	if i := slices.Index(args, "--body-file"); i >= 0 {
		b, err := os.ReadFile(args[i+1])
		if err != nil {
			return nil, err
		}
		w.bodies = append(w.bodies, string(b))
		if w.bodyFail != "" && strings.HasPrefix(string(b), w.bodyFail) {
			return nil, errors.New("gh issue comment: HTTP 502")
		}
	}
	if w.fail != nil {
		return nil, w.fail
	}
	or := func(s, d string) ([]byte, error) {
		if s == "" {
			s = d
		}
		return []byte(s), nil
	}
	switch {
	case args[0] == "pr" && args[1] == "list" && slices.Contains(args, "--search"):
		return or(w.prs, "[]")
	case args[0] == "pr" && args[1] == "list" && slices.Contains(args, "--head"):
		if w.headFail != nil {
			return nil, w.headFail
		}
		return or(w.headPRs, "[]")
	case args[0] == "issue" && args[1] == "view":
		if w.after != "" && len(w.bodies) > 0 {
			return byOwner(w.after)
		}
		c, _ := or(w.comments, `{"comments":[]}`)
		return byOwner(string(c))
	case args[0] == "api" && args[1] == "user":
		return or(w.login, fakeLogin)
	case args[0] == "api":
		branch := args[1][strings.LastIndex(args[1], "/branches/")+len("/branches/"):]
		if d, ok := w.branches[branch]; ok {
			return []byte(d + "\n"), nil
		}
		return nil, errors.New("gh api repos: HTTP 404: Branch not found")
	case args[0] == "pr" && args[1] == "view":
		return []byte(w.state + "\n"), nil
	}
	return nil, nil
}

const fakeLogin = "owner"

// byOwner gives canned comments without an author to the account gh acts as.
func byOwner(doc string) ([]byte, error) {
	var d struct {
		Comments []map[string]any `json:"comments"`
	}
	if err := json.Unmarshal([]byte(doc), &d); err != nil {
		return nil, err
	}
	for _, c := range d.Comments {
		if _, ok := c["author"]; !ok {
			c["author"] = map[string]string{"login": fakeLogin}
		}
	}
	return json.Marshal(d)
}

func (w *ghWorld) called(prefix ...string) [][]string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out [][]string
	for _, c := range w.calls {
		if len(c) >= len(prefix) && slices.Equal(c[:len(prefix)], prefix) {
			out = append(out, c)
		}
	}
	return out
}

var issue12 = IssueRef{Repo: "o/n", Number: 12}

func runLease(run string) Lease {
	return Lease{Holder: "hrn run", Run: run, Backend: "claude", Branch: "fix-x"}
}

func writeIssueLease(t *testing.T, home string, ref IssueRef, l Lease) {
	t.Helper()
	if err := os.MkdirAll(IssuesDir(home), 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(l)
	if err := os.WriteFile(issueLeasePath(home, ref), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readIssueLease(t *testing.T, home string, ref IssueRef) (Lease, bool) {
	t.Helper()
	l, _, err := readLease(issueLeasePath(home, ref))
	if errors.Is(err, os.ErrNotExist) {
		return Lease{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return l, true
}

func ago(d time.Duration) string { return time.Now().Add(-d).UTC().Format(time.RFC3339) }

func TestParseIssueAcceptsTheThreeForms(t *testing.T) {
	https, ssh := t.TempDir(), t.TempDir()
	for dir, url := range map[string]string{https: "https://github.com/o/n.git", ssh: "git@github.com:o/n"} {
		for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", url}} {
			if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v %s", args, err, out)
			}
		}
	}
	for _, tc := range []struct{ in, dir string }{
		{"o/n#12", ""}, {"https://github.com/o/n/issues/12", ""}, {"https://github.com/o/n/issues/12/", ""},
		{"#12", https}, {"12", https}, {"#12", ssh},
		{"O/N#12", ""}, {"https://github.com/O/n/issues/12", ""},
	} {
		got, err := ParseIssue(tc.in, tc.dir)
		if err != nil || got != issue12 {
			t.Errorf("ParseIssue(%q) = %v, %v; want o/n#12", tc.in, got, err)
		}
	}
	if got := issue12.String(); got != "o/n#12" {
		t.Errorf("String = %q", got)
	}
	for _, bad := range []string{"o/n#0", "o/n", "n#12", "https://github.com/o/n/pull/12", "twelve", ""} {
		if _, err := ParseIssue(bad, https); err == nil || !strings.Contains(err.Error(), "owner/name#N") {
			t.Errorf("ParseIssue(%q) must be refused naming the forms, got %v", bad, err)
		}
	}
	if _, err := ParseIssue("#12", t.TempDir()); err == nil || !strings.Contains(err.Error(), "origin") {
		t.Errorf("#N without an origin must say so: %v", err)
	}
}

func TestTakeIssueRefusesASecondRunUntilTakenOver(t *testing.T) {
	home := t.TempDir()
	first, err := TakeIssue(context.Background(), home, issue12, runLease("r1"), IssueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Same process, another run: two runs in one MCP server are two holders.
	_, err = TakeIssue(context.Background(), home, issue12, runLease("r2"), IssueOptions{})
	var held *IssueHeldError
	if !errors.As(err, &held) {
		t.Fatalf("a second run took a held issue: %v", err)
	}
	for _, want := range []string{"o/n#12", "run r1", "backend claude", "branch fix-x", "since", "--take-over", "take_over"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err, want)
		}
	}
	second, err := TakeIssue(context.Background(), home, issue12, runLease("r2"), IssueOptions{TakeOver: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Notes) == 0 || !strings.Contains(second.Notes[0], "run r1") {
		t.Errorf("the takeover must name who it replaced: %v", second.Notes)
	}
	if l, ok := readIssueLease(t, home, issue12); !ok || l.Run != "r2" || l.Issue != "o/n#12" || l.PID != os.Getpid() {
		t.Errorf("lease after takeover = %+v, %t", l, ok)
	}
	if err := first.Release(context.Background(), "run r1 ended"); err != nil {
		t.Errorf("the replaced holder's release must leave the new lease quietly: %v", err)
	}
	if l, ok := readIssueLease(t, home, issue12); !ok || l.Run != "r2" {
		t.Errorf("the replaced holder removed the new lease: %+v, %t", l, ok)
	}
	if err := second.Release(context.Background(), "run r2 ended"); err != nil {
		t.Fatal(err)
	}
	if _, ok := readIssueLease(t, home, issue12); ok {
		t.Error("release left the lease")
	}
	if err := second.Release(context.Background(), "again"); err != nil {
		t.Errorf("a second release must do nothing: %v", err)
	}
}

func TestIssueLeaseOfAnExitedProcessIsTaken(t *testing.T) {
	home := t.TempDir()
	pid, start := exitedPID(t)
	l := runLease("old")
	l.PID, l.Start, l.Taken = pid, start, time.Now()
	writeIssueLease(t, home, issue12, l)
	if got, _ := ListIssues(context.Background(), home); len(got) != 1 || got[0].State != "stale" {
		t.Errorf("an exited holder must list as stale: %+v", got)
	}
	if _, ok := readIssueLease(t, home, issue12); !ok {
		t.Error("listing removed a stale process lease")
	}
	if _, err := TakeIssue(context.Background(), home, issue12, runLease("new"), IssueOptions{}); err != nil {
		t.Errorf("an exited holder's lease must be taken: %v", err)
	}
}

func TestIssueLeaseWithReusedPIDIsTaken(t *testing.T) {
	id, known := startID(os.Getpid())
	if !known {
		t.Skip("process start identity is unavailable on this platform")
	}
	other := strings.SplitN(id, ":", 2)[0] + ":1"
	if other == id {
		other += "0"
	}
	home := t.TempDir()
	l := runLease("old")
	l.PID, l.Start, l.Taken = os.Getpid(), other, time.Now()
	writeIssueLease(t, home, issue12, l)
	if got, _ := ListIssues(context.Background(), home); len(got) != 1 || got[0].State != "stale" || !strings.Contains(got[0].Why, "another process") {
		t.Errorf("a reused pid must list as stale: %+v", got)
	}
	if _, err := TakeIssue(context.Background(), home, issue12, runLease("new"), IssueOptions{}); err != nil {
		t.Errorf("a lease whose pid was reused must be taken: %v", err)
	}
}

func TestClaimAndReleaseCommentsHaveTheSharedFormat(t *testing.T) {
	home := t.TempDir()
	w := useGH(t, &ghWorld{})
	il, err := TakeIssue(context.Background(), home, issue12, runLease("r1"), IssueOptions{GitHub: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(il.Notes) != 0 {
		t.Errorf("notes = %v", il.Notes)
	}
	label := []string{"label", "create", "claimed", "-R", "o/n", "--color", "FBCA04", "--force", "--description", "A worker is on this issue; see the Claim comment"}
	if got := w.called("label", "create"); len(got) != 1 || !slices.Equal(got[0], label) {
		t.Errorf("label create = %q", got)
	}
	if got := w.called("issue", "edit"); len(got) != 1 || !slices.Equal(got[0], []string{"issue", "edit", "12", "-R", "o/n", "--add-label", "claimed"}) {
		t.Errorf("issue edit = %q", got)
	}
	if len(w.bodies) != 1 || w.bodies[0] != "Claim: hrn r1 (claude) on branch `fix-x`\n" {
		t.Errorf("claim comment = %q", w.bodies)
	}
	// The claim is open on GitHub now; the release must find none after its own Released.
	w.comments = `{"comments":[{"body":"Claim: hrn r1 (claude) on branch ` + "`fix-x`" + `","createdAt":"` + ago(time.Minute) + `"},` +
		`{"body":"Released: hrn r1 (claude)\n\nrun r1 ended","createdAt":"` + ago(0) + `"}]}`
	if err := il.Release(context.Background(), "run r1 ended"); err != nil {
		t.Fatal(err)
	}
	if len(w.bodies) != 2 || w.bodies[1] != "Released: hrn r1 (claude)\n\nrun r1 ended\n" {
		t.Errorf("release comment = %q", w.bodies)
	}
	if got := w.called("issue", "edit", "12", "-R", "o/n", "--remove-label", "claimed"); len(got) != 1 {
		t.Errorf("the label must go once no claim is open: %q", w.calls)
	}
	if _, ok := readIssueLease(t, home, issue12); ok {
		t.Error("release without a pull request left the lease")
	}
	n := len(w.calls)
	if err := il.Release(context.Background(), "again"); err != nil || len(w.calls) != n {
		t.Errorf("a second release must do nothing: %v, %d calls", err, len(w.calls)-n)
	}
}

func TestReleaseKeepsTheLabelWhileAnotherClaimIsOpen(t *testing.T) {
	home := t.TempDir()
	w := useGH(t, &ghWorld{})
	il, err := TakeIssue(context.Background(), home, issue12, runLease("r1"), IssueOptions{GitHub: true})
	if err != nil {
		t.Fatal(err)
	}
	w.comments = `{"comments":[{"body":"Claim: owner on branch ` + "`other`" + `","createdAt":"` + ago(time.Minute) + `"}]}`
	if err := il.Release(context.Background(), "done"); err != nil {
		t.Fatal(err)
	}
	if got := w.called("issue", "edit", "12", "-R", "o/n", "--remove-label", "claimed"); len(got) != 0 {
		t.Errorf("the label went while another claim is open: %q", got)
	}
}

func TestReleaseWithAnOpenPullRequestLeavesTheLeaseToIt(t *testing.T) {
	home := t.TempDir()
	w := useGH(t, &ghWorld{headPRs: `[{"url":"https://github.com/o/n/pull/7"}]`})
	il, err := TakeIssue(context.Background(), home, issue12, runLease("r1"), IssueOptions{GitHub: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := il.Release(context.Background(), "run r1 ended"); err != nil {
		t.Fatal(err)
	}
	l, ok := readIssueLease(t, home, issue12)
	if !ok || l.PID != 0 || l.Start != "" || l.PR != "https://github.com/o/n/pull/7" || l.Run != "r1" {
		t.Fatalf("parked lease = %+v, %t", l, ok)
	}
	if len(w.bodies) != 1 {
		t.Errorf("a parked lease posts no Released comment: %q", w.bodies)
	}

	w.state = "OPEN"
	if got, _ := ListIssues(context.Background(), home); len(got) != 1 || got[0].State != "held by pull request" || got[0].PR == "" {
		t.Errorf("an open pull request holds the issue: %+v", got)
	}
	if _, err := TakeIssue(context.Background(), home, issue12, runLease("r2"), IssueOptions{}); err == nil || !strings.Contains(err.Error(), "pull/7") {
		t.Errorf("an open pull request must refuse the take, naming it: %v", err)
	}
	w.fail = errors.New("gh: not logged in")
	if _, err := TakeIssue(context.Background(), home, issue12, runLease("r2"), IssueOptions{}); err == nil {
		t.Error("a failing gh must keep the pull request's lease held")
	}
	w.fail = nil

	w.state = "MERGED"
	before := len(w.called("issue"))
	got, _ := ListIssues(context.Background(), home)
	if len(got) != 1 || got[0].State != "released" {
		t.Errorf("a merged pull request releases the issue: %+v", got)
	}
	if _, ok := readIssueLease(t, home, issue12); !ok {
		t.Error("listing must leave the lease to the next take, which releases its GitHub claim")
	}
	if got := w.called("issue"); len(got) != before {
		t.Errorf("listing must not touch the issue on GitHub: %q", got[before:])
	}

	w.state = "CLOSED"
	taken, err := TakeIssue(context.Background(), home, issue12, runLease("r3"), IssueOptions{})
	if err != nil {
		t.Fatalf("a closed pull request frees the issue: %v", err)
	}
	if len(taken.Notes) == 0 || !strings.Contains(taken.Notes[0], "pull/7") {
		t.Errorf("notes = %v", taken.Notes)
	}
}

func TestOpenPullRequestsThatCloseTheIssueRefuseTheTake(t *testing.T) {
	pr := func(head, body, closing string) string {
		return `[{"number":5,"url":"https://github.com/o/n/pull/5","headRefName":"` + head + `","body":` + jsonText(body) +
			`,"closingIssuesReferences":[` + closing + `]}]`
	}
	for _, tc := range []struct {
		name, prs string
		held      bool
	}{
		{"keyword", pr("other", "Fixes #12", ""), true},
		{"keyword with colon", pr("other", "closes: o/n#12", ""), true},
		{"issue url", pr("other", "Resolved https://github.com/o/n/issues/12", ""), true},
		{"linked by GitHub", pr("other", "", `{"number":12,"url":"https://github.com/o/n/issues/12"}`), true},
		{"bare mention", pr("other", "see #12 for context", ""), false},
		{"another issue", pr("other", "Fixes #123", ""), false},
		{"another repository", pr("other", "Fixes x/y#12", ""), false},
		{"own branch", pr("fix-x", "Fixes #12", ""), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			useGH(t, &ghWorld{prs: tc.prs})
			_, err := TakeIssue(context.Background(), home, issue12, runLease("r1"), IssueOptions{GitHub: true})
			var held *IssueHeldError
			if tc.held != errors.As(err, &held) {
				t.Fatalf("held = %t, err = %v", tc.held, err)
			}
			if _, ok := readIssueLease(t, home, issue12); ok == tc.held {
				t.Errorf("a refused take must leave no local lease, a granted one keeps it (lease present %t)", ok)
			}
			if tc.held && !strings.Contains(err.Error(), "pull/5") {
				t.Errorf("refusal must name the pull request: %v", err)
			}
		})
	}
}

func jsonText(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestGitHubClaimsRefuseUnlessStaleOrReleased(t *testing.T) {
	claim := func(worker, branch, at string) string {
		return `{"body":"Claim: ` + worker + ` on branch ` + "`" + branch + "`" + `","createdAt":"` + at + `"}`
	}
	released := func(worker, at string) string {
		return `{"body":"Released: ` + worker + `\n\nPR closed","createdAt":"` + at + `"}`
	}
	for _, tc := range []struct {
		name, comments string
		branches       map[string]string
		held           bool
	}{
		{"live claim", claim("hrn other (codex)", "b", ago(time.Hour)), nil, true},
		{"old claim on a busy branch", claim("owner", "b", ago(48*time.Hour)), map[string]string{"b": ago(time.Hour)}, true},
		{"old claim on a quiet branch", claim("owner", "b", ago(48*time.Hour)), map[string]string{"b": ago(30 * time.Hour)}, false},
		{"old claim with no branch", claim("owner", "b", ago(48*time.Hour)), nil, false},
		{"released claim", claim("owner", "b", ago(time.Hour)) + "," + released("owner", ago(time.Minute)), nil, false},
		{"another worker's release", claim("owner", "b", ago(time.Hour)) + "," + released("someone", ago(time.Minute)), nil, true},
		{"a comment that only quotes a claim", `{"body":"> Claim: owner on branch ` + "`b`" + `","createdAt":"` + ago(time.Hour) + `"}`, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useGH(t, &ghWorld{comments: `{"comments":[` + tc.comments + `]}`, branches: tc.branches})
			_, err := TakeIssue(context.Background(), t.TempDir(), issue12, runLease("r1"), IssueOptions{GitHub: true})
			var held *IssueHeldError
			if tc.held != errors.As(err, &held) {
				t.Fatalf("held = %t, err = %v", tc.held, err)
			}
		})
	}
}

func TestTakeOverOfAGitHubClaimSaysWhomItReplaces(t *testing.T) {
	w := useGH(t, &ghWorld{comments: `{"comments":[{"body":"Claim: owner on branch ` + "`b`" + `","createdAt":"` + ago(time.Hour) + `"}]}`})
	if _, err := TakeIssue(context.Background(), t.TempDir(), issue12, runLease("r1"), IssueOptions{GitHub: true, TakeOver: true}); err != nil {
		t.Fatal(err)
	}
	if len(w.bodies) != 2 || !strings.HasPrefix(w.bodies[0], "Claim: hrn r1 (claude) on branch `fix-x`\nTakes over from ") ||
		!strings.Contains(w.bodies[0], "owner on branch b") || !strings.HasSuffix(w.bodies[0], ".\n") {
		t.Errorf("takeover comment = %q", w.bodies)
	}
	if len(w.bodies) == 2 && w.bodies[1] != "Released: owner\n\ntaken over by hrn r1 (claude)\n" {
		t.Errorf("the replaced claim must be released: %q", w.bodies[1])
	}
}

func claimOf(worker, branch string, age time.Duration) string {
	return `{"body":"Claim: ` + worker + ` on branch ` + "`" + branch + "`" + `","createdAt":"` + ago(age) + `"}`
}

func TestATakeReleasesTheGitHubClaimOfTheHolderItReplaces(t *testing.T) {
	pid, start := exitedPID(t)
	for _, tc := range []struct {
		name     string
		old      func() Lease
		state    string
		takeOver bool
		reason   string
	}{
		{"exited process", func() Lease { l := runLease("old"); l.PID, l.Start = pid, start; return l }, "", false, "its process ended"},
		{"merged pull request", func() Lease { l := runLease("old"); l.PR = "https://github.com/o/n/pull/7"; return l }, "MERGED", false,
			"pull request https://github.com/o/n/pull/7 is merged or closed"},
		{"live holder taken over", func() Lease { l := runLease("old"); l.PID, l.Start = os.Getpid(), ""; return l }, "", true, "taken over by hrn new (claude)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			old := tc.old()
			old.Taken = time.Now()
			writeIssueLease(t, home, issue12, old)
			w := useGH(t, &ghWorld{state: tc.state, comments: `{"comments":[` + claimOf("hrn old (claude)", "fix-x", time.Hour) + `]}`})
			l := runLease("new")
			l.Branch = "fix-y"
			if _, err := TakeIssue(context.Background(), home, issue12, l, IssueOptions{GitHub: true, TakeOver: tc.takeOver}); err != nil {
				t.Fatalf("the replaced holder's claim must not refuse the take: %v", err)
			}
			// A dead holder is released before the claim, a live one only once the take goes ahead.
			claim, release := 0, 1
			if !tc.takeOver {
				claim, release = 1, 0
			}
			if len(w.bodies) != 2 || !strings.HasPrefix(w.bodies[claim], "Claim: hrn new (claude) on branch `fix-y`") ||
				w.bodies[release] != "Released: hrn old (claude)\n\n"+tc.reason+"\n" {
				t.Errorf("comments = %q", w.bodies)
			}
		})
	}
}

func TestOfTwoClaimsMadeAtOnceTheOlderWins(t *testing.T) {
	home := t.TempDir()
	w := useGH(t, &ghWorld{after: `{"comments":[` + claimOf("hrn elsewhere (codex)", "b", 2*time.Second) + `,` +
		claimOf("hrn r1 (claude)", "fix-x", time.Second) + `]}`})
	_, err := TakeIssue(context.Background(), home, issue12, runLease("r1"), IssueOptions{GitHub: true})
	var held *IssueHeldError
	if !errors.As(err, &held) || !strings.Contains(err.Error(), "hrn elsewhere (codex)") {
		t.Fatalf("the later claim must give way: %v", err)
	}
	if len(w.bodies) != 2 || w.bodies[1] != "Released: hrn r1 (claude)\n\nclaimed at the same time as hrn elsewhere (codex)\n" {
		t.Errorf("comments = %q", w.bodies)
	}
	if _, ok := readIssueLease(t, home, issue12); ok {
		t.Error("the losing take kept its local lease")
	}

	w = useGH(t, &ghWorld{after: `{"comments":[` + claimOf("hrn r1 (claude)", "fix-x", 2*time.Second) + `,` +
		claimOf("hrn elsewhere (codex)", "b", time.Second) + `]}`})
	if _, err := TakeIssue(context.Background(), home, issue12, runLease("r1"), IssueOptions{GitHub: true}); err != nil {
		t.Fatalf("the older claim must win: %v", err)
	}
	if len(w.bodies) != 1 {
		t.Errorf("the winner posts only its claim: %q", w.bodies)
	}
}

func TestOfTwoClaimsWithTheSameTimeTheEarlierCommentWins(t *testing.T) {
	at := ago(time.Minute)
	c := func(worker, branch string) string {
		return `{"body":"Claim: ` + worker + ` on branch ` + "`" + branch + "`" + `","createdAt":"` + at + `"}`
	}
	useGH(t, &ghWorld{after: `{"comments":[` + c("hrn elsewhere (codex)", "b") + `,` + c("hrn r1 (claude)", "fix-x") + `]}`})
	_, err := TakeIssue(context.Background(), t.TempDir(), issue12, runLease("r1"), IssueOptions{GitHub: true})
	var held *IssueHeldError
	if !errors.As(err, &held) || !strings.Contains(err.Error(), "hrn elsewhere (codex)") {
		t.Fatalf("the later comment must give way: %v", err)
	}
	useGH(t, &ghWorld{after: `{"comments":[` + c("hrn r1 (claude)", "fix-x") + `,` + c("hrn elsewhere (codex)", "b") + `]}`})
	if _, err := TakeIssue(context.Background(), t.TempDir(), issue12, runLease("r1"), IssueOptions{GitHub: true}); err != nil {
		t.Fatalf("the earlier comment must win: %v", err)
	}
}

// deadHolder leaves the lease of run old, whose process has ended, with an open GitHub claim.
func deadHolder(t *testing.T, home string) (pid int, start string, claims string) {
	t.Helper()
	pid, start = exitedPID(t)
	old := runLease("old")
	old.PID, old.Start, old.Taken = pid, start, time.Now()
	writeIssueLease(t, home, issue12, old)
	return pid, start, `{"comments":[` + claimOf("hrn old (claude)", "fix-x", time.Hour) + `]}`
}

func newLease() Lease {
	l := runLease("new")
	l.Branch = "fix-y"
	return l
}

const openPR12 = `[{"number":5,"url":"https://github.com/o/n/pull/5","headRefName":"other","body":"Fixes #12"}]`

func TestADeadHoldersClaimIsReleasedEvenWhenTheTakeIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, prs, after string }{
		{"open pull request", openPR12, ""},
		{"lost race", "", `{"comments":[` + claimOf("hrn elsewhere (codex)", "b", 2*time.Second) + `,` + claimOf("hrn new (claude)", "fix-y", time.Second) + `]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			_, _, claims := deadHolder(t, home)
			w := useGH(t, &ghWorld{prs: tc.prs, after: tc.after, comments: claims})
			_, err := TakeIssue(context.Background(), home, issue12, newLease(), IssueOptions{GitHub: true})
			var held *IssueHeldError
			if !errors.As(err, &held) {
				t.Fatalf("the take must be refused: %v", err)
			}
			if len(w.bodies) == 0 || w.bodies[0] != "Released: hrn old (claude)\n\nits process ended\n" {
				t.Errorf("the dead holder's claim must be released: %q", w.bodies)
			}
			if _, ok := readIssueLease(t, home, issue12); ok {
				t.Error("a refused take with nothing owed must leave no lease")
			}
		})
	}
}

func TestARefusedTakeWhoseReleaseFailedPutsTheDeadLeaseBack(t *testing.T) {
	home := t.TempDir()
	_, _, claims := deadHolder(t, home)
	before, err := os.ReadFile(issueLeasePath(home, issue12))
	if err != nil {
		t.Fatal(err)
	}
	useGH(t, &ghWorld{prs: openPR12, comments: claims, bodyFail: "Released: hrn old"})
	var held *IssueHeldError
	if _, err := TakeIssue(context.Background(), home, issue12, newLease(), IssueOptions{GitHub: true}); !errors.As(err, &held) {
		t.Fatalf("the take must be refused: %v", err)
	}
	if after, err := os.ReadFile(issueLeasePath(home, issue12)); err != nil || string(after) != string(before) {
		t.Errorf("the dead holder's lease must be back for the next take to release it: %q, %v", after, err)
	}
}

// takeOwing takes issue12 from a dead holder while posting its Released comment fails.
func takeOwing(t *testing.T, home string) (*ghWorld, *IssueLease, int, string) {
	t.Helper()
	pid, start, claims := deadHolder(t, home)
	w := useGH(t, &ghWorld{comments: claims, bodyFail: "Released: hrn old"})
	il, err := TakeIssue(context.Background(), home, issue12, newLease(), IssueOptions{GitHub: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(il.Notes, func(n string) bool { return strings.Contains(n, "GitHub release of hrn old (claude) failed") }) {
		t.Errorf("notes = %v", il.Notes)
	}
	if l, ok := readIssueLease(t, home, issue12); !ok || l.Run != "new" || !slices.Equal(l.Retired, []string{"hrn old (claude)"}) {
		t.Fatalf("our lease must owe the release: %+v, %t", l, ok)
	}
	return w, il, pid, start
}

func TestReleasePostsTheOwedReleaseFirst(t *testing.T) {
	home := t.TempDir()
	w, il, _, _ := takeOwing(t, home)
	w.bodyFail = ""
	n := len(w.bodies)
	if err := il.Release(context.Background(), "done"); err != nil {
		t.Fatal(err)
	}
	if len(w.bodies) <= n || w.bodies[n] != "Released: hrn old (claude)\n\nretired by hrn new (claude)\n" {
		t.Errorf("comments = %q", w.bodies[n:])
	}
	if _, ok := readIssueLease(t, home, issue12); ok {
		t.Error("release left the lease")
	}
}

func TestAnOwedReleaseThatFailsAgainIsPostedByTheNextTake(t *testing.T) {
	home := t.TempDir()
	w, il, pid, start := takeOwing(t, home)
	n := len(w.bodies)
	if err := il.Release(context.Background(), "done"); err == nil || !strings.Contains(err.Error(), "hrn old (claude)") {
		t.Errorf("a failed owed release must be reported: %v", err)
	}
	if len(w.bodies) != n+1 || w.bodies[n] != "Released: hrn old (claude)\n\nretired by hrn new (claude)\n" {
		t.Errorf("release must retry the owed comment first and stop there: %q", w.bodies[n:])
	}
	kept, ok := readIssueLease(t, home, issue12)
	if !ok || kept.PID != os.Getpid() || !slices.Equal(kept.Retired, []string{"hrn old (claude)"}) {
		t.Fatalf("the lease must stay as it was: %+v, %t", kept, ok)
	}

	// Our process ends with the release still owed.
	kept.PID, kept.Start = pid, start
	writeIssueLease(t, home, issue12, kept)
	w.bodyFail = ""
	w.comments = `{"comments":[` + claimOf("hrn old (claude)", "fix-x", 2*time.Hour) + `,` + claimOf("hrn new (claude)", "fix-y", time.Hour) + `]}`
	n = len(w.bodies)
	next := runLease("next")
	next.Branch = "fix-z"
	if _, err := TakeIssue(context.Background(), home, issue12, next, IssueOptions{GitHub: true}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Released: hrn new (claude)\n\nits process ended\n", "Released: hrn old (claude)\n\nits process ended\n"} {
		if !slices.Contains(w.bodies[n:], want) {
			t.Errorf("the next take must post %q: %q", want, w.bodies[n:])
		}
	}
}

func TestReleaseKeepsTheLeaseWhenThePullRequestLookupFails(t *testing.T) {
	home := t.TempDir()
	w := useGH(t, &ghWorld{})
	il, err := TakeIssue(context.Background(), home, issue12, runLease("r1"), IssueOptions{GitHub: true})
	if err != nil {
		t.Fatal(err)
	}
	w.headFail = errors.New("gh pr: HTTP 502")
	if err := il.Release(context.Background(), "run r1 ended"); err == nil || !strings.Contains(err.Error(), "502") {
		t.Errorf("a failed lookup must be reported: %v", err)
	}
	if l, ok := readIssueLease(t, home, issue12); !ok || l.PID != os.Getpid() || l.PR != "" {
		t.Errorf("the lease must stay as it was: %+v, %t", l, ok)
	}
	if len(w.bodies) != 1 {
		t.Errorf("a failed lookup posts nothing: %q", w.bodies)
	}
}

func TestATakeWithNoBranchKeepsTheLeaseAndSkipsGitHub(t *testing.T) {
	for _, branch := range []string{"", "HEAD"} {
		home := t.TempDir()
		w := useGH(t, &ghWorld{})
		l := runLease("r1")
		l.Branch = branch
		il, err := TakeIssue(context.Background(), home, issue12, l, IssueOptions{GitHub: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(il.Notes) != 1 || il.Notes[0] != "GitHub claim skipped: no branch (detached HEAD or not a git checkout)" {
			t.Errorf("branch %q: notes = %v", branch, il.Notes)
		}
		if len(w.calls) != 0 {
			t.Errorf("branch %q: gh was called: %q", branch, w.calls)
		}
		if _, ok := readIssueLease(t, home, issue12); !ok {
			t.Errorf("branch %q: the local lease must stay", branch)
		}
	}
	useGH(t, &ghWorld{})
	notGit := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(notGit))
	il, err := TakeRunIssue(context.Background(), t.TempDir(), "o/n#12", notGit, Lease{Holder: "hrn run", Run: "r2"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(il.Notes) != 1 || !strings.Contains(il.Notes[0], "no branch") {
		t.Errorf("a run outside a git checkout: notes = %v", il.Notes)
	}
}

func TestGitHubFailureKeepsTheLocalLeaseWithANote(t *testing.T) {
	home := t.TempDir()
	w := useGH(t, &ghWorld{fail: errors.New("gh: To get started with GitHub CLI, please run: gh auth login")})
	il, err := TakeIssue(context.Background(), home, issue12, runLease("r1"), IssueOptions{GitHub: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(il.Notes) != 1 || !strings.HasPrefix(il.Notes[0], "GitHub claim skipped: ") || !strings.Contains(il.Notes[0], "auth login") {
		t.Errorf("notes = %v", il.Notes)
	}
	if _, ok := readIssueLease(t, home, issue12); !ok {
		t.Fatal("the local lease must stay when GitHub cannot be asked")
	}
	n := len(w.calls)
	if err := il.Release(context.Background(), "done"); err != nil {
		t.Fatal(err)
	}
	if len(w.calls) != n {
		t.Errorf("an unclaimed lease must not call gh on release: %q", w.calls[n:])
	}
	if _, ok := readIssueLease(t, home, issue12); ok {
		t.Error("release left the lease")
	}
}

func TestFreeIssueRemovesOnlyALeaseNoProcessHolds(t *testing.T) {
	home := t.TempDir()
	if err := FreeIssue(home, issue12); err == nil || !strings.Contains(err.Error(), "free already") {
		t.Errorf("freeing a free issue: %v", err)
	}
	if _, err := TakeIssue(context.Background(), home, issue12, runLease("r1"), IssueOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := FreeIssue(home, issue12); err == nil || !strings.Contains(err.Error(), "running process") {
		t.Errorf("a live holder must be refused: %v", err)
	}
	l := runLease("r1")
	l.PR = "https://github.com/o/n/pull/7"
	writeIssueLease(t, home, issue12, l)
	if err := FreeIssue(home, issue12); err != nil {
		t.Fatal(err)
	}
	if _, ok := readIssueLease(t, home, issue12); ok {
		t.Error("free left the lease")
	}
}

func TestReadIssuesNeverAsksGitHub(t *testing.T) {
	home := t.TempDir()
	w := useGH(t, &ghWorld{state: "MERGED"})
	l := runLease("r1")
	l.PR = "https://github.com/o/n/pull/7"
	writeIssueLease(t, home, issue12, l)
	got, err := ReadIssues(home)
	if err != nil || len(got) != 1 || got[0].State != "held by pull request" || got[0].Issue != "o/n#12" || got[0].Run != "r1" {
		t.Errorf("ReadIssues = %+v, %v", got, err)
	}
	if len(w.calls) != 0 {
		t.Errorf("ReadIssues called gh: %q", w.calls)
	}
	if got := issueFromFile("o__my__repo__12"); got != "o/my__repo#12" {
		t.Errorf("issueFromFile = %q", got)
	}
}

func TestAFailedGitHubStepOwesTheDeadHoldersRelease(t *testing.T) {
	home := t.TempDir()
	deadHolder(t, home)
	useGH(t, &ghWorld{fail: errors.New("gh: not logged in")})
	if _, err := TakeIssue(context.Background(), home, issue12, newLease(), IssueOptions{GitHub: true}); err != nil {
		t.Fatal(err)
	}
	if l, ok := readIssueLease(t, home, issue12); !ok || !slices.Equal(l.Retired, []string{"hrn old (claude)"}) {
		t.Errorf("the dead holder's release must stay owed: %+v, %t", l, ok)
	}
}
