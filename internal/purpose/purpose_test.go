package purpose

import (
	"fmt"
	"runtime"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func BenchmarkParagraphsLabels(b *testing.B) {
	for _, mib := range []int{1, 4, 16} {
		b.Run(fmt.Sprintf("short/%dMiB", mib), func(b *testing.B) {
			prompt := strings.Repeat("A: b\n", (mib<<20)/len("A: b\n"))
			b.ReportAllocs()
			for b.Loop() {
				paragraphs(prompt)
			}
		})
	}
	b.Run("label-and-text/4MiB", func(b *testing.B) {
		prompt := strings.Repeat("A: b\ntext\n", (4<<20)/len("A: b\ntext\n"))
		b.ReportAllocs()
		for b.Loop() {
			paragraphs(prompt)
		}
	})
}

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
		{"free text", "Purpose: review\nFix it.", Result{Category: "review", Rule: "declared"}},
		{"free text", "Implement it.", Classify("Implement it.")},
		{"unknown", "Fix it.", Classify("Fix it.")},
		{"", "Purpose: deploy notes\nAdd a test.", Classify("Add a test.")},
		{"", "\r\n pUrPoSe: deploy notes\r\nAdd a test.", Classify("Add a test.")},
		{"", "Purpose: deploy notes", Result{Category: Unknown}},
		{"", "Purposeful: deploy notes", Classify("Purposeful: deploy notes")},
		{"", "Purpose : deploy notes", Classify("Purpose : deploy notes")},
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
		{"error before build verb", "Error: build failed", "debug", "debug.error-message"},
		{"type error before make verb", "TypeError: make is not a function", "debug", "debug.error-message"},
		{"panic before split verb", "panic: split slice out of range", "debug", "debug.error-message"},
		{"panic before instruction", "panic: nil map write\nAdd a nil check to the loader.", "debug", "debug.error-message"},
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
		{"repository keywords before instruction", "Repository: hrn (Go, CI on GitHub)\nImplement the purpose classifier.", "implement", "implement.build"},
		{"context keywords before instruction", "Context: deploy notes\nExplain the classifier.", "research", "research.explain"},
		{"labels exceed cap before instruction", strings.Repeat("Context: "+strings.Repeat("x", 80)+"\n", 8) + "Refactor the loader.", "refactor", "refactor.refactor"},
		{"labels exceed cap before long instruction", strings.Repeat("Context: "+strings.Repeat("x", 80)+"\n", 8) + "Refactor the loader. " + strings.Repeat("X", 4<<20), "refactor", "refactor.refactor"},
		{"instruction keeps test precedence", "Repository: hrn (CI)\nAdd a test.", "test", "test.write-tests"},
		{"url lead stays intact", "https://example.org/x", "unknown", ""},
		{"windows path lead stays intact", `C:\repo\x`, "unknown", ""},
		{"fix windows path", `fix C:\repo\x`, "debug", "debug.fix"},
		{"panic value keeps diagnostic precedence", "panic: deployment failed", "debug", "debug.error-message"},
		{"exception value keeps diagnostic precedence", "TypeError: cannot compare values", "debug", "debug.error-message"},
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
		{"a panic line is an error, not a label", "panic: test timed out after 10m0s", "debug", "debug.error-message"},
		{"an exception name is an error, not a label", "TypeError: x is undefined", "debug", "debug.error-message"},
		{"an error word before a code is an error, not a label", "Error: ENOENT: no such file or directory", "debug", "debug.error-message"},
		{"a drive letter is not a label", "fix C:\\repo\\README.md", "debug", "debug.fix"},
		{"only leading labels are skipped", "Repository: hrn\nImplement x\nBranch: y", "implement", "implement.build"},
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

func TestUnlabelKeepsASingleLabelValue(t *testing.T) {
	if got := unlabel("Repository: hrn"); got != "hrn" {
		t.Errorf("unlabel = %q, want hrn", got)
	}
}

func TestUnlabelRequiresSpaceOrEndAfterTheColon(t *testing.T) {
	for _, tc := range []struct{ prompt, want string }{
		{"https://example.org/x\nHello.", "https://example.org/x\nHello."},
		{`C:\repo\x` + "\nHello.", `C:\repo\x` + "\nHello."},
		{"E:/repo/x\nHello.", "E:/repo/x\nHello."},
		{"Repository: hrn\nHello.", "Hello."},
		{"Repository:\nHello.", "Hello."},
	} {
		if got := unlabel(tc.prompt); got != tc.want {
			t.Errorf("unlabel(%q) = %q, want %q", tc.prompt, got, tc.want)
		}
	}
}

func TestDeclarableExcludesUnknownRegardlessOfItsPosition(t *testing.T) {
	original := Categories
	t.Cleanup(func() { Categories = original })
	Categories = []string{Unknown, "review", "debug"}
	if got := Declarable(); !slices.Equal(got, []string{"review", "debug"}) {
		t.Errorf("Declarable = %v, want review and debug", got)
	}
}

func TestLeadBoundsWorkForALongFirstLine(t *testing.T) {
	prompt := "Implement " + strings.Repeat("X", 4<<20)
	if got, want := Classify(prompt), Classify(prompt[:maxLead]); got != want {
		t.Errorf("long prompt = %+v, prefix = %+v", got, want)
	}
	if got := testing.AllocsPerRun(10, func() { lead(prompt) }); got > 2 {
		t.Errorf("lead allocated %g times per call, want at most 2", got)
	}
}

func TestLeadCutsAfterLowercasingABoundedPrefix(t *testing.T) {
	for _, tc := range []struct{ prompt, want string }{
		{strings.Repeat("İ", maxLead) + "REVIEW", strings.Repeat("i", maxLead)},
		{strings.Repeat("Ⱥ", maxLead) + "REVIEW", strings.Repeat("ⱥ", maxLead/3)},
		{strings.Repeat("x\n", maxLead), strings.Repeat("x ", maxLead/2)},
	} {
		if got := lead(tc.prompt); got != tc.want || !utf8.ValidString(got) {
			t.Errorf("lead = %q, want %q", got, tc.want)
		}
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

func TestParagraphsBoundGathering(t *testing.T) {
	for _, text := range []string{strings.Repeat("X", 4<<20), strings.Repeat("X\n", 2<<20)} {
		got := paragraphs(text + "\n\nImplement it.")
		if len(got) != 2 || len(got[0]) > maxLead*utf8.UTFMax || got[1] != "Implement it." {
			t.Errorf("paragraphs did not bound the first paragraph and preserve the second")
		}
	}
}

func TestParagraphsBoundGatheringAfterLabels(t *testing.T) {
	header := "Context: " + strings.Repeat("x", 80) + "\n"
	prefix := header + "Refactor the loader. " + strings.Repeat("X", maxLead-len("Refactor the loader. "))
	prompt := strings.Repeat(header, 8) + strings.TrimPrefix(prefix, header) + strings.Repeat("X", 4<<20)
	if got := paragraphs(prompt); !slices.Equal(got, []string{prefix}) {
		t.Error("paragraphs did not preserve the latest label and cap the instruction")
	}
	if got := allocatedBytes(func() { paragraphs(prompt) }); got > 64<<10 {
		t.Errorf("long instruction allocated %d bytes, want at most 64 KiB", got)
	}
	// Race mode randomly discards sync.Pool entries used by regexp, so exact
	// allocation counts are unstable. The byte bound above still runs there.
	if !raceEnabled {
		baseline := testing.AllocsPerRun(10, func() { paragraphs(prefix) })
		if got := testing.AllocsPerRun(10, func() { paragraphs(prompt) }); got > baseline {
			t.Errorf("long instruction allocated %g times, bounded prefix allocated %g", got, baseline)
		}
	}
}

func TestParagraphsBoundLabelGathering(t *testing.T) {
	for _, tc := range []struct{ name, prompt, want string }{
		{"one long label", "Context: " + strings.Repeat("X", 4<<20), "Context: " + strings.Repeat("X", maxLead-len("Context: "))},
		{"many short labels", strings.Repeat("A: b\n", (4<<20)/len("A: b\n")) + "Last: value", "Last: value"},
		{"unicode label", "Context: " + strings.Repeat("\U0001f600", maxLead), "Context: " + strings.Repeat("\U0001f600", maxLead-len("Context: "))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, suffix := range []string{"", "\n" + strings.Repeat("x", maxLead*2) + "\n\nImplement it."} {
				prompt := tc.prompt + suffix
				want := []string{tc.want}
				if suffix != "" {
					want = []string{tc.want + "\n" + strings.Repeat("x", maxLead), "Implement it."}
				}
				if got := paragraphs(prompt); !slices.Equal(got, want) {
					t.Error("paragraphs did not cap the latest label and preserve the instruction budget")
				}
			}
			allocated := allocatedBytes(func() { paragraphs(tc.prompt) })
			if allocated > 64<<10 {
				t.Errorf("paragraphs allocated %d bytes per call, want at most 64 KiB", allocated)
			}
		})
	}
}

func TestLeadingLabelsMatchTheGrammarWithoutScratchAllocations(t *testing.T) {
	lines := []string{
		"", "A: b", "A:", "A:two", "A:\tb", "A : b", "A  B: c",
		"A B C: d", "A B C D: e", "A (note: value): b", "A (): b",
		"A (nested (note)): b", "A (unclosed: b", "A (note) : b",
		"A B C (note): d", "A B C D (note): e", "A\tB: c",
		"Task 2 Build/Goal-x: b", "\u00e9 \u2160: b", "\ufffd: b", "\xff: b",
		"Error: b", "TypeError: b", "EXCEPTION: b", "panic: b",
		"\u212aError: b", "\u017fException: b", "\u00e9Error: b", "1Error: b",
		"A-Error: b", "A/Error: b", "A Error: b", "Error (note): b",
	}
	for _, line := range lines {
		want := !errorLine.MatchString(line) && label.MatchString(line)
		if got := leadingLabel(line); got != want {
			t.Errorf("leadingLabel(%q) = %v, want %v", line, got, want)
		}
	}
	if got := allocatedBytes(func() {
		for _, line := range lines {
			leadingLabel(line)
		}
	}); got > 1<<10 {
		t.Errorf("label checks allocated %d bytes, want at most 1 KiB", got)
	}
}

// allocatedBytes measures a cold call: two GCs evict regexp's pooled scratch
// space so a warm pool cannot hide allocations. Do not use in parallel tests.
func allocatedBytes(f func()) uint64 {
	runtime.GC()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

func TestParagraphsTrimNewlineAtCap(t *testing.T) {
	for _, prefix := range []string{"", "Hello.\n\n"} {
		for _, suffix := range []string{"", "\n\nHello."} {
			want := strings.Repeat("x", maxLead-1)
			got := paragraphs(prefix + want + "\nx" + suffix)
			index := 0
			if prefix != "" {
				index = 1
			}
			if got[index] != want {
				t.Errorf("paragraph with prefix %q and suffix %q retained a trailing newline", prefix, suffix)
			}
		}
	}
}

func TestDeclaredDoesNotCopyLongPrompts(t *testing.T) {
	for _, prompt := range []string{
		"Purpose: review\r\n" + strings.Repeat("X", 4<<20),
		"Purpose: " + strings.Repeat("X", 4<<20) + "\r\n",
	} {
		if got := testing.AllocsPerRun(10, func() { Declared(prompt) }); got != 0 {
			t.Errorf("Declared allocated %g times, want no copies", got)
		}
	}
}

func TestResolveDoesNotCopyThePromptBody(t *testing.T) {
	prompt := "Purpose: deploy notes\r\nImplement it.\r\n\r\n" + strings.Repeat("X", 4<<20)
	body := strings.TrimPrefix(prompt, "Purpose: deploy notes\r\n")
	// Allow cold regexp scratch for every rule (race mode can discard every
	// pooled matcher), but never a copy of the 4 MiB body.
	if got := allocatedBytes(func() { Resolve("", prompt) }); got > 2<<20 {
		t.Errorf("Resolve allocated %d bytes, want at most 2 MiB", got)
	}
	// Race mode randomly drops regexp's sync.Pool entries; compare counts only
	// without instrumentation, keeping the byte bound active in both builds.
	if !raceEnabled {
		baseline := testing.AllocsPerRun(10, func() { Classify(body) })
		if got := testing.AllocsPerRun(10, func() { Resolve("", prompt) }); got > baseline {
			t.Errorf("Resolve allocated %g times, Classify allocated %g", got, baseline)
		}
	}
	if got := Resolve("", prompt); got != Classify("Implement it.") {
		t.Errorf("Resolve = %+v, want implement.build", got)
	}
}
