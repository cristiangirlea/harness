package purpose

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestValidAcceptsOnlyDeclaredCategories(t *testing.T) {
	for _, c := range Categories {
		if got := Valid(c); got != (c != Unknown) {
			t.Errorf("Valid(%q) = %v", c, got)
		}
	}
	for _, c := range []string{"", "Review", " review", "free text"} {
		if Valid(c) {
			t.Errorf("Valid(%q) = true", c)
		}
	}
}

func TestDeclaredReadsOnlyTheFirstNonBlankLine(t *testing.T) {
	for _, tc := range []struct{ prompt, want string }{
		{"Purpose: review\nImplement it.", "review"},
		{"\n \t\r\n pUrPoSe: ReFaCtOr \r\nFix it.", "refactor"},
		{"\rPurpose: OPS\rFix it.", "ops"},
		{"Purpose: fix the bug\nReview it.", ""},
		{"Purpose: unknown\nReview it.", ""},
		{"Purpose: UNKNOWN", ""},
		{"Purpose:", ""},
		{"Review it.\nPurpose: debug", ""},
		{"Repository: hrn\nPurpose: debug", ""},
		{"Purposeful: review", ""},
		{"", ""},
		{" \n\t", ""},
	} {
		if got := Declared(tc.prompt); got != tc.want {
			t.Errorf("Declared(%q) = %q, want %q", tc.prompt, got, tc.want)
		}
	}
	for _, c := range Categories {
		if c != Unknown && Declared("Purpose: "+strings.ToUpper(c)) != c {
			t.Errorf("header does not accept %q", c)
		}
	}
}

func TestResolvePrefersDeclarationsAndKeepsTheFallbackRule(t *testing.T) {
	for _, tc := range []struct {
		declared, prompt string
		want             Result
	}{
		{"plan", "Purpose: review\nFix the bug.", Result{Category: "plan", Rule: "declared"}},
		{"", "Purpose: review\nFix the bug.", Result{Category: "review", Rule: "declared"}},
		{"", "Fix the bug.", Classify("Fix the bug.")},
		{"", "Purpose: free text\nReview the diff.", Classify("Purpose: free text\nReview the diff.")},
		{"", "Purpose: unknown\nReview the diff.", Classify("Purpose: unknown\nReview the diff.")},
		{"", "", Result{Category: Unknown}},
	} {
		if got := Resolve(tc.declared, tc.prompt); got != tc.want {
			t.Errorf("Resolve(%q, %q) = %+v, want %+v", tc.declared, tc.prompt, got, tc.want)
		}
	}
}

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
