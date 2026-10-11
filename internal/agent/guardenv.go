package agent

import "github.com/cristiangirlea/harness/internal/tools"

// guardCommands are the command lines claude's deny rules refuse, in both of
// its shells. Claude Code reads " *" as "followed by arguments, or nothing"
// and a bare "*" as any text, so "npm run deploy*" takes deploy:prod as well.
var guardCommands = []string{
	"git push", "git push *",
	// Steering a push around the guarded environment's dead end.
	"git config *push*", "git -c *push*", "git remote set-url --push *",
	"unset GIT_CONFIG*", "unset GH_CONFIG_DIR*", "env -u *",
	"gh pr merge *", "gh release create *", "gh release upload *", "gh release edit *", "gh release delete *",
	"gh repo delete *", "gh repo edit *", "gh repo archive *", "gh repo rename *",
	"gh api *-X *", "gh api *--method*",
	"wrangler deploy *", "npx wrangler deploy *", "wrangler pages deploy *", "npx wrangler pages deploy *",
	"npm publish *", "pnpm publish *", "yarn publish *", "bun publish *", "cargo publish *",
	"npm run deploy*", "pnpm run deploy*", "yarn deploy*", "yarn run deploy*", "bun run deploy*",
	"docker push *",
	"kubectl apply *", "kubectl delete *", "kubectl replace *", "kubectl patch *", "kubectl rollout *",
	"terraform apply *", "terraform destroy *", "tofu apply *", "tofu destroy *",
	"helm install *", "helm upgrade *", "helm uninstall *",
	"vercel *--prod*", "fly deploy *", "flyctl deploy *", "netlify deploy *",
	"bash deploy/push.sh *", "./deploy/push.sh *",
}

// guardHomeFiles are logins kept in the operator's home, besides the secret
// files and directories tools lists for any place: gh's (on Windows under
// %APPDATA%\GitHub CLI), npm's, docker's, gcloud's, cargo's, and the plan
// logins of the agent CLIs themselves, which the CLIs read with their own
// code rather than through these tools.
var guardHomeFiles = []string{
	"~/.config/gh/**", "//**/GitHub CLI/**", "~/.npmrc", "~/.docker/config.json", "~/.config/gcloud/**",
	"~/.cargo/credentials*", "~/.claude/.credentials.json", "~/.codex/auth.json", "~/.gemini/oauth_creds.json",
}

// claudeDenyRules is the guard as Claude Code's own deny rules, which its
// documentation says hold in every permission mode, bypassPermissions too.
// The command rules match command text, so `git -C dir push` or `bash -c
// "git push"` get past them; the guarded environment is what stops those.
// What they add is an early, plain refusal, and the secret files, which an
// environment cannot hide: a Read rule covers the tools that read a path
// (cat and head in Bash among them), an Edit rule the ones that write it,
// a new file included.
func claudeDenyRules() []string {
	var rules []string
	for _, shell := range []string{"Bash", "PowerShell"} {
		for _, c := range guardCommands {
			rules = append(rules, shell+"("+c+")")
		}
	}
	var paths []string
	for _, name := range tools.SecretNames {
		paths = append(paths, "//**/"+name)
	}
	for _, dir := range tools.SecretDirs {
		paths = append(paths, "//**/"+dir+"/**")
	}
	paths = append(paths, guardHomeFiles...)
	for _, p := range paths {
		rules = append(rules, "Read("+p+")", "Edit("+p+")")
	}
	return rules
}
