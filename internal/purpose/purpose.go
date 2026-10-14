// Package purpose says what a run was for (review, debug, implement, ...) from
// its prompt, with ordered keyword rules: the first rule that matches wins.
//
// hrn prompts are long briefs whose later paragraphs mention every keyword
// ("review the diff", "CI", "deploy"), so only the first two non-blank paragraphs
// can supply the lead: the second is tried only if no rule matches the first.
// A paragraph is read as written, then with its leading label lines
// ("Repository: hrn") skipped. Each lead is lowercased and capped at maxLead bytes.
package purpose

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Categories in the order hrn lists them. Unknown means no rule fired.
var Categories = []string{"review", "debug", "implement", "refactor", "test", "research", "plan", "ops", "unknown"}

// Unknown is the category of a prompt no rule matches.
const Unknown = "unknown"

// Result is a prompt's category and the id of the rule that put it there ("" for
// unknown). It never carries prompt text.
type Result struct {
	Category string `json:"category"`
	Rule     string `json:"rule,omitempty"`
}

// maxLead caps the bytes of the lead the rules see.
const maxLead = 600

type rule struct {
	id, category string
	re           *regexp.Regexp
}

func r(id, category, expr string) rule {
	return rule{id: id, category: category, re: regexp.MustCompile(expr)}
}

// rules are tried in order. The first is hrn's own reviewer brief; the rest
// are adapted from classify-sessions rules-v1.
var rules = []rule{
	r("review.reviewer-role", "review", `\byou are an? ([a-z-]+,? )*(reviewer|auditor)\b`),
	r("plan.before-code", "plan", `\bbefore (you )?(writing|write|making|changing|touching|implementing) (any )?(code|changes)\b`),
	r("plan.write-plan", "plan", `\b(give me|write|draft|propose|outline|sketch|prepare|come up with) (me )?(a |an )?([a-z-]+ )?(plan|design|spec|specification|proposal|rfc|roadmap)\b`),
	r("plan.lead", "plan", `^(plan|design)\b`),
	r("review.ship-ready", "review", `\b(good enough|ready|safe) to (ship|merge|release|deploy)\b`),
	r("review.find-breakage", "review", `\b(look at|look over|go over|go through|check|audit|inspect)\b.*\bfor (anything|problems|issues|bugs|risks)\b|\banything that (would|could|might) break\b`),
	r("review.review", "review", `^(please )?(code[- ]?)?review\b|\breview (the|this|my|our) |\bwhat would you block on\b`),
	r("debug.why-does", "debug", `^(why (does|do|is|are|did|would|won't|doesn't|isn't|can't)|how come)\b`),
	r("debug.error-message", "debug", `\b[a-z]*(error|exception)\s*:|\btraceback\b|\bpanic:|\bstack ?trace\b|\bsegfault\b|\bsegmentation fault\b`),
	r("research.how-does", "research", `\bhow does\b[^?]*\b(work|works|handle|handles|implement|implements|manage|manages|resolve|resolves|decide|decides|store|stores|load|loads)\b`),
	r("research.explain", "research", `^(explain|describe|walk me through|tell me about|what is|what are)\b|\bexplain how\b|\bpoint me (at|to)\b`),
	r("research.compare", "research", `\bcompare\b|\bpros and cons\b|\btrade-?offs?\b|\bdifference between\b|\bshould (i|we) use\b|\b(versus|vs)\b`),
	r("refactor.refactor", "refactor", `\brefactor`),
	r("refactor.behaviour-preserving", "refactor", `\bno behaviou?r(al)? changes?\b|\bwithout changing (the |its |any )?behaviou?r\b|\bremove (the )?duplication\b|\bdeduplicate\b`),
	r("refactor.restructure", "refactor", `^(extract|rename|restructure|reorgani[sz]e|split|inline|simplify|clean up|tidy up|consolidate|decouple|modulari[sz]e)\b`),
	r("ops.ci-container", "ops", `\b(dockerfile|docker|docker-compose|github actions|gitlab ci|ci|pipeline|kubernetes|k8s|helm|terraform|ansible|nginx)\b`),
	r("ops.deploy-release", "ops", `\b(deploy|deploys|deployment|release|rollback|roll back|provision)\b`),
	r("ops.dependencies", "ops", `\b(upgrade|bump)\b|\bdependenc(y|ies)\b|\blockfile\b|\b(npm|pip|go mod|poetry|yarn|pnpm) (install|update|tidy|audit)\b|\bmakefile\b|\bbuild script\b`),
	r("test.coverage", "test", `\b(increase|improve|raise|extend|add|boost|expand)\b[^.;]*\bcoverage\b|\bcoverage (on|for|of)\b`),
	r("test.write-tests", "test", `\b(write|add|create|extend|expand|generate)\b (a |an |some |more |the |new )?([a-z-]+ ){0,2}(tests?|test cases)\b`),
	r("test.repair", "test", `\b(repair|update|stabili[sz]e|deflake|unskip)\b (the )?([a-z-]+ ){0,2}tests?\b|\bflaky tests?\b`),
	r("debug.fix", "debug", `^(please )?(fix|debug)\b|\b(investigate|diagnose|troubleshoot)\b`),
	r("debug.symptom", "debug", `\b(is|are|keeps) (broken|failing|crashing|hanging|flaky)\b|\b(crash|crashes|crashed|hangs|regressed|returns the wrong)\b|\b(doesn't|does not|isn't|is not) work(ing)?\b|\bnot working\b|\bstopped working\b`),
	r("implement.add", "implement", `^(please )?add\b|\badd (a|an|the|support for|new)\b`),
	r("implement.build", "implement", `^(please )?(implement|create|build|write|introduce|support|make|generate|scaffold|wire up|set up|integrate)\b`),
}

