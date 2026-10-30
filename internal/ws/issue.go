package ws

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// IssueRef names a GitHub issue; Repo is owner/name.
type IssueRef struct {
	Repo   string
	Number int
}

func (r IssueRef) String() string { return r.Repo + "#" + strconv.Itoa(r.Number) }

// URL is the issue's page on GitHub.
func (r IssueRef) URL() string {
	return "https://github.com/" + r.Repo + "/issues/" + strconv.Itoa(r.Number)
}

var (
	issueRefRE = regexp.MustCompile(`^([A-Za-z0-9-]+)/([A-Za-z0-9._-]+)#([0-9]+)$`)
	issueURLRE = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9-]+)/([A-Za-z0-9._-]+)/issues/([0-9]+)/?$`)
	issueNumRE = regexp.MustCompile(`^#?([0-9]+)$`)
	originRE   = regexp.MustCompile(`^(?:https://(?:[^@/]+@)?github\.com/|git@github\.com:|ssh://git@github\.com/)([A-Za-z0-9-]+)/([A-Za-z0-9._-]+?)(?:\.git)?/?$`)
)

// ParseIssue reads owner/name#N, the issue's URL, or #N and N against dir's origin remote.
func ParseIssue(s, dir string) (IssueRef, error) {
	s = strings.TrimSpace(s)
	forms := "use owner/name#N, https://github.com/owner/name/issues/N, or #N for this repository's origin"
	var owner, name, num string
	if m := issueRefRE.FindStringSubmatch(s); m != nil {
		owner, name, num = m[1], m[2], m[3]
	} else if m := issueURLRE.FindStringSubmatch(s); m != nil {
		owner, name, num = m[1], m[2], m[3]
	} else if m := issueNumRE.FindStringSubmatch(s); m != nil {
		url, err := gitOut(context.Background(), dir, "remote", "get-url", "origin")
		if err != nil {
			return IssueRef{}, fmt.Errorf("issue %q: %s has no origin remote to take the repository from; %s", s, dir, forms)
		}
		o := originRE.FindStringSubmatch(url)
		if o == nil {
			return IssueRef{}, fmt.Errorf("issue %q: origin is not a GitHub repository; %s", s, forms)
		}
		owner, name, num = o[1], o[2], m[1]
	} else {
		return IssueRef{}, fmt.Errorf("issue %q: %s", s, forms)
	}
	n, err := strconv.Atoi(num)
	if err != nil || n < 1 || name == "." || name == ".." {
		return IssueRef{}, fmt.Errorf("issue %q: %s", s, forms)
	}
	// GitHub names are case-insensitive, so one issue must give one lease and one duplicate key.
	return IssueRef{Repo: strings.ToLower(owner + "/" + name), Number: n}, nil
}

// IssuesDir holds issue leases under the hrn home, so every checkout on this machine shares them.
func IssuesDir(home string) string { return filepath.Join(home, "leases", "issues") }

func issueLeasePath(home string, r IssueRef) string {
	owner, name, _ := strings.Cut(r.Repo, "/")
	return filepath.Join(IssuesDir(home), owner+"__"+name+"__"+strconv.Itoa(r.Number)+".lease")
}

// GH runs the gh CLI; tests replace it so that none reaches GitHub.
var GH = func(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "NO_COLOR=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		msg, _, _ = strings.Cut(msg, "\n")
		return nil, fmt.Errorf("gh %s: %s", strings.Join(args[:min(2, len(args))], " "), printable(msg))
	}
	return out, nil
}

// IssueOptions says whether to replace a holder and whether to claim on GitHub too.
type IssueOptions struct {
	TakeOver bool
	GitHub   bool
}

// IssueLease is a taken issue; Release gives it back.
type IssueLease struct {
	Ref   IssueRef
	Lease Lease
	Notes []string // takeovers and GitHub steps skipped, for the caller to print

	path    string
	raw     []byte
	worker  string
	claimed bool
	done    bool
}

// IssueHeldError refuses a take because someone else works on the issue.
type IssueHeldError struct {
	Issue IssueRef
	By    string
}

func (e *IssueHeldError) Error() string {
	return fmt.Sprintf("issue %s is held by %s; --take-over (hrn run) or take_over (batch spec or task, MCP run) takes it", e.Issue, printable(e.By))
}

