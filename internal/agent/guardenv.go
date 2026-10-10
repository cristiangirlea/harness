package agent

// claudeDenyRules is the guard as Claude Code's own deny rules, which its
// documentation says hold in every permission mode, bypassPermissions too.
// They match command text, so `git -C dir push` or `bash -c "git push"` get
// past them; the guarded environment is what stops those. What they add is
// an early, plain refusal, and the secret files, which an environment cannot
// hide: a Read rule also covers Edit and Write on that path, and cat or head
// in Bash.
func claudeDenyRules() []string {
	commands := []string{
		"git push *",
		"gh pr merge *", "gh release create *", "gh release upload *", "gh release edit *", "gh release delete *",
		"gh repo delete *", "gh repo edit *", "gh repo archive *", "gh repo rename *",
		"wrangler deploy *", "npx wrangler deploy *", "wrangler pages deploy *", "npx wrangler pages deploy *",
		"npm publish *", "pnpm publish *", "yarn publish *", "cargo publish *",
		"docker push *", "kubectl apply *", "kubectl delete *",
		"bash deploy/push.sh *", "./deploy/push.sh *",
	}
	var rules []string
	for _, shell := range []string{"Bash", "PowerShell"} {
		for _, c := range commands {
			rules = append(rules, shell+"("+c+")")
		}
	}
	for _, path := range []string{
		"//**/.env", "//**/.env.local", "//**/.env.*.local", "//**/.env.production",
		"//**/*.pem", "//**/.git-credentials", "//**/.private/**",
		"~/.ssh/**", "~/.aws/**", "~/.config/gh/**", "~/.npmrc", "~/.wrangler/**",
	} {
		rules = append(rules, "Read("+path+")")
	}
	return rules
}
