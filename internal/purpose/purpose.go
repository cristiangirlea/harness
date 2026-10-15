// Package purpose says what a run was for (review, debug, implement, ...) from
// its prompt, with ordered keyword rules: the first rule that matches wins.
//
// hrn prompts are long briefs whose later paragraphs mention every keyword
// ("review the diff", "CI", "deploy"), so only the first two non-blank paragraphs
// can supply the lead: the second is tried only if no rule matches the first.
// An instruction after leading label lines ("Repository: hrn") is tried before
// the paragraph as written. Each lead is lowercased and capped at maxLead bytes.
package purpose

import (
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// Categories in the order hrn lists them. Unknown means no rule fired.
var Categories = []string{"review", "debug", "implement", "refactor", "test", "research", "plan", "ops", "unknown"}

// Unknown is the category of a prompt no rule matches.
const Unknown = "unknown"

// Valid reports whether c is a category that callers may declare.
func Valid(c string) bool {
	return c != Unknown && slices.Contains(Categories, c)
}

// Declarable returns the categories callers may declare, in listing order.
func Declarable() []string {
	var categories []string
	for _, c := range Categories {
		if c != Unknown {
			categories = append(categories, c)
		}
	}
	return categories
}

// Declared reads a Purpose: <category> header on the first non-blank line.
// The label and category are case-insensitive. A non-category value, including
// unknown or free text, is ignored rather than treated as an error.
func Declared(prompt string) string {
	line, _ := firstNonBlankLine(prompt)
	label, value, ok := strings.Cut(line, ":")
	if ok && strings.EqualFold(label, "Purpose") {
		value = strings.TrimSpace(value)
		for _, category := range Categories {
			if category != Unknown && strings.EqualFold(value, category) {
				return category
			}
		}
	}
	return ""
}

// Resolve prefers an explicit declaration, then a header, then keyword rules.
func Resolve(declared, prompt string) Result {
	if !Valid(declared) {
		declared = Declared(prompt)
	}
	if declared != "" {
		return Result{Category: declared, Rule: "declared"}
	}
	// A free-text Purpose header is metadata, not classifier input.
	first, rest := firstNonBlankLine(prompt)
	if label, _, ok := strings.Cut(first, ":"); ok && strings.EqualFold(label, "Purpose") {
		prompt = rest
	}
	return Classify(prompt)
}

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
// two paragraphs that a rule matches. An anchored instruction after leading
// labels is tried first, then the raw lead, then the lead with labels skipped.
func Classify(prompt string) Result {
	for _, paragraph := range paragraphs(prompt) {
		u := unlabel(paragraph)
		var stripped string
		if u != paragraph {
			stripped = lead(u)
			for _, rl := range rules {
				if !strings.HasPrefix(rl.re.String(), "^") {
					continue
				}
				if loc := rl.re.FindStringIndex(stripped); loc != nil && loc[0] == 0 {
					// Keep rule precedence within the instruction ("Add a test"
					// is test, although the anchored add rule also matches).
					r, _ := match(stripped)
					return r
				}
			}
		}
		if r, ok := match(lead(paragraph)); ok {
			return r
		}
		if u != paragraph {
			if r, ok := match(stripped); ok {
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

// nextLine slices a line without copying or normalizing the remaining prompt.
func nextLine(prompt string) (line, rest string) {
	if i := strings.IndexAny(prompt, "\r\n"); i >= 0 {
		end := i + 1
		if prompt[i] == '\r' && end < len(prompt) && prompt[end] == '\n' {
			end++
		}
		return prompt[:i], prompt[end:]
	}
	return prompt, ""
}

func firstNonBlankLine(prompt string) (line, rest string) {
	for prompt != "" {
		line, prompt = nextLine(prompt)
		line = strings.TrimSpace(line)
		if line != "" {
			return line, prompt
		}
	}
	return "", ""
}

// paragraphs gathers leading label lines and at most maxLead instruction runes
// from each of the first two non-blank paragraphs. CRLF, LF and bare CR end lines;
// whitespace-only lines separate paragraphs.
func paragraphs(prompt string) []string {
	var result []string
	var paragraph strings.Builder
	runes := 0
	for prompt != "" {
		var line string
		line, prompt = nextLine(prompt)
		line = strings.TrimSpace(line)
		if line == "" {
			if paragraph.Len() > 0 {
				result = append(result, strings.TrimSuffix(paragraph.String(), "\n"))
				if len(result) == 2 {
					return result
				}
				paragraph.Reset()
				runes = 0
			}
			continue
		}
		if runes == maxLead {
			continue
		}
		if paragraph.Len() > 0 {
			paragraph.WriteByte('\n')
			if runes > 0 {
				runes++
			}
		}
		// Leading labels are removed by unlabel, so they do not use the
		// instruction's budget. Error lines remain part of the instruction.
		if runes == 0 && strings.Contains(line, ":") && !errorLine.MatchString(line) && label.MatchString(line) {
			paragraph.WriteString(line)
			continue
		}
		for _, r := range line {
			if runes == maxLead {
				break
			}
			paragraph.WriteRune(r)
			runes++
		}
		if runes == maxLead && len(result) == 1 {
			break
		}
	}
	if paragraph.Len() > 0 {
		result = append(result, strings.TrimSuffix(paragraph.String(), "\n"))
	}
	return result
}

var errorLine = regexp.MustCompile(`(?i)^([a-z]*(error|exception)|panic)\s*:`)

var label = regexp.MustCompile(`^[\p{L}\p{N}/-]+(?: +[\p{L}\p{N}/-]+){0,2}(?: +\([^()]*\))?:(?: +|$)`)

// unlabel drops a paragraph's leading label lines ("Repository: hrn", "Goal
// (issue #82): ..."); a last line that is itself a label keeps its value.
// Lines stay separated by "\n".
func unlabel(paragraph string) string {
	lines := strings.Split(paragraph, "\n")
	for len(lines) > 0 {
		if errorLine.MatchString(lines[0]) {
			break
		}
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
	// At most maxLead runes can contribute to maxLead lowercased bytes,
	// even when lowercasing shrinks a rune's UTF-8 encoding.
	runes := 0
	for i := range paragraph {
		if runes == maxLead {
			paragraph = paragraph[:i]
			break
		}
		runes++
	}
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
