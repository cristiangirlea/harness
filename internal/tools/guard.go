package tools

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// A scriptblock built from a download, by either type name and with $(...) too.
const scriptblockOfDownload = `\[(?i:(?:(?:System\.)?Management\.Automation\.)?scriptblock)\]\s*::\s*(?i:Create)\s*\(\s*(?:\$?\(\s*)*(?:` +
	`(?i:curl|wget|irm|iwr|Invoke-RestMethod|Invoke-WebRequest)\s+` +
	`|(?i:New-Object)\s+(?i:(?:System\.)?Net\.WebClient)\s*\)\s*\.\s*(?i:DownloadString)\s*\()`

// Textual rules stop accidents; the permission mode stops hostile commands (docs/permissions.md).
var guardRules = []struct {
	name  string
	match func(command string) bool
}{
	// pushurl/pushInsteadOf can bypass guarded push rewrites.
	{"git-push", matches(`(?i)\bgit\b[^|&;]*\bpush(?:url|insteadof)?\b`)},
	{"guard-env", matches(guardEnvPattern())},
	// Privilege prompts hang without a terminal.
	{"sudo", matches(`\b(?i:sudo)\b`)},
	{"rm-root", rmHitsRoot},
	// Catch indirect pipes and PowerShell evaluation too.
	{"pipe-to-shell", matches(
		`\b(?i:curl|wget|irm|iwr|Invoke-RestMethod|Invoke-WebRequest)\b.*\|\s*(?:[&.]\s*)?(?:(?i:sudo)\s+)?(?:"[^"|]*[\\/]|'[^'|]*[\\/]|[^\s"'|]*[\\/])?["']?(?i:sh|bash|zsh|powershell|pwsh|iex|Invoke-Expression)\b` +
			`|(?:^|[^\w])(?:&|\.)\s*\(\s*` + scriptblockOfDownload +
			`|` + scriptblockOfDownload + `.*\)\s*\.\s*(?i:Invoke(?:ReturnAsIs)?)\s*\(` +
			`|(?:^|[^\w-])(?i:iex|Invoke-Expression)\s*(?:-(?i:c\w*)\s+)?\$?\(\s*(?:\(\s*)*(?:` +
			`(?i:curl|wget|irm|iwr|Invoke-RestMethod|Invoke-WebRequest)\s+` +
			`|(?i:New-Object)\s+(?i:(?:System\.)?Net\.WebClient)\s*\)\s*\.\s*(?i:DownloadString)\s*\()`,
	)},
	// Accept unlikely Unix case collisions.
	{"disk", matches(`\b(?i:mkfs)|\b(?i:format)\s+[A-Za-z]:|\b(?i:dd)\b[^|&;]*\bof=\s*(?:/dev/|\\\\\.\\)`)},
	// Block GET: fields imply POST; logins guard pr create.
	{"gh-publish", matches(
		`\b(?i:gh)\b[^|&;]*\b(?:pr\s+merge|release\s+(?:create|upload|edit|delete)|repo\s+(?:delete|archive|rename|edit\b[^|&;]*--visibility))\b` +
			`|\b(?i:gh(?:\.exe)?)["']?\s+api\b[^|&;]*(?:-X|--method)[\s="'\\$]*(?i:PUT|PATCH|DELETE)\b` +
			`|\b(?i:gh(?:\.exe)?)["']?\s+api\b[^|&;]*(?:/(?:releases|git/(?:refs|tags)|rulesets|keys|hooks|collaborators|dispatches|merges|environments|transfer|deployments|statuses|branches/[^/\s"'?]+/rename|merge-upstream|forks|pages|actions/(?:secrets|variables|runs/[^/\s"'?]+/pending_deployments|runners/(?:registration-token|generate-jitconfig|remove-token))|user/repos|orgs/[^/\s"'?]+/repos|gists))(?:/|[\s"'?]|$)` +
			`|\b(?i:gh(?:\.exe)?)["']?\s+api(?:\s+-[-\w]+(?:[\s=]+(?:[^\s"'-]|"[^"]*"|'[^']*')(?:[^\s"']|"[^"]*"|'[^']*')*)?)*\s+["']?(?:user/repos|orgs/[^/\s"'?]+/repos|gists)(?:/|[\s"'?]|$)` +
			`|\b(?i:gh(?:\.exe)?)["']?\s+api\b[^|&;]*(?:\bgraphql\b[^|&;]*(?i:mutation)\b|(?i:mutation)\b[^|&;]*\bgraphql\b)`,
	)},
	// Redirects cannot hide system credentials.
	{"gh-token", matches(`(?i)\bgh\b[^|&;]*\bauth\s+(?:token|git-credential)\b|\bgh\b[^|&;]*\bauth\s+status\b[^|&;]*(?:--show-token\b|\s-[a-z]*t\b)`)},
	// Shells must not bypass secret-file refusals.
	{"secret-file", namesSecretFile},
	// Project scripts can deploy too.
	{"deploy", matches(`\bwrangler\b[^|&;]*\b(?:deploy|publish)\b|\b(?:npm|pnpm|yarn|bun|cargo)\s+publish\b|\b(?:npm|pnpm|yarn|bun)\b[^|&;]*\b(?:run(?:-script)?\s+)?deploy\b|\bdeploy/push\.sh\b|\bkubectl\b[^|&;]*\b(?:apply|delete|replace|patch|rollout)\b|\b(?:terraform|tofu)\b[^|&;]*\b(?:apply|destroy)\b|\bhelm\b[^|&;]*\b(?:install|upgrade|uninstall|delete)\b|\bdocker\s+push\b|\bvercel\b[^|&;]*--prod\b|\b(?:fly|flyctl|netlify)\s+deploy\b`)},
}

