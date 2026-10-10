package tools

import (
	"fmt"
	"regexp"
	"strings"
)

// The command guard refuses a few command lines that a coding task never means
// to ask for: they publish, need a password, or destroy something no gate can
// undo. It is a guard rail, not a sandbox: matching is textual and an agent
// that wants to get around a pattern can. What stops a hostile command is the
// permission mode (commands run only in yolo, or after an approval); what this
// stops is the plausible accident.
//
// The rules lean towards refusing too much. A command segment ends at |, &&,
// || or ;, so a rule reads one command at a time rather than the whole line.
var guardRules = []struct {
	name  string
	match func(command string) bool
}{
	// Publishing is the operator's call: a batch commits on its own branch and
	// the operator decides what leaves the machine. Any git segment that
	// mentions push counts, so `git -C dir push` is caught along with the
	// false positive `git commit -m "push later"`.
	{"git-push", matches(`\bgit\b[^|&;]*\bpush\b`)},
	// A password prompt hangs a run that has no terminal, and anything worth
	// running as root is worth a human typing it.
	{"sudo", matches(`\bsudo\b`)},
	// rm -r against a root rather than something inside one.
	{"rm-root", rmHitsRoot},
	// Downloading a script straight into a shell, however many stages the pipe
	// has on the way.
	{"pipe-to-shell", matches(`\b(?:curl|wget)\b.*\|\s*(?:sudo\s+)?(?:sh|bash|zsh|powershell|pwsh)\b`)},
	// Formatting a filesystem, or writing over a device rather than a file.
	{"disk", matches(`\bmkfs|\bformat\s+[A-Za-z]:|\bdd\b[^|&;]*\bof=\s*(?:/dev/|\\\\\.\\)`)},
	// GitHub's side of publishing: merging, releasing, changing who can see a
	// repository or removing it, and the API calls that change things.
	// gh pr create is left to the guarded environment, where gh has no login:
	// handing work over is hrn's (a branch per batch task) or the operator's.
	{"gh-publish", matches(`\bgh\b[^|&;]*\b(?:pr\s+merge|release\s+(?:create|upload|edit|delete)|repo\s+(?:delete|archive|rename|edit\b[^|&;]*--visibility))\b|\bgh\s+api\b[^|&;]*(?:-X|--method)[\s=]*(?:PUT|PATCH|DELETE)\b`)},
	// Shipping somewhere: Cloudflare, package registries, image registries,
	// clusters, infrastructure and hosting platforms, a project's own deploy
	// script (npm run deploy, the token itself, not deploy-docs-preview), and
	// this owner's deploy script.
	{"deploy", matches(`\bwrangler\b[^|&;]*\bdeploy\b|\b(?:npm|pnpm|yarn|cargo)\s+publish\b|\b(?:npm|pnpm|yarn)\s+(?:run\s+)?deploy(?:$|[\s;&|])|\bdeploy/push\.sh\b|\bkubectl\b[^|&;]*\b(?:apply|delete|replace|patch|rollout)\b|\b(?:terraform|tofu)\b[^|&;]*\b(?:apply|destroy)\b|\bhelm\b[^|&;]*\b(?:install|upgrade|uninstall|delete)\b|\bdocker\s+push\b|\bvercel\b[^|&;]*--prod\b|\b(?:fly|flyctl|netlify)\s+deploy\b`)},
}

func matches(pattern string) func(string) bool {
	re := regexp.MustCompile(pattern)
	return re.MatchString
}

// Guard names the rule that refuses a command line, or "" when it is allowed.
// The match is textual, so a line that merely mentions a pattern (echo "git
// push") is refused as well; that false positive costs the agent one turn,
// which is cheap next to what the rules prevent.
func Guard(command string) string {
	for _, r := range guardRules {
		if r.match(command) {
			return r.name
		}
	}
	return ""
}

// GuardError is what a refused command returns to the model. It names the rule
// and the setting that lifts it, and says not to retry, because only the
// operator can change that setting.
func GuardError(rule string) error {
	return fmt.Errorf("blocked by the hrn command guard (%s): only the operator can allow this, "+
		"with --allow-dangerous (allow_dangerous in a batch spec or an MCP call). "+
		"Do not retry the command; say in your answer why it is needed", rule)
}

// rmCallRE isolates what follows an rm up to the end of its command segment.
var rmCallRE = regexp.MustCompile(`\brm\b([^|&;]*)`)

// rmHitsRoot reports whether the line deletes a root recursively. Reading the
// arguments beats one long pattern here, because the spelling that matters is
// not the famous one: `rm -rf /` is refused by rm itself without
// --no-preserve-root, while `rm -rf /*`, `rm -rf "~/"` and `rm -rf $HOME` are
// not, and they are what actually empties a machine.
func rmHitsRoot(command string) bool {
	for _, call := range rmCallRE.FindAllStringSubmatch(command, -1) {
		recursive, root := false, false
		for _, tok := range strings.Fields(call[1]) {
			switch {
			case strings.HasPrefix(tok, "--"):
				recursive = recursive || tok == "--recursive"
			case strings.HasPrefix(tok, "-"):
				recursive = recursive || strings.ContainsAny(tok, "rR")
			default:
				root = root || isRootTarget(tok)
			}
		}
		if recursive && root {
			return true
		}
	}
	return false
}

// driveRoot matches a whole drive: C: on Windows, /c as Git Bash spells it.
var driveRoot = regexp.MustCompile(`^(?:[A-Za-z]:|/[A-Za-z])$`)

// isRootTarget reports whether an rm argument names a root rather than
// something inside one, ignoring quotes, a trailing glob and the slash style.
func isRootTarget(arg string) bool {
	// Quotes can sit anywhere in a shell token: "$HOME"/ is one path.
	arg = strings.NewReplacer(`"`, "", "'", "").Replace(arg)
	arg = strings.TrimSuffix(arg, "*")
	arg = strings.ReplaceAll(arg, `\`, "/")
	if arg != "/" {
		arg = strings.TrimSuffix(arg, "/")
	}
	switch arg {
	case "/", "~", "$HOME", "${HOME}", "%USERPROFILE%":
		return true
	}
	return driveRoot.MatchString(arg)
}