// Classify names what a prompt asks for, from its lead: the first of at most
// two paragraphs that a rule matches, each read as written first (so "panic:"
// and "C:\" stay what they are) and then with its leading label lines skipped.
func Classify(prompt string) Result {
	for _, paragraph := range paragraphs(prompt) {
		if r, ok := match(lead(paragraph)); ok {
			return r
		}
		if u := unlabel(paragraph); u != paragraph {
			if r, ok := match(lead(u)); ok {
				return r
			}
		}
	}
	return Result{Category: Unknown}
}

// match tries the rules in order on a lead; false when none fires.
func match(l string) (Result, bool) {
	if l == "" {
		return Result{}, false
	}
	for _, rl := range rules {
		if rl.re.MatchString(l) {
			return Result{Category: rl.category, Rule: rl.id}, true
		}
	}
	return Result{}, false
}

// paragraphs returns at most the first two non-blank paragraphs. "\r\n", "\n"
// and a bare "\r" each end a line; whitespace-only lines separate paragraphs.
func paragraphs(prompt string) []string {
	var result []string
	var lines []string
	prompt = strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(prompt)
	for line := range strings.Lines(prompt) {
		line = strings.TrimSpace(line)
		if line == "" {
			if len(lines) > 0 {
				result = append(result, strings.Join(lines, "\n"))
				if len(result) == 2 {
					return result
				}
				lines = nil
			}
			continue
		}
		lines = append(lines, line)
	}
	if len(lines) > 0 {
		result = append(result, strings.Join(lines, "\n"))
	}
	return result
}

var label = regexp.MustCompile(`^[\p{L}\p{N}/-]+(?: +[\p{L}\p{N}/-]+){0,2}(?: +\([^()]*\))?: *`)

// unlabel drops a paragraph's leading label lines ("Repository: hrn", "Goal
// (issue #82): ..."); a last line that is itself a label keeps its value.
// Lines stay separated by "\n".
func unlabel(paragraph string) string {
	lines := strings.Split(paragraph, "\n")
	for len(lines) > 0 {
		end := label.FindStringIndex(lines[0])
		if end == nil {
			break
		}
		if len(lines) == 1 {
			lines[0] = strings.TrimSpace(lines[0][end[1]:])
			break
		}
		lines = lines[1:]
	}
	return strings.Join(lines, "\n")
}

// lead is a paragraph's lines joined by spaces, lowercased and cut to maxLead
// bytes without splitting a rune.
func lead(paragraph string) string {
	l := strings.ToLower(strings.ReplaceAll(paragraph, "\n", " "))
	if len(l) <= maxLead {
		return l
	}
	cut := maxLead
	for cut > 0 && !utf8.RuneStart(l[cut]) {
		cut--
	}
	return l[:cut]
}
