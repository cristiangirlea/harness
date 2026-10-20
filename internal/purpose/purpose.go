// Package purpose says what a run was for (review, debug, implement, ...) from
// its prompt, with ordered keyword rules: the first rule that matches wins.
// Only a prompt's lead counts, since long briefs mention every keyword later
// (docs/commands.md, "Declared purpose").
package purpose

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
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

// Inherited is the purpose a continuation keeps from the run it follows: that
// run's declaration or classified purpose, "" when it was unknown.
func Inherited(declared, prompt string) string {
	if c := Resolve(declared, prompt).Category; c != Unknown {
		return c
	}
	return ""
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
	// firstSentence keeps a keyword rule off the constraints and context
	// that follow the instruction ("no docker", "never deploy").
	firstSentence bool
}

func r(id, category, expr string) rule {
	return rule{id: id, category: category, re: regexp.MustCompile(expr)}
}

func r1(id, category, expr string) rule {
	return rule{id: id, category: category, re: regexp.MustCompile(expr), firstSentence: true}
}

// Keyword bounds that keep a branch, file or path name ("ci/pin", "deploy-x",
// "ci.yml") from counting; a full stop that ends a sentence still bounds.
const (
	nameStart = `(?:^|[^\w/\\.-])`
	nameEnd   = `(?:[^\w/\\.-]|\.(?:\W|$)|$)`
)

// rules are tried in order. The first is hrn's own reviewer brief; the rest
// are adapted from classify-sessions rules-v1.
var rules = []rule{
	r("review.reviewer-role", "review", `\byou are an? ([a-z-]+,? )*(reviewer|auditor)\b`),
	// Only at the start: an implement brief often names the review that follows it.
	r("review.brief", "review", `^(?:(?:final|narrow|short|bounded|focused|independent|read-only|pre-merge|merged-head|exact-head|verification|follow-up|fix-round|confirmation|attacker-style|second|third|security|code)[ ,]+){1,4}(?:review|verification|security read)(?: (?:of|on|for|at|in|with|and|only)\b| ?[(,.:;]|$)|^(?:(?:final|narrow|short|bounded|focused|independent|read-only)[ ,]+){0,4}re-review\b|^verify (?:the (?:newest|latest|fix|fixes|head)|commit)\b|^you are re-?reviewing\b|^continue the review\b`),
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
	r1("research.compare", "research", nameStart+`(?:compare|versus|vs|trade-?offs?)`+nameEnd+`|\bpros and cons\b|\bdifference between\b|\bshould (i|we) use\b`),
	r("refactor.refactor", "refactor", `\brefactor`),
	r("refactor.behaviour-preserving", "refactor", `\bno behaviou?r(al)? changes?\b|\bwithout changing (the |its |any )?behaviou?r\b|\bremove (the )?duplication\b|\bdeduplicate\b`),
	r("refactor.restructure", "refactor", `^(extract|rename|restructure|reorgani[sz]e|split|inline|simplify|clean up|tidy up|consolidate|decouple|modulari[sz]e)\b`),
	r1("ops.ci-container", "ops", nameStart+`(?:dockerfile|docker|docker-compose|github actions|gitlab ci|ci|pipeline|kubernetes|k8s|helm|terraform|ansible|nginx)`+nameEnd),
	r1("ops.deploy-release", "ops", nameStart+`(?:deploy|deploys|deployment|release|rollback|roll back|provision)`+nameEnd),
	r("ops.dependencies", "ops", `\b(upgrade|bump)\b|\bdependenc(y|ies)\b|\blockfile\b|\b(npm|pip|go mod|poetry|yarn|pnpm) (install|update|tidy|audit)\b|\bmakefile\b|\bbuild script\b`),
	r("test.coverage", "test", `\b(increase|improve|raise|extend|add|boost|expand)\b[^.;]*\bcoverage\b|\bcoverage (on|for|of)\b`),
	r("test.write-tests", "test", `\b(write|add|create|extend|expand|generate)\b (a |an |some |more |the |new )?([a-z-]+ ){0,2}(tests?|test cases)\b`),
	r("test.repair", "test", `\b(repair|update|stabili[sz]e|deflake|unskip)\b (the )?([a-z-]+ ){0,2}tests?\b|\bflaky tests?\b`),
	r("debug.fix", "debug", `^(please )?(fix|debug)\b|\b(investigate|diagnose|troubleshoot)\b`),
	r("debug.symptom", "debug", `\b(is|are|keeps) (broken|failing|crashing|hanging|flaky)\b|\b(crash|crashes|crashed|hangs|regressed|returns the wrong)\b|\b(doesn't|does not|isn't|is not) work(ing)?\b|\bnot working\b|\bstopped working\b`),
	r("implement.add", "implement", `^(please )?add\b|\badd (a|an|the|support for|new)\b`),
	r("implement.build", "implement", `^(please )?(implement|create|build|write|introduce|support|make|generate|scaffold|wire up|set up|integrate)\b|^you are implementing\b`),
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
	first := firstSentence(l)
	for _, rl := range rules {
		s := l
		if rl.firstSentence {
			s = first
		}
		if rl.re.MatchString(s) {
			return Result{Category: rl.category, Rule: rl.id}, true
		}
	}
	return Result{}, false
}

// firstSentence cuts a lead at the first ".", ";", "?" or "!" before white
// space, except the full stops of "e.g." and "i.e.".
func firstSentence(l string) string {
	for i := 0; i+1 < len(l); i++ {
		if strings.IndexByte(".;?!", l[i]) < 0 {
			continue
		}
		if r, _ := utf8.DecodeRuneInString(l[i+1:]); !unicode.IsSpace(r) {
			continue
		}
		if abbreviation(l[:i+1]) {
			continue
		}
		return l[:i]
	}
	return l
}