// printable keeps lease, branch and GitHub text from steering a terminal when it is printed.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return '?'
		}
		return r
	}, s)
}

func holderText(l Lease) string {
	return printable(holderLine(l))
}

func holderLine(l Lease) string {
	s := fmt.Sprintf("%q", l.Holder)
	if l.Run != "" {
		s += ", run " + l.Run
	}
	if l.Backend != "" {
		s += ", backend " + l.Backend
	}
	if l.Branch != "" {
		s += ", branch " + l.Branch
	}
	if l.PR != "" {
		s += ", pull request " + l.PR
	}
	if !l.Taken.IsZero() {
		s += fmt.Sprintf(", since %s (%s ago)", l.Taken.Local().Format("2006-01-02 15:04"), time.Since(l.Taken).Round(time.Minute))
	}
	return s
}

// retiredWorker is a previous holder this take replaces; its GitHub claim no longer holds the issue.
// A takeover retiree is released only once the take goes ahead; the others are dead whatever happens.
type retiredWorker struct {
	worker, reason string
	takeover       bool
}

// retire retires l's worker and the workers whose release l still owed.
func (il *IssueLease) retire(rs []retiredWorker, l Lease, reason string, takeover bool) []retiredWorker {
	add := func(w string, takeover bool) {
		if w != il.worker && !isRetired(rs, w) {
			rs = append(rs, retiredWorker{w, reason, takeover})
		}
	}
	if l.Run != "" {
		add(workerOf(l), takeover)
	}
	for _, w := range l.Retired {
		add(w, false)
	}
	return rs
}

func isRetired(rs []retiredWorker, worker string) bool {
	for _, r := range rs {
		if r.worker == worker {
			return true
		}
	}
	return false
}

func workerOf(l Lease) string {
	if l.Backend == "" {
		return "hrn " + l.Run
	}
	return "hrn " + l.Run + " (" + l.Backend + ")"
}

// TakeIssue leases ref to l's run for this process, and claims it on GitHub when opt.GitHub is set.
// A failing gh only adds a note: the local lease still guards this machine.
func TakeIssue(ctx context.Context, home string, ref IssueRef, l Lease, opt IssueOptions) (*IssueLease, error) {
	if err := os.MkdirAll(IssuesDir(home), 0o700); err != nil {
		return nil, err
	}
	p := issueLeasePath(home, ref)
	l.Issue, l.PR = ref.String(), ""
	l.PID = os.Getpid()
	l.Start, _ = startID(l.PID)
	l.Taken = time.Now().UTC().Truncate(time.Second)
	data, err := json.Marshal(l)
	if err != nil {
		return nil, err
	}
	il := &IssueLease{Ref: ref, Lease: l, path: p, raw: append(data, '\n'), worker: workerOf(l)}
	// Read before acquire: acquire removes a stale lease, and its holder's GitHub claim must be released with it.
	var retired []retiredWorker
	old, raw, oldErr := readLease(p)
	if oldErr == nil && old.PID == 0 && old.PR != "" && prDone(ctx, old.PR) {
		if why, _ := removeLease(p, raw); why == "" {
			il.Notes = append(il.Notes, "pull request "+old.PR+" that held "+ref.String()+" is closed or merged, so its lease is gone")
			retired = il.retire(retired, old, "pull request "+old.PR+" is merged or closed", false)
		}
	}
	busy, holder, _, err := acquire(p, l)
	if err != nil {
		return nil, fmt.Errorf("issue %s: %w", ref, err)
	}
	var previous []string
	if busy == "" && oldErr == nil && old.stale() {
		retired = il.retire(retired, old, "its process ended", false)
	}
	if busy != "" {
		by := busy
		if holder != nil {
			by = holderText(*holder)
		}
		if !opt.TakeOver {
			return nil, &IssueHeldError{Issue: ref, By: by}
		}
		if err := replaceLease(p, il.raw); err != nil {
			return nil, fmt.Errorf("issue %s: %w", ref, err)
		}
		if holder != nil {
			retired = il.retire(retired, *holder, "taken over by "+il.worker, true)
		}
		previous = append(previous, by)
		il.Notes = append(il.Notes, "took issue "+ref.String()+" over from "+by)
	}
	switch {
	case !opt.GitHub:
	case l.Branch == "" || l.Branch == "HEAD":
		il.Notes = append(il.Notes, "GitHub claim skipped: no branch (detached HEAD or not a git checkout)")
		il.oweRetired(retired)
	case strings.Contains(l.Branch, "`") || printable(l.Branch) != l.Branch:
		il.Notes = append(il.Notes, "GitHub claim skipped: branch "+printable(l.Branch)+" has a character the Claim comment cannot carry")
		il.oweRetired(retired)
	default:
		by, keep, err := il.claimGitHub(ctx, opt.TakeOver, previous, retired)
		switch {
		case err != nil:
			il.Notes = append(il.Notes, "GitHub claim skipped: "+err.Error())
			il.oweRetired(retired)
		case by != "" && keep:
			// Our claim could not be released: the lease goes stale with this process and the next take releases it.
			return nil, &IssueHeldError{Issue: ref, By: by}
		case by != "":
			if len(il.Lease.Retired) > 0 && oldErr == nil {
				// The replaced lease names the releases still owed, so putting it back lets the next take retry them.
				il.swap(raw)
			} else {
				removeLease(p, il.raw)
			}
			return nil, &IssueHeldError{Issue: ref, By: by}
		}
	}
	return il, nil
}

