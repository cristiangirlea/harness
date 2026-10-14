package purpose

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestClassifyNamesTheRuleThatFired(t *testing.T) {
	cases := []struct {
		name, prompt, category, rule string
	}{
		// One row per rule.
		{"reviewer role", "You are an independent, read-only senior reviewer. Your job is to find real defects.", "review", "review.reviewer-role"},
		{"auditor role", "You are a security auditor for this repository.", "review", "review.reviewer-role"},
		{"before code", "Before writing any code, read the store layer and tell me what it does.", "plan", "plan.before-code"},
		{"write a plan", "Write a migration plan for the record format.", "plan", "plan.write-plan"},
		{"plan lead", "Design the retry policy for flaky gates.", "plan", "plan.lead"},
		{"ship ready", "Is this branch ready to merge?", "review", "review.ship-ready"},
		{"find breakage", "Go through the parser for bugs.", "review", "review.find-breakage"},
		{"anything that would break", "Tell me anything that would break when the flag goes away.", "review", "review.find-breakage"},
		{"review", "Review the diff on this branch.", "review", "review.review"},
		{"why does", "Why does the watcher drop the last event?", "debug", "debug.why-does"},
		{"error message", "Got this: TypeError: x is undefined on the watch page", "debug", "debug.error-message"},
		{"how does", "How does the guard handle a symlinked workspace?", "research", "research.how-does"},
		{"explain", "Explain the batch retry logic.", "research", "research.explain"},
		{"compare", "Compare bufio.Scanner and bufio.Reader for the codec.", "research", "research.compare"},
		{"refactor", "Refactor the renderer into smaller functions.", "refactor", "refactor.refactor"},
		{"behaviour preserving", "Remove the duplication in the CLI adapters.", "refactor", "refactor.behaviour-preserving"},
		{"restructure", "Extract the table renderer into its own file.", "refactor", "refactor.restructure"},
		{"ci", "Update the GitHub Actions workflow to cache modules.", "ops", "ops.ci-container"},
		{"deploy", "Roll back the change from yesterday.", "ops", "ops.deploy-release"},
		{"dependencies", "Bump the Go toolchain to 1.26.3.", "ops", "ops.dependencies"},
		{"coverage", "Improve coverage of the mcp codec.", "test", "test.coverage"},
		{"write tests", "Write unit tests for the parser.", "test", "test.write-tests"},
		{"repair tests", "Deflake the watcher tests.", "test", "test.repair"},
		{"fix", "Fix the off-by-one in shortKey.", "debug", "debug.fix"},
		{"symptom", "The watch page is broken on phones.", "debug", "debug.symptom"},
		{"add", "Add a --by purpose flag to hrn report.", "implement", "implement.add"},
		{"build", "Implement retries for the MCP client.", "implement", "implement.build"},

		// Precedence.
		{"plan beats implement", "Plan for adding a cache to the store.", "plan", "plan.lead"},
		{"a written plan beats a build lead", "Write a plan to add support for gemini.", "plan", "plan.write-plan"},
		{"test beats add", "Add tests for the purpose classifier.", "test", "test.write-tests"},
		{"reviewer role beats debug and ops words", "You are an independent reviewer: debug the panic: in CI and check the deploy.", "review", "review.reviewer-role"},
		{"ops beats test", "CI runs the tests twice; add the fuzz tests to it once.", "ops", "ops.ci-container"},

		// Only the lead counts.
		{"later paragraphs are ignored", "Implement the purpose classifier.\n\nWhen done, review the diff and check that CI passes.", "implement", "implement.build"},
		{"a paragraph is joined across its lines", "Go through the codec\nfor problems.\n\nThen fix them.", "review", "review.find-breakage"},
		{"crlf blank line ends the lead", "Implement it.\r\n\r\nReview the diff.", "implement", "implement.build"},
		{"bare cr blank line ends the lead", "Implement it.\r\rReview the diff.", "implement", "implement.build"},
		{"a paragraph is joined across bare cr lines", "Write\runit tests for the codec.\r\rThen fix them.", "test", "test.write-tests"},
		{"leading blank lines are skipped", "\n  \n\t\nFix the hang in batch.", "debug", "debug.fix"},

		// Nothing fires.
		{"empty", "", "unknown", ""},
		{"whitespace only", "  \n\t\r\n  ", "unknown", ""},
		{"vague", "Hello there.", "unknown", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Classify(c.prompt)
			if got.Category != c.category || got.Rule != c.rule {
				t.Errorf("Classify(%q) = %+v, want {%s %s}", c.prompt, got, c.category, c.rule)
			}
		})
	}
}

