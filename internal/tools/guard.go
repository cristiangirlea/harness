package tools

import (
	"fmt"
	"regexp"
	"sort"
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
	// false positive `git commit -m "push later"`, and so is setting a pushurl
	// or a pushInsteadOf, which would steer a push around the guarded
	// environment's dead end.
	{"git-push", matches(`(?i)\bgit\b[^|&;]*\bpush(?:url|insteadof)?\b`)},
	// The guarded environment itself: a command that unsets or overrides
	// git's push rewrite or points a tool back at the operator's login.
	// Reading a variable or naming it in a search is fine.
	{"guard-env", matches(guardEnvPattern())},
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
	// gh's login in the system's credential store, which no variable hides:
	// printing it, or handing it to git as a credential helper.
	{"gh-token", matches(`(?i)\bgh\b[^|&;]*\bauth\s+(?:token|git-credential)\b|\bgh\b[^|&;]*\bauth\s+status\b[^|&;]*(?:--show-token\b|\s-[a-z]*t\b)`)},
	// The secret files the file tools refuse, reached through the shell
	// instead: cat .env, echo x > .env, cp .env.example .env.
	{"secret-file", namesSecretFile},
	// Shipping somewhere: Cloudflare, package registries, image registries,
	// clusters, infrastructure and hosting platforms, a project's own deploy
	// scripts (npm run deploy, deploy:prod, deploy-docs and the like), and
	// this owner's deploy script.
	{"deploy", matches(`\bwrangler\b[^|&;]*\bdeploy\b|\b(?:npm|pnpm|yarn|bun|cargo)\s+publish\b|\b(?:npm|pnpm|yarn|bun)\s+(?:run\s+)?deploy\b|\bdeploy/push\.sh\b|\bkubectl\b[^|&;]*\b(?:apply|delete|replace|patch|rollout)\b|\b(?:terraform|tofu)\b[^|&;]*\b(?:apply|destroy)\b|\bhelm\b[^|&;]*\b(?:install|upgrade|uninstall|delete)\b|\bdocker\s+push\b|\bvercel\b[^|&;]*--prod\b|\b(?:fly|flyctl|netlify)\s+deploy\b`)},
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
// and says only the operator can lift it, and not to retry. It names no way
// for the model to lift it itself: an hrn MCP server can be one of its tools.
func GuardError(rule string) error {
	return fmt.Errorf("blocked by the hrn guard (%s): only the operator can allow this, "+
		"by starting the run with --allow-dangerous. "+
		"Do not retry it; say in your answer why it is needed", rule)
}

// guardEnvPattern matches a command that changes a variable the guarded
// environment sets: git's numbered config entries (and the -c channel, which
// outranks them), the ssh agent, and the variables that point tools at an
// empty login. Changing is setting one (NAME=, export, $env:NAME =), unsetting
// one (unset, env -u, Remove-Item env:, SetEnvironmentVariable) or starting
// with an empty environment (env -i). Case is ignored, for PowerShell. A
// flag of the same name (--kubeconfig=k.yaml) is not a variable.
func guardEnvPattern() string {
	// KEY_$i: a loop that writes the numbered entries.
	names := []string{`GIT_CONFIG_(?:COUNT|KEY_(?:\d+|\$\{?\w+\}?)|VALUE_(?:\d+|\$\{?\w+\}?)|PARAMETERS)`, "SSH_AUTH_SOCK"}
	for name := range redirects("") {
		if name != "SSH_AUTH_SOCK" {
			names = append(names, regexp.QuoteMeta(name))
		}
	}
	for name := range fixedEnv() {
		names = append(names, regexp.QuoteMeta(name))
	}
	sort.Strings(names[2:])
	n := `(?:` + strings.Join(names, "|") + `)`
	return `(?i)` + strings.Join([]string{
		// NAME=, NAME+=, export, declare, $env:NAME = and ${env:NAME} =.
		`(?:^|[^\w-])` + n + `\}?\s*\+?=`,
		// unset, or taken off what child processes see, or set by a builtin;
		// not a flag that only shares the word (--read-only, --kubeconfig).
		`(?:^|[^\w-])(?:unset|export\s+-n|declare\s+\+x|typeset\s+\+x|read|printf\s+-v)\b[^|&;]*[^\w-]` + n + `\b`,
		`\benv\b[^|&;]*(?:-u\s*|--unset[=\s]\s*)` + n + `\b`,
		`\benv\s+(?:-i|--ignore-environment|-)(?:\s|$)`,
		// PowerShell's env: drive used as a path, whatever cmdlet, alias or
		// scriptblock it is in, and any wildcard on it. $env:NAME and
		// ${env:NAME} are the variable syntax: read freely, caught above when set.
		`(?:^|[^${\w])env:[\\/]?(?:` + n + `\b|\w*[*?])`,
		`\benvironment::[\\/]?(?:` + n + `\b|\w*[*?])`, // the same drive by its provider's name
		`setenvironmentvariable\(\s*['"]` + n + `['"]`,
	}, "|")
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
