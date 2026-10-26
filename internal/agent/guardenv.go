package agent

import (
	"strings"

	"github.com/cristiangirlea/harness/internal/tools"
)

// Claude's " *" also matches no arguments; "*" matches suffixes.
var guardCommands = []string{
	"git push", "git push *",
	// Match key variants because deny rules are case-sensitive and git keys are not.
	"git config *push*", "git config *Push*", "git -c *push*", "git -c *Push*", "git remote set-url --push *",
	"unset GIT_CONFIG*", "unset GH_CONFIG_DIR*", "unset GH_TOKEN*", "unset HRN_GUARDED*", "env -u *",
	// The system credential store survives environment filtering.
	"gh auth token", "gh auth token *", "gh auth git-credential *", "gh auth status *--show-token*", "gh auth status *-t*",
	"gh pr merge *", "gh release create *", "gh release upload *", "gh release edit *", "gh release delete *",
	"gh repo delete *", "gh repo edit *", "gh repo archive *", "gh repo rename *",
	"gh api *-X *", "gh api *--method*",
	"wrangler *deploy *", "npx wrangler *deploy *", "wrangler *publish *", "npx wrangler *publish *",
	"npm publish *", "pnpm publish *", "yarn publish *", "bun publish *", "cargo publish *",
	"npm run deploy*", "pnpm run deploy*", "yarn deploy*", "yarn run deploy*", "bun run deploy*",
	"npm * deploy*", "pnpm * deploy*", "yarn * deploy*", "bun * deploy*",
	"docker push *",
	"kubectl apply *", "kubectl delete *", "kubectl replace *", "kubectl patch *", "kubectl rollout *",
	"terraform apply *", "terraform destroy *", "tofu apply *", "tofu destroy *",
	"helm install *", "helm upgrade *", "helm uninstall *",
	"vercel *--prod*", "fly deploy *", "flyctl deploy *", "netlify deploy *",
	"bash deploy/push.sh *", "./deploy/push.sh *",
}

// Command rules give early refusals but allow shell indirection; the environment guard remains necessary.
// Read/Edit rules also protect secret files that environment filtering cannot hide.
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
	for _, p := range tools.SecretPaths {
		if strings.HasSuffix(p, "/") {
			p += "**"
		}
		paths = append(paths, "//**/"+p)
	}
	for _, p := range paths {
		rules = append(rules, "Read("+p+")", "Edit("+p+")")
	}
	return rules
}
