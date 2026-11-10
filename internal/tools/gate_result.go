package tools

import (
	"regexp"
	"strconv"
	"strings"
)

// The starts of the refusals and failures runGated and the gate's tool server write.
const (
	gateRefusedBy    = "refused by the " + GateName + " gate: "
	gateDidNotRun    = "the " + GateName + " gate did not run the command"
	gateCouldNot     = "the " + GateName + " gate could not start"
	hrnRefused       = "refused by hrn: "
	gatePolicyMoved  = "refused: the run's gate policy file was changed or removed"
	commandTimedOut  = "command timed out"
	originalReadable = "; read_original reads it"
)

var (
	exitLine     = regexp.MustCompile(`^\[exit code (-?\d+)\]$`)
	timeoutLine  = regexp.MustCompile(`^\[timed out after ([0-9a-zµ.]+)\]$`)
	originalLine = regexp.MustCompile(`^original: ([0-9a-f]{16}) \((\d+) bytes\)` + regexp.QuoteMeta(originalReadable) + `$`)
	refusedRule  = regexp.MustCompile(`refused by the policy: ([a-z0-9][a-z0-9-]*)(?:\.| ask|$)`)
	guardRuleOf  = regexp.MustCompile(`^` + regexp.QuoteMeta(GuardRefusal) + ` \(([^)\n]+)\)`)
	notRunExit   = regexp.MustCompile(`^` + regexp.QuoteMeta(gateDidNotRun) + ` \(exit (\d+)\)`)
)

// GateResult is what a gated command's recorded result says the gate and hrn did with it.
type GateResult struct {
	By, Decision, Rule, Exit string
	// Original is a shaped result's handle, which only the run's originals directory can confirm.
	Original      string
	OriginalBytes int
}

// ReadGateResult reads a run_command result as the api loop or the gate's tool server recorded it.
// A command's own output cannot fake the lines hrn appends after it, so those are read from the end.
func ReadGateResult(text string) (GateResult, bool) {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	n := len(lines)
	last := lines[n-1]
	tail := -1
	r := GateResult{By: "gate", Decision: "allowed"}
	switch {
	case exitLine.MatchString(last):
		tail = n - 1
	case n >= 2 && "["+last+"]" == lines[n-2] && exitLine.MatchString(lines[n-2]):
		tail = n - 2
	case n >= 2 && last == commandTimedOut && timeoutLine.MatchString(lines[n-2]):
		tail = n - 2
	}
	if tail >= 0 {
		if m := exitLine.FindStringSubmatch(lines[tail]); m != nil {
			r.Exit = "exit " + m[1]
		} else {
			r.Exit = "timed out after " + timeoutLine.FindStringSubmatch(lines[tail])[1]
		}
		if m := originalLine.FindStringSubmatch(line(lines, tail-1)); m != nil {
			r.Original = m[1]
			r.OriginalBytes, _ = strconv.Atoi(m[2])
		}
		return r, true
	}
	first := lines[0]
	switch {
	case strings.HasPrefix(first, gateRefusedBy):
		r.Decision = "refused"
		if m := refusedRule.FindStringSubmatch(first); m != nil {
			r.Rule = m[1]
		}
	case guardRuleOf.MatchString(first):
		r.By, r.Decision, r.Rule = "hrn guard", "refused", guardRuleOf.FindStringSubmatch(first)[1]
	case strings.HasPrefix(first, hrnRefused), strings.HasPrefix(first, gatePolicyMoved):
		r.By, r.Decision = "hrn", "refused"
	case notRunExit.MatchString(first):
		r.Decision, r.Exit = "not run", "exit "+notRunExit.FindStringSubmatch(first)[1]
	case strings.HasPrefix(first, gateCouldNot):
		r.Decision = "not run"
	default:
		return GateResult{}, false
	}
	return r, true
}

func line(lines []string, i int) string {
	if i < 0 {
		return ""
	}
	return lines[i]
}