// abbreviation reports whether s ends in a whole "e.g." or "i.e.".
func abbreviation(s string) bool {
	if !strings.HasSuffix(s, "e.g.") && !strings.HasSuffix(s, "i.e.") {
		return false
	}
	r, _ := utf8.DecodeLastRuneInString(s[:len(s)-4])
	return len(s) == 4 || !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '.'
}

// handover drops a hand-over marker ("## Hand-over", "Hand-over: fix x") from
// a line, keeping what follows it; false when nothing is left.
func handover(line string) (string, bool) {
	rest := strings.TrimLeft(strings.TrimLeft(line, "#"), " \t")
	for _, marker := range []string{"hand-over", "handover"} {
		if len(rest) < len(marker) || !strings.EqualFold(rest[:len(marker)], marker) {
			continue
		}
		after := rest[len(marker):]
		if r, _ := utf8.DecodeRuneInString(after); after != "" && (unicode.IsLetter(r) || unicode.IsNumber(r)) {
			return line, true
		}
		after = strings.TrimLeft(after, " \t.:|*-–—")
		return after, after != ""
	}
	return line, true
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

// paragraphs gathers the latest leading label (see boundLabel) and the instruction,
// capped at maxLead runes, from each of the first two non-blank paragraphs. CRLF, LF and
// bare CR end lines; whitespace-only lines separate paragraphs.
func paragraphs(prompt string) []string {
	var result []string
	var paragraph strings.Builder
	var lastLabel string
	runes := 0
	for prompt != "" {
		var line string
		line, prompt = nextLine(prompt)
		line = strings.TrimSpace(line)
		var keep bool
		if line, keep = handover(line); !keep {
			continue
		}
		if line == "" {
			if paragraph.Len() > 0 || lastLabel != "" {
				result = append(result, strings.TrimSuffix(lastLabel+paragraph.String(), "\n"))
				if len(result) == 2 {
					return result
				}
				paragraph.Reset()
				lastLabel = ""
				runes = 0
			}
			continue
		}
		if runes == maxLead {
			continue
		}
		// Keep only the latest leading label, bounded separately so labels
		// cannot exhaust the instruction's budget or grow the gathered text.
		// Error lines remain part of the instruction.
		if runes == 0 {
			if colon, open := leadingLabel(line); colon >= 0 {
				lastLabel = boundLabel(line, colon, open)
				continue
			}
		}
		if paragraph.Len() > 0 || lastLabel != "" {
			paragraph.WriteByte('\n')
			if runes > 0 {
				runes++
			}
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
	if paragraph.Len() > 0 || lastLabel != "" {
		result = append(result, strings.TrimSuffix(lastLabel+paragraph.String(), "\n"))
	}
	return result
}

var errorLine = regexp.MustCompile(`(?i)^([a-z]*(error|exception)|panic)\s*:`)

var label = regexp.MustCompile(`^[\p{L}\p{N}/-]+(?: +[\p{L}\p{N}/-]+){0,2}(?: +\([^()]*\))?:(?: +|$)`)

// leadingLabel returns label's colon and bracket indexes (-1 when absent)
// without regexp scratch allocations, which sync.Pool may discard under -race.
// Words past maxLead runes are not a label, so a kept label stays bounded.
func leadingLabel(line string) (colon, open int) {
	i := 0
	for words := 0; words < 3; words++ {
		start := i
		for i < len(line) {
			r, size := utf8.DecodeRuneInString(line[i:])
			if !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '/' && r != '-' {
				break
			}
			i += size
		}
		if i == start {
			return -1, -1
		}
		if i < len(line) && line[i] == ':' {
			if words == 0 && errorLabel(line[:i]) {
				return -1, -1
			}
			if (i+1 == len(line) || line[i+1] == ' ') && utf8.RuneCountInString(line[:i]) <= maxLead {
				return i, -1
			}
			return -1, -1
		}
		if i == len(line) || line[i] != ' ' {
			return -1, -1
		}
		for i < len(line) && line[i] == ' ' {
			i++
		}
		if i < len(line) && line[i] == '(' {
			open = i
			i++
			end := strings.IndexAny(line[i:], "()")
			if end < 0 || line[i+end] != ')' {
				return -1, -1
			}
			i += end + 1
			if i < len(line) && line[i] == ':' && (i+1 == len(line) || line[i+1] == ' ') && utf8.RuneCountInString(line[:open]) <= maxLead {
				return i, open
			}
			return -1, -1
		}
	}
	return -1, -1
}

// boundLabel caps a label's bracketed part and its value at maxLead runes
// each, keeping the colon so the label stays a label and its value whole.
func boundLabel(line string, colon, open int) string {
	if len(line) <= maxLead {
		return line
	}
	prefix := line[:colon+1]
	if open >= 0 {
		prefix = line[:open+1] + capRunes(line[open+1:colon-1]) + "):"
	}
	if value := capRunes(strings.TrimLeft(line[colon+1:], " ")); value != "" {
		return prefix + " " + value
	}
	return prefix
}

func capRunes(s string) string {
	runes := 0
	for i := range s {
		if runes == maxLead {
			return s[:i]
		}
		runes++
	}
	return s
}

// errorLabel is errorLine's name check for a single label word. Only ASCII
// letters and their Unicode case-fold equivalents can precede error/exception.
func errorLabel(name string) bool {
	if strings.EqualFold(name, "panic") {
		return true
	}
	for _, suffix := range []string{"error", "exception"} {
		if len(name) < len(suffix) || !strings.EqualFold(name[len(name)-len(suffix):], suffix) {
			continue
		}
		for _, r := range name[:len(name)-len(suffix)] {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '\u212a' || r == '\u017f') {
				return false
			}
		}
		return true
	}
	return false
}

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