func TestClassifyReadsPastLabelsAndAContextParagraph(t *testing.T) {
	cases := []struct {
		name, prompt, category, rule string
	}{
		{"repository line before instruction", "Repository: hrn\nImplement the purpose classifier.", "implement", "implement.build"},
		{"batch context before goal", "Repository: hrn (harness), Go, standard library only. Read AGENTS.md, CLAUDE.md and .claude/rules/ first; they are the house rules.\n\nGoal (issue #82): on hrn watch's index, each run and each batch leads with its repository and its task, not its id. Add a repository column ...", "implement", "implement.add"},
		{"goal with note", "Goal (issue #82): fix the flaky ws create-lock test", "debug", "debug.fix"},
		{"reviewer role in first paragraph wins", "You are an independent, read-only senior reviewer. Your job is to find real defects.\n\nSecond round on branch x: review the diff", "review", "review.reviewer-role"},
		{"third paragraph never counts", "Hello there.\n\nThis is hrn.\n\nReview the diff for anything that would break.", "unknown", ""},
		{"label-only context before instruction", "Repository: hrn\nBranch: main\n\nAdd a repository column.", "implement", "implement.add"},
		{"context label", "Context: Implement the purpose classifier.", "implement", "implement.build"},
		{"three word label with digits slash and hyphen", "Task 2 Build/Goal-x: Implement the purpose classifier.", "implement", "implement.build"},
		{"four word prefix is not a label", "This has four words: Implement the purpose classifier.", "unknown", ""},
		{"punctuation is not a label", "Task!: Implement the purpose classifier.", "unknown", ""},
		{"empty labels before instruction", "Repository:\nContext:\n\nImplement the purpose classifier.", "implement", "implement.build"},
		{"first paragraph wins over earlier rule in second", "Implement the purpose classifier.\n\nReview the diff.", "implement", "implement.build"},
		{"crlf context", "Repository: hrn\r\n \r\nGoal: Implement the purpose classifier.", "implement", "implement.build"},
		{"bare cr context", "Repository: hrn\r\rGoal: Implement the purpose classifier.", "implement", "implement.build"},
		{"second paragraph has its own cap", strings.Repeat("x", maxLead+1) + "\n\nImplement the purpose classifier.", "implement", "implement.build"},
		{"second paragraph cannot match past cap", "Hello there.\n\n" + strings.Repeat("x", maxLead+1) + " review the diff.", "unknown", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Classify(c.prompt)
			if got.Category != c.category || got.Rule != c.rule {
				t.Errorf("Classify(%q) = %+v, want {%s %s}", c.prompt, got, c.category, c.rule)
			}
		})
	}
}

func TestLeadKeepsASingleLabelValue(t *testing.T) {
	if got := lead("Repository: hrn"); got != "hrn" {
		t.Errorf("lead = %q, want hrn", got)
	}
}

func TestEveryRuleIsNamedAfterAKnownCategory(t *testing.T) {
	want := []string{"review", "debug", "implement", "refactor", "test", "research", "plan", "ops", "unknown"}
	if !slices.Equal(Categories, want) {
		t.Fatalf("Categories = %v, want %v", Categories, want)
	}
	seen := map[string]bool{}
	for _, r := range rules {
		if seen[r.id] {
			t.Errorf("rule %s appears twice", r.id)
		}
		seen[r.id] = true
		if r.category == "unknown" || !slices.Contains(Categories, r.category) || !strings.HasPrefix(r.id, r.category+".") {
			t.Errorf("rule %s has category %q", r.id, r.category)
		}
	}
	if rules[0].id != "review.reviewer-role" {
		t.Errorf("first rule = %s, want hrn's own reviewer role", rules[0].id)
	}
}

func TestALongLeadIsCutOnARuneBoundary(t *testing.T) {
	// "Implement x" is 11 bytes and each é is 2, so byte 600 falls inside a rune.
	prompt := "Implement x" + strings.Repeat("É", 5000) + "\n\nreview the diff"
	l := lead(prompt)
	if len(l) > maxLead || len(l) < maxLead-utf8.UTFMax {
		t.Errorf("lead is %d bytes, want just under %d", len(l), maxLead)
	}
	if !utf8.ValidString(l) {
		t.Error("the cut split a rune")
	}
	if strings.ContainsRune(l, 'É') {
		t.Error("the lead was not lowercased")
	}
	if got := Classify(prompt); got.Category != "implement" {
		t.Errorf("Classify = %+v, want implement", got)
	}
}
