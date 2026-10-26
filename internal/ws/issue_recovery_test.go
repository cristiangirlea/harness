package ws

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestAFailedReleaseOfOurOwnClaimKeepsTheLeaseForTheNextTake(t *testing.T) {
	home := t.TempDir()
	w := useGH(t, &ghWorld{})
	il, err := TakeIssue(context.Background(), home, issue12, newLease(), IssueOptions{GitHub: true})
	if err != nil {
		t.Fatal(err)
	}
	w.bodyFail = "Released: hrn new"
	if err := il.Release(context.Background(), "done"); err == nil {
		t.Fatal("a failed release comment must be an error")
	}
	if _, ok := readIssueLease(t, home, issue12); !ok {
		t.Fatal("the lease must stay so the next take releases our claim")
	}
}

func TestALostRaceWhoseReleaseFailsKeepsOurLease(t *testing.T) {
	home := t.TempDir()
	after := `{"comments":[` + claimOf("hrn elsewhere (codex)", "b", 2*time.Second) + `,` + claimOf("hrn new (claude)", "fix-y", time.Second) + `]}`
	useGH(t, &ghWorld{after: after, bodyFail: "Released: hrn new"})
	var held *IssueHeldError
	if _, err := TakeIssue(context.Background(), home, issue12, newLease(), IssueOptions{GitHub: true}); !errors.As(err, &held) {
		t.Fatalf("the lost race must be refused: %v", err)
	}
	if l, ok := readIssueLease(t, home, issue12); !ok || l.Run != "new" {
		t.Errorf("our lease must stay until our process ends, so the next take releases our claim: %+v, %t", l, ok)
	}
}

func TestATakeWithNoBranchOwesTheDeadHoldersRelease(t *testing.T) {
	home := t.TempDir()
	deadHolder(t, home)
	w := useGH(t, &ghWorld{})
	l := newLease()
	l.Branch = "HEAD"
	if _, err := TakeIssue(context.Background(), home, issue12, l, IssueOptions{GitHub: true}); err != nil {
		t.Fatal(err)
	}
	if got, ok := readIssueLease(t, home, issue12); !ok || !slices.Equal(got.Retired, []string{"hrn old (claude)"}) {
		t.Errorf("the dead holder's release must stay owed: %+v, %t", got, ok)
	}
	if len(w.called("issue", "comment")) != 0 {
		t.Error("a take with no branch must not post on GitHub")
	}
}

func TestAnOwedReleaseIsKeptInMemoryWhenTheLeaseCannotBeRewritten(t *testing.T) {
	home := t.TempDir()
	useGH(t, &ghWorld{})
	il, err := TakeIssue(context.Background(), home, issue12, newLease(), IssueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(il.path, []byte(`{"holder":"someone else"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	il.owe("hrn old (claude)")
	if !slices.Equal(il.Lease.Retired, []string{"hrn old (claude)"}) {
		t.Errorf("Retired = %v", il.Lease.Retired)
	}
}

func TestABranchWithABacktickIsNotClaimedOnGitHub(t *testing.T) {
	home := t.TempDir()
	w := useGH(t, &ghWorld{})
	l := newLease()
	l.Branch = "fix`tick"
	il, err := TakeIssue(context.Background(), home, issue12, l, IssueOptions{GitHub: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(w.called("issue", "comment")) != 0 || !slices.ContainsFunc(il.Notes, func(n string) bool { return strings.Contains(n, "cannot carry") }) {
		t.Errorf("notes = %v, comments = %v", il.Notes, w.called("issue", "comment"))
	}
}

func TestARefusalCarriesNoTerminalControls(t *testing.T) {
	home := t.TempDir()
	useGH(t, &ghWorld{})
	holder := runLease("other\x1b]0;x\x07")
	holder.PID = os.Getpid()
	holder.Start, _ = startID(holder.PID)
	holder.Holder = "h\x9b2J"
	writeIssueLease(t, home, issue12, holder)
	_, err := TakeIssue(context.Background(), home, issue12, newLease(), IssueOptions{})
	if err == nil || strings.ContainsAny(err.Error(), "\x1b\x07\u009b") {
		t.Fatalf("refusal = %q", err)
	}
}

func TestAnOriginWithCredentialsIsNotEchoed(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "https://user:s3cret@example.com/o/n.git"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	_, err := ParseIssue("#5", dir)
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("ParseIssue = %v", err)
	}
}

func TestAClaimByAnotherAccountIsIgnored(t *testing.T) {
	home := t.TempDir()
	stranger := `{"comments":[{"author":{"login":"stranger"},"body":"Claim: hrn x (codex) on branch ` + "`b`" + `","createdAt":"` + ago(time.Minute) + `"}]}`
	useGH(t, &ghWorld{comments: stranger})
	if _, err := TakeIssue(context.Background(), home, issue12, newLease(), IssueOptions{GitHub: true}); err != nil {
		t.Fatalf("a claim posted by another account must not hold the issue: %v", err)
	}
}

func TestTheLoginIsReadAgainForEachTake(t *testing.T) {
	home := t.TempDir()
	byB := `{"comments":[{"author":{"login":"b"},"body":"Claim: hrn x (codex) on branch ` + "`x`" + `","createdAt":"` + ago(time.Minute) + `"}]}`
	w := useGH(t, &ghWorld{comments: byB, login: "a"})
	if _, err := TakeIssue(context.Background(), home, IssueRef{Repo: "o/n", Number: 13}, newLease(), IssueOptions{GitHub: true}); err != nil {
		t.Fatalf("as a, b's claim does not count: %v", err)
	}
	w.login = "b"
	var held *IssueHeldError
	if _, err := TakeIssue(context.Background(), home, issue12, newLease(), IssueOptions{GitHub: true}); !errors.As(err, &held) {
		t.Fatalf("once gh acts as b, b's claim holds the issue: %v", err)
	}
}

func TestAClaimKeepsItsExactBranchForTheStaleCheck(t *testing.T) {
	home := t.TempDir()
	branch := "fix\u0085x"
	old := `{"comments":[{"body":"Claim: hrn x (codex) on branch ` + "`" + branch + "`" + `","createdAt":"` + ago(48*time.Hour) + `"}]}`
	useGH(t, &ghWorld{comments: old, branches: map[string]string{"fix\u0085x": ago(time.Hour)}})
	var held *IssueHeldError
	if _, err := TakeIssue(context.Background(), home, issue12, newLease(), IssueOptions{GitHub: true}); !errors.As(err, &held) {
		t.Fatalf("a claim whose branch had a recent commit holds the issue: %v", err)
	}
	if strings.ContainsRune(held.Error(), '\u0085') {
		t.Errorf("refusal = %q", held.Error())
	}
}

func TestAStrangersReleaseDoesNotCloseOurClaim(t *testing.T) {
	home := t.TempDir()
	doc := `{"comments":[` + claimOf("hrn x (codex)", "x", time.Minute) + `,{"author":{"login":"stranger"},"body":"Released: hrn x (codex)","createdAt":"` + ago(time.Second) + `"}]}`
	useGH(t, &ghWorld{comments: doc})
	var held *IssueHeldError
	if _, err := TakeIssue(context.Background(), home, issue12, newLease(), IssueOptions{GitHub: true}); !errors.As(err, &held) {
		t.Fatalf("a stranger cannot release a claim: %v", err)
	}
}