func matches(pattern string) func(string) bool {
	re := regexp.MustCompile(pattern)
	return re.MatchString
}

// Guard returns a rule or "", accepting false positives.
func Guard(command string) string {
	quoted := guardQuotedSeparators(command)
	// Both forms prevent quotes hiding real pipes.
	for _, r := range guardRules {
		if r.match(command) || r.match(quoted) {
			return r.name
		}
	}
	return ""
}

func guardQuotedSeparators(command string) string {
	line := []byte(command)
	var quote byte
	escaped := false
	for i, c := range line {
		if quote == '"' && escaped {
			escaped = false
			continue
		}
		if quote == '"' && c == '\\' {
			escaped = true
			continue
		}
		if quote != 0 && c == quote {
			quote = 0
			continue
		}
		if quote == 0 && (c == '\'' || c == '"') {
			quote = c
			continue
		}
		if quote != 0 && (c == '|' || c == '&' || c == ';') {
			line[i] = ' '
		}
	}
	return string(line)
}

// GuardError names no way to lift the guard: the model may reach hrn through an MCP server.
func GuardError(rule string) error {
	return fmt.Errorf("blocked by the hrn guard (%s): only the operator can allow this, "+
		"by starting the run with --allow-dangerous. "+
		"Do not retry it; say in your answer why it is needed", rule)
}

// GIT_CONFIG_PARAMETERS carries git -c, which outranks the numbered entries; (?i) is for PowerShell.
func guardEnvPattern() string {
	// Include loop-generated KEY_$i overrides.
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
		`(?:^|[^\w-])` + n + `\}?\s*\+?=`,
		// Flags are not variables.
		`(?:^|[^\w-])(?:unset|export\s+-n|declare\s+\+x|typeset\s+\+x|read|printf\s+-v)\b[^|&;]*[^\w-]` + n + `\b`,
		`\benv\b[^|&;]*(?:-u\s*|--unset[=\s]\s*)` + n + `\b`,
		`\benv\s+(?:-i|--ignore-environment|-)(?:\s|$)`,
		// Any cmdlet can mutate env:; $env: reads are safe.
		`(?:^|[^${\w])env:[\\/]?(?:` + n + `\b|\w*[*?])`,
		`\benvironment::[\\/]?(?:` + n + `\b|\w*[*?])`, // the same drive by its provider's name
		`setenvironmentvariable\(\s*['"]` + n + `['"]`,
	}, "|")
}

var rmCallRE = regexp.MustCompile(`\brm\b([^|&;]*)`)

// rm's root protection misses /* and $HOME.
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

// Git Bash drive roots use /c.
var driveRoot = regexp.MustCompile(`^(?:[A-Za-z]:|/[A-Za-z])$`)

func isRootTarget(arg string) bool {
	// "$HOME"/ is one token despite partial quoting.
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
