package tools

import (
	"fmt"
	"regexp"
)

// The command guard refuses a few command lines that a coding task never means
// to ask for: they publish, need a password, or destroy something no gate can
// undo. It is a guard rail, not a sandbox: matching is textual and an agent
// that wants to get around a pattern can. What stops a hostile command is the
// permission mode (commands run only in yolo, or after an approval); what this
// stops is the plausible accident.
var guardRules = []struct {
	name string
	re   *regexp.Regexp
}{
	// Publishing is the operator's call: a batch commits on its own branch and
	// the operator decides what leaves the machine.
	{"git-push", regexp.MustCompile(`\bgit\s+push\b`)},
	// A password prompt hangs a run that has no terminal, and anything worth
	// running as root is worth a human typing it.
	{"sudo", regexp.MustCompile(`\bsudo\b`)},
	// rm -r against a root, a home or a whole drive.
	{"rm-root", regexp.MustCompile(`\brm\s+-[A-Za-z]*r[A-Za-z]*\s+(/|~|\$HOME|[A-Za-z]:\\)(\s|$)`)},
	// Downloading a script straight into a shell.
	{"pipe-to-shell", regexp.MustCompile(`\b(curl|wget)\b[^|]*\|\s*(sh|bash|zsh|powershell)\b`)},
	// Formatting or overwriting a device.
	{"disk", regexp.MustCompile(`\b(mkfs|format\s+[A-Za-z]:|dd\s+if=)`)},
}

// Guard names the rule that refuses a command line, or "" when it is allowed.
// The match is textual, so a line that merely mentions a pattern (echo "git
// push") is refused as well; that false positive costs the agent one turn,
// which is cheap next to what the rules prevent.
func Guard(command string) string {
	for _, r := range guardRules {
		if r.re.MatchString(command) {
			return r.name
		}
	}
	return ""
}

// GuardError is what a refused command returns to the model. It names the rule
// and the setting that lifts it, and says not to retry, because only the
// operator can change that setting.
func GuardError(rule string) error {
	return fmt.Errorf("blocked by the hx command guard (%s): only the operator can allow this, "+
		"with --allow-dangerous (allow_dangerous in a batch spec or an MCP call). "+
		"Do not retry the command; say in your answer why it is needed", rule)
}
