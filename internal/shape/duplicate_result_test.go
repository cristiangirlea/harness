package shape

import "testing"

func TestRepeatedResultNamesWithAFailureFallBack(t *testing.T) {
	for _, name := range []string{")", "TestSame"} {
		for _, results := range [][2]string{{"FAIL", "PASS"}, {"PASS", "FAIL"}, {"FAIL", "FAIL"}} {
			for _, diagnostic := range []string{"F", "ordinary diagnostic"} {
				t.Run(name+"/"+results[0]+"/"+results[1]+"/"+diagnostic, func(t *testing.T) {
					text := "--- " + results[0] + ": " + name + "\n--- " + results[1] + ": " + name + "\n" + diagnostic
					if s, ok := Summarize(Test, text); ok {
						t.Fatalf("accepted ambiguous result names: %q", s)
					}
				})
			}
		}
	}
}

func TestAMarkerAfterAFailureFallsBack(t *testing.T) {
	for _, marker := range []string{"PAUSE", "RUN"} {
		text := "--- FAIL: )\n=== " + marker + " )\nF"
		if s, ok := Summarize(Test, text); ok {
			if lost := lostFact(text, s); lost != "" {
				t.Fatalf("%s after a failure: %s\nsummary: %q", marker, lost, s)
			}
		}
	}
}

func TestRepeatedPassingResultsStillSummarize(t *testing.T) {
	text := "--- PASS: TestSame (0.00s)\n--- PASS: TestSame (0.00s)\n--- FAIL: TestBad (0.00s)\n    bad_test.go:1: no\nFAIL\nFAIL\tex\t0.01s\n"
	s, ok := Summarize(Test, text)
	if !ok {
		t.Fatal("repeated passes made the summary fall back")
	}
	if lost := lostFact(text, s); lost != "" {
		t.Fatalf("summary of repeated passes %s\nsummary: %q", lost, s)
	}
}