// Takeover overwrites in place under the lock, so no third taker slips in between a remove and a create.
func replaceLease(p string, data []byte) error {
	unlock, why := lockLease(p)
	if why != "" {
		return errors.New(why)
	}
	defer unlock()
	return os.WriteFile(p, data, 0o600)
}

type ghPR struct {
	Number      int    `json:"number"`
	URL         string `json:"url"`
	HeadRefName string `json:"headRefName"`
	Body        string `json:"body"`
	Closing     []struct {
		Number     int    `json:"number"`
		URL        string `json:"url"`
		Repository struct {
			Name  string `json:"name"`
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"repository"`
	} `json:"closingIssuesReferences"`
}

func (p ghPR) closes(ref IssueRef) bool {
	for _, c := range p.Closing {
		if c.Number != ref.Number {
			continue
		}
		switch {
		case c.URL != "":
			if strings.EqualFold(strings.TrimSuffix(c.URL, "/"), ref.URL()) {
				return true
			}
		case c.Repository.Name != "":
			if strings.EqualFold(c.Repository.Owner.Login+"/"+c.Repository.Name, ref.Repo) {
				return true
			}
		default:
			return true
		}
	}
	return closesInBody(p.Body, ref)
}

// A bare mention does not hold an issue; only GitHub's closing keywords do.
func closesInBody(body string, ref IssueRef) bool {
	n := strconv.Itoa(ref.Number)
	repo := regexp.QuoteMeta(ref.Repo)
	target := `(?:` + repo + `)?#` + n + `|https://github\.com/` + repo + `/issues/` + n
	re := regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?):?\s+(?:` + target + `)\b`)
	return re.MatchString(body)
}

func (il *IssueLease) openPRs(ctx context.Context) ([]ghPR, error) {
	out, err := GH(ctx, "pr", "list", "-R", il.Ref.Repo, "--state", "open", "--search", strconv.Itoa(il.Ref.Number)+" in:body",
		"--json", "number,url,headRefName,body,closingIssuesReferences")
	if err != nil {
		return nil, err
	}
	var prs []ghPR
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("gh pr list: %v", err)
	}
	var held []ghPR
	for _, p := range prs {
		if p.HeadRefName != il.Lease.Branch && p.closes(il.Ref) {
			held = append(held, p)
		}
	}
	return held, nil
}

type issueClaim struct {
	worker, branch string
	at             time.Time
}

var (
	claimRE    = regexp.MustCompile("^Claim: (.+?) on branch `([^`]+)`")
	releasedRE = regexp.MustCompile(`^Released: (.+)$`)
)

// Claims read in comment order: a Released line from the same worker closes its claim.
func (il *IssueLease) claims(ctx context.Context) ([]issueClaim, error) {
	me, err := ghLogin(ctx)
	if err != nil {
		return nil, err
	}
	out, err := GH(ctx, "issue", "view", strconv.Itoa(il.Ref.Number), "-R", il.Ref.Repo, "--json", "comments")
	if err != nil {
		return nil, err
	}
	var doc struct {
		Comments []struct {
			Author struct {
				Login string `json:"login"`
			} `json:"author"`
			Body      string    `json:"body"`
			CreatedAt time.Time `json:"createdAt"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("gh issue view: %v", err)
	}
	var open []issueClaim
	for _, c := range doc.Comments {
		if !strings.EqualFold(c.Author.Login, me) {
			continue
		}
		first, _, _ := strings.Cut(c.Body, "\n")
		first = strings.TrimSpace(first)
		if m := claimRE.FindStringSubmatch(first); m != nil {
			open = dropClaim(open, m[1])
			open = append(open, issueClaim{worker: m[1], branch: m[2], at: c.CreatedAt})
		} else if m := releasedRE.FindStringSubmatch(first); m != nil {
			open = dropClaim(open, strings.TrimSpace(m[1]))
		}
	}
	return open, nil
}

// ghLogin names the account gh acts as now; every worker posts as it, and anyone else can comment on a public issue.
func ghLogin(ctx context.Context) (string, error) {
	out, err := GH(ctx, "api", "user", "--jq", ".login")
	if err != nil {
		return "", err
	}
	l := strings.TrimSpace(string(out))
	if l == "" {
		return "", errors.New("gh api user: no login")
	}
	return l, nil
}

func dropClaim(open []issueClaim, worker string) []issueClaim {
	kept := open[:0]
	for _, c := range open {
		if c.worker != worker {
			kept = append(kept, c)
		}
	}
	return kept
}

const claimStaleAfter = 24 * time.Hour

// A fresh claim with no branch yet is normal: branches are pushed only once tested.
func (il *IssueLease) claimStale(ctx context.Context, c issueClaim) (bool, error) {
	if time.Since(c.at) <= claimStaleAfter {
		return false, nil
	}
	out, err := GH(ctx, "api", "repos/"+il.Ref.Repo+"/branches/"+c.branch, "--jq", ".commit.commit.committer.date")
	if err != nil {
		if s := err.Error(); strings.Contains(s, "404") || strings.Contains(s, "Not Found") {
			return true, nil
		}
		return false, err
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(string(out)))
	if err != nil {
		return false, fmt.Errorf("branch %s: commit date %q: %v", c.branch, strings.TrimSpace(string(out)), err)
	}
	return time.Since(at) > claimStaleAfter, nil
}

// Comment, label and Released formats are shared with a claim tool outside hrn and must stay exact.
func (il *IssueLease) claimGitHub(ctx context.Context, takeOver bool, previous []string, retired []retiredWorker) (heldBy string, keep bool, err error) {
	prs, err := il.openPRs(ctx)
	if err != nil {
		return "", false, err
	}
	claims, err := il.claims(ctx)
	if err != nil {
		return "", false, err
	}
	// Dead holders' claims are void whatever this take decides, so a refusal or a lost race must not drop their release.
	il.releaseRetired(ctx, claims, retired, false)
	var held []string
	for _, p := range prs {
		held = append(held, "open pull request "+printable(p.URL)+" on branch "+printable(p.HeadRefName))
	}
	stale := map[string]bool{}
	var others []issueClaim
	for _, c := range claims {
		if c.worker == il.worker || isRetired(retired, c.worker) {
			continue
		}
		s, err := il.claimStale(ctx, c)
		if err != nil {
			return "", false, err
		}
		if s {
			stale[claimKey(c)] = true
			continue
		}
		held = append(held, claimText(c))
		others = append(others, c)
	}
	if len(held) > 0 && !takeOver {
		return strings.Join(held, "; "), false, nil
	}
	for _, c := range others {
		retired = append(retired, retiredWorker{c.worker, "taken over by " + il.worker, true})
	}
	previous = append(previous, held...)
	n, repo := strconv.Itoa(il.Ref.Number), il.Ref.Repo
	if _, err := GH(ctx, "label", "create", "claimed", "-R", repo, "--color", "FBCA04", "--force",
		"--description", "A worker is on this issue; see the Claim comment"); err != nil {
		return "", false, err
	}
	if _, err := GH(ctx, "issue", "edit", n, "-R", repo, "--add-label", "claimed"); err != nil {
		return "", false, err
	}
	body := "Claim: " + il.worker + " on branch `" + il.Lease.Branch + "`"
	if takeOver && len(previous) > 0 {
		body += "\nTakes over from " + strings.Join(previous, "; ") + "."
	}
	if err := il.comment(ctx, body); err != nil {
		return "", false, err
	}
	il.claimed = true
	// Another machine may have claimed between our read and our comment: the oldest live claim wins.
	if winner, err := il.raceWinner(ctx, retired, stale); err != nil {
		il.Notes = append(il.Notes, "GitHub claim not rechecked: "+err.Error())
	} else if winner != nil {
		il.claimed = false
		by := claimText(*winner)
		if err := il.comment(ctx, "Released: "+il.worker+"\n\nclaimed at the same time as "+winner.worker); err != nil {
			return by + " (releasing our own claim failed: " + err.Error() + ")", true, nil
		}
		return by, false, nil
	}
	il.releaseRetired(ctx, claims, retired, true)
	return "", false, nil
}

// releaseRetired posts Released for the retirees of one kind that hold an open claim; a failed post is owed in our lease.
func (il *IssueLease) releaseRetired(ctx context.Context, claims []issueClaim, rs []retiredWorker, takeover bool) {
	for _, r := range rs {
		if r.takeover != takeover || !slices.ContainsFunc(claims, func(c issueClaim) bool { return c.worker == r.worker }) {
			continue
		}
		if err := il.comment(ctx, "Released: "+r.worker+"\n\n"+r.reason); err != nil {
			il.Notes = append(il.Notes, "GitHub release of "+printable(r.worker)+" failed: "+err.Error())
			il.owe(r.worker)
		}
	}
}

// oweRetired records dead holders' releases when the claims could not be read: one may be open.
func (il *IssueLease) oweRetired(rs []retiredWorker) {
	for _, r := range rs {
		if !r.takeover && !slices.Contains(il.Lease.Retired, r.worker) {
			il.owe(r.worker)
		}
	}
}

func (il *IssueLease) owe(worker string) {
	l := il.Lease
	l.Retired = append(slices.Clone(l.Retired), worker)
	il.Lease.Retired = l.Retired
	if err := il.rewrite(l); err != nil {
		il.Notes = append(il.Notes, "recording that the release of "+worker+" is owed failed: "+err.Error())
	}
}

var errLeaseChanged = errors.New(leaseChanged)

// rewrite replaces our lease file with l, only while its bytes are still ours.
func (il *IssueLease) rewrite(l Lease) error {
	data, err := json.Marshal(l)
	if err != nil {
		return err
	}
	if err := il.swap(append(data, '\n')); err != nil {
		return err
	}
	il.Lease = l
	return nil
}

func (il *IssueLease) swap(data []byte) error {
	unlock, why := lockLease(il.path)
	if why != "" {
		return errors.New(why)
	}
	defer unlock()
	if now, err := os.ReadFile(il.path); err != nil || !bytes.Equal(now, il.raw) {
		return errLeaseChanged
	}
	if err := os.WriteFile(il.path, data, 0o600); err != nil {
		return err
	}
	il.raw = data
	return nil
}

func claimKey(c issueClaim) string { return c.worker + "\x00" + c.at.Format(time.RFC3339Nano) }

func claimText(c issueClaim) string {
	return printable(fmt.Sprintf("the GitHub claim of %s on branch %s since %s", c.worker, c.branch, c.at.Local().Format("2006-01-02 15:04")))
}

// raceWinner returns the oldest open claim that is neither stale nor retired when it is not ours; ties go to the earlier comment.
func (il *IssueLease) raceWinner(ctx context.Context, retired []retiredWorker, stale map[string]bool) (*issueClaim, error) {
	claims, err := il.claims(ctx)
	if err != nil {
		return nil, err
	}
	var first *issueClaim
	for i, c := range claims {
		if isRetired(retired, c.worker) || stale[claimKey(c)] {
			continue
		}
		if first == nil || c.at.Before(first.at) {
			first = &claims[i]
		}
	}
	if first == nil || first.worker == il.worker {
		return nil, nil
	}
	return first, nil
}

func (il *IssueLease) comment(ctx context.Context, body string) error {
	f, err := os.CreateTemp("", "hrn-issue-*.md")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, werr := f.WriteString(body + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	_, err = GH(ctx, "issue", "comment", strconv.Itoa(il.Ref.Number), "-R", il.Ref.Repo, "--body-file", f.Name())
	return err
}

func prDone(ctx context.Context, url string) bool {
	out, err := GH(ctx, "pr", "view", url, "--json", "state", "--jq", ".state")
	if err != nil {
		return false
	}
	state := strings.TrimSpace(string(out))
	return state == "MERGED" || state == "CLOSED"
}

// Release gives the issue back, or leaves it to the branch's open pull request so the lease outlives the run.
// It runs at run end, so it ignores the caller's cancellation; a second call does nothing.
func (il *IssueLease) Release(ctx context.Context, reason string) error {
	if il == nil || il.done {
		return nil
	}
	il.done = true
	ctx = context.WithoutCancel(ctx)
	for _, w := range il.Lease.Retired {
		if err := il.comment(ctx, "Released: "+w+"\n\nretired by "+il.worker); err != nil {
			// The lease names the owed releases: it goes stale with this process and the next take retries them.
			return fmt.Errorf("issue %s: posting the release of %s failed, so the lease stays for the next take to retry: %w", il.Ref, w, err)
		}
	}
	il.Lease.Retired = nil
	if il.claimed && il.Lease.Branch != "" {
		url, err := il.branchPR(ctx)
		if err != nil {
			// Unknown is not "no pull request": the lease goes stale with this process and the next take releases the claim.
			return fmt.Errorf("issue %s: looking for the pull request of branch %s failed, so the lease and the GitHub claim stay: %w", il.Ref, il.Lease.Branch, err)
		}
		if url != "" {
			return il.park(url)
		}
	}
	if il.claimed {
		body := "Released: " + il.worker
		if reason != "" {
			body += "\n\n" + reason
		}
		if err := il.comment(ctx, body); err != nil {
			// The lease names our worker: it goes stale with this process and the next take releases our claim.
			return fmt.Errorf("issue %s: posting our release failed, so the lease stays for the next take to release: %w", il.Ref, err)
		}
	}
	var errs []error
	// Changed hands means a takeover: the file is the new holder's now.
	if why, changed := removeLease(il.path, il.raw); why != "" && !changed {
		errs = append(errs, fmt.Errorf("issue %s: %s", il.Ref, why))
	}
	if il.claimed {
		if err := il.unlabel(ctx); err != nil {
			errs = append(errs, fmt.Errorf("issue %s: GitHub release: %w", il.Ref, err))
		}
	}
	return errors.Join(errs...)
}

func (il *IssueLease) branchPR(ctx context.Context) (string, error) {
	out, err := GH(ctx, "pr", "list", "-R", il.Ref.Repo, "--head", il.Lease.Branch, "--state", "open", "--json", "url")
	if err != nil {
		return "", err
	}
	var prs []struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(out, &prs); err != nil || len(prs) == 0 {
		return "", err
	}
	return prs[0].URL, nil
}

func (il *IssueLease) park(url string) error {
	l := il.Lease
	l.PID, l.Start, l.PR = 0, "", url
	if err := il.rewrite(l); errors.Is(err, errLeaseChanged) {
		return fmt.Errorf("issue %s: its lease changed hands, so pull request %s does not hold it", il.Ref, url)
	} else if err != nil {
		return fmt.Errorf("issue %s: %w", il.Ref, err)
	}
	il.Notes = append(il.Notes, "pull request "+url+" holds issue "+il.Ref.String()+" until it is merged or closed")
	return nil
}

func (il *IssueLease) unlabel(ctx context.Context) error {
	open, err := il.claims(ctx)
	if err != nil {
		return err
	}
	if len(open) == 0 {
		_, err = GH(ctx, "issue", "edit", strconv.Itoa(il.Ref.Number), "-R", il.Ref.Repo, "--remove-label", "claimed")
	}
	return err
}

// IssueStatus describes one issue lease.
type IssueStatus struct {
	Issue   string    `json:"issue"`
	State   string    `json:"state"` // held, held by pull request, stale, or released (its pull request closed)
	Why     string    `json:"why,omitempty"`
	Holder  string    `json:"holder,omitempty"`
	PID     int       `json:"pid,omitempty"`
	Run     string    `json:"run,omitempty"`
	Backend string    `json:"backend,omitempty"`
	Branch  string    `json:"branch,omitempty"`
	PR      string    `json:"pr,omitempty"`
	Since   time.Time `json:"since,omitzero"`
}

// ListIssues reports every issue lease, asking GitHub whether pull-request-held ones are still open.
// It removes nothing: the next take replaces stale and released leases and releases their GitHub claims.
func ListIssues(ctx context.Context, home string) ([]IssueStatus, error) {
	return issueStatuses(ctx, home, true)
}

// ReadIssues reports issue leases from disk alone, for views that refresh often.
func ReadIssues(home string) ([]IssueStatus, error) {
	return issueStatuses(context.Background(), home, false)
}

func issueStatuses(ctx context.Context, home string, checkPR bool) ([]IssueStatus, error) {
	dir := IssuesDir(home)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []IssueStatus
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".lease")
		if !ok || e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		l, _, err := readLease(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		s := IssueStatus{Issue: printable(l.Issue), Holder: printable(l.Holder), PID: l.PID, Run: printable(l.Run), Backend: printable(l.Backend), Branch: printable(l.Branch), PR: printable(l.PR), Since: l.Taken}
		if s.Issue == "" {
			s.Issue = printable(issueFromFile(name))
		}
		switch {
		case err != nil:
			s.State, s.Why = "held", err.Error()
		case l.PID == 0 && l.PR != "":
			s.State = "held by pull request"
			if checkPR && prDone(ctx, l.PR) {
				s.State, s.Why = "released", "its pull request is merged or closed"
			}
		case l.stale():
			s.State, s.Why = "stale", "its process ended"
			if alive(l.PID) {
				s.Why = "its process id now names another process"
			}
		default:
			s.State = "held"
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Issue < out[j].Issue })
	return out, nil
}

func issueFromFile(name string) string {
	owner, rest, ok := strings.Cut(name, "__")
	i := strings.LastIndex(rest, "__")
	if !ok || i < 0 {
		return name
	}
	return owner + "/" + rest[:i] + "#" + rest[i+2:]
}

// FreeIssue removes ref's lease by hand; a running holder gives it back itself.
func FreeIssue(home string, ref IssueRef) error {
	p := issueLeasePath(home, ref)
	l, raw, err := readLease(p)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("issue %s has no lease: it is free already", ref)
	}
	if err != nil {
		return fmt.Errorf("issue %s: %v", ref, err)
	}
	if l.processHolds() {
		return fmt.Errorf("issue %s is held by running process %d (%s); it gives the issue back itself, or a run with --take-over takes it", ref, l.PID, holderText(l))
	}
	unlock, why := lockLease(p)
	if why != "" {
		return fmt.Errorf("issue %s: %s", ref, why)
	}
	defer unlock()
	if now, err := os.ReadFile(p); err != nil || !bytes.Equal(now, raw) {
		return fmt.Errorf("issue %s: its lease changed while it was being freed", ref)
	}
	return removeOwn(p)
}

// CurrentBranch names dir's checked-out branch, or HEAD when detached.
func CurrentBranch(ctx context.Context, dir string) (string, error) {
	return gitOut(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
}

// TakeRunIssue leases issue, read against dir, to a single run on dir's current branch, claiming it on GitHub too.
func TakeRunIssue(ctx context.Context, home, issue, dir string, l Lease, takeOver bool) (*IssueLease, error) {
	ref, err := ParseIssue(issue, dir)
	if err != nil {
		return nil, err
	}
	if l.Branch == "" {
		l.Branch, _ = CurrentBranch(ctx, dir)
	}
	return TakeIssue(ctx, home, ref, l, IssueOptions{TakeOver: takeOver, GitHub: true})
}
