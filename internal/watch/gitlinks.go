package watch

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/cristiangirlea/harness/internal/runlog"
	"github.com/cristiangirlea/harness/internal/tools"
)

// commitLinks is how many of a run's commits its page lists.
const commitLinks = 10

// gitFacts says where a run's work went; on github.com each part links there, built from the record without calling GitHub.
// Past the first commits, one link compares the run's start and end, which lists every commit.
func gitFacts(rec runlog.Record) []fact {
	// Only what the run recorded: a folder's checkout today may hold another repository than the run's commits.
	key, repo := recordedRepo(rec)
	base := ""
	if strings.HasPrefix(key, "r:") && strings.HasPrefix(repo, "github.com/") {
		base = "https://" + repo
	}
	link := func(text, path string) factLink {
		if base == "" {
			return factLink{Text: text}
		}
		return factLink{Text: text, Href: base + path}
	}
	var facts []fact
	if repo != "" {
		name := repoLabel(repo)
		facts = append(facts, fact{Label: "Repository", Value: name, Links: []factLink{link(name, "")}})
	}
	if b := rec.Branch; b != "" {
		f := fact{Label: "Branch", Value: b, Links: []factLink{link(b, "/tree/"+escapeRef(b))}}
		if base != "" {
			f.Links = append(f.Links, link("pull requests", "/pulls?q="+url.QueryEscape("is:pr head:"+b)))
		}
		facts = append(facts, f)
	}
	if isSHA(rec.StartHead) {
		facts = append(facts, fact{Label: "Started on", Value: tools.ShortSHA(rec.StartHead), Links: []factLink{link(tools.ShortSHA(rec.StartHead), "/commit/"+rec.StartHead)}})
	}
	if commits := rec.Result.Commits; rec.Done && len(commits) > 0 {
		f := fact{Label: "Commits", Value: fmt.Sprint(len(commits))}
		for _, c := range commits[:min(len(commits), commitLinks)] {
			if isSHA(c) {
				f.Links = append(f.Links, link(tools.ShortSHA(c), "/commit/"+c))
			}
		}
		if more := len(commits) - commitLinks; more > 0 {
			rest := factLink{Text: fmt.Sprintf("and %d more", more)}
			if end := rec.Result.EndHead; isSHA(rec.StartHead) && isSHA(end) {
				rest = link(rest.Text, "/compare/"+rec.StartHead+"..."+end)
			}
			f.Links = append(f.Links, rest)
		}
		facts = append(facts, f)
	}
	return facts
}

func escapeRef(ref string) string {
	parts := strings.Split(ref, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func isSHA(s string) bool {
	if len(s) < 7 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
