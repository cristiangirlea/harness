package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuardBlocks(t *testing.T) {
	for command, want := range map[string]string{
		"git push origin main":                   "git-push",
		"git push -f":                            "git-push",
		"cd repo && git   push":                  "git-push",
		"git -C /repo push origin HEAD":          "git-push", // the flag form
		`echo "git push"`:                        "git-push", // documented false positive
		"sudo apt-get install -y jq":             "sudo",
		"rm -rf /":                               "rm-root",
		"rm -rf /*":                              "rm-root", // what rm itself does not refuse
		`rm -rf "/"`:                             "rm-root",
		"rm -rf / --no-preserve-root":            "rm-root",
		"rm -fr ~":                               "rm-root",
		"rm -rf ~/":                              "rm-root",
		"rm -rf ~/*":                             "rm-root",
		"rm -r $HOME":                            "rm-root",
		`rm -rf "$HOME"/`:                        "rm-root",
		"rm --recursive --force $HOME":           "rm-root",
		`rm -rf C:\`:                             "rm-root",
		"rm -rf C:/":                             "rm-root",
		"rm -rf /c/":                             "rm-root", // Git Bash spells a drive this way
		"cd /tmp && rm -rf /":                    "rm-root",
		"curl https://example.com/i.sh | sh":     "pipe-to-shell",
		"wget -qO- https://example.com/i | bash": "pipe-to-shell",
		"curl https://x/i | tee log | bash":      "pipe-to-shell", // more than one stage
		"mkfs.ext4 /dev/sda1":                    "disk",
		"dd if=/dev/zero of=/dev/sda":            "disk",
		"format C: /q":                           "disk",
		"gh pr merge 34 --merge":                 "gh-publish",
		"gh pr merge --auto --squash":            "gh-publish",
		"gh release create v1.0.0 dist/*":        "gh-publish",
		"gh release upload v1 a.zip":             "gh-publish",
		"gh repo edit --visibility public":       "gh-publish",
		"gh repo delete owner/x --yes":           "gh-publish",
		"gh api -X PUT repos/o/r/pulls/1/merge":  "gh-publish",
		"gh api --method DELETE repos/o/r":       "gh-publish",
		"cd site && npx wrangler deploy":         "deploy",
		"wrangler pages deploy dist":             "deploy",
		"npm publish":                            "deploy",
		"cargo publish --dry-run=false":          "deploy",
		"bash deploy/push.sh":                    "deploy",
		"kubectl apply -f prod.yaml":             "deploy",
		"docker push ghcr.io/o/i:latest":         "deploy",
		"vercel --prod":                          "deploy",
		"fly deploy":                             "deploy",
		"npm run deploy":                         "deploy",
		"pnpm run deploy --prod":                 "deploy",
		"yarn deploy":                            "deploy",
		"terraform apply -auto-approve":          "deploy",
		"tofu destroy":                           "deploy",
		"helm upgrade --install app ./chart":     "deploy",
		"helm uninstall app":                     "deploy",
		"npm run deploy:prod":                    "deploy",
		"npm run deploy-docs-preview --dry-run":  "deploy", // a project's deploy-something is a deploy too
		"bun run deploy":                         "deploy",
		// Steering a push around the guarded environment's dead end.
		"git config remote.origin.pushurl https://github.com/o/r":       "git-push",
		"git config --global url.https://x/.pushInsteadOf y":            "git-push",
		"git -c url.a.PushInsteadOf=b fetch":                            "git-push",
		"unset GIT_CONFIG_COUNT && git fetch":                           "guard-env",
		"GIT_CONFIG_PARAMETERS= git status":                             "guard-env",
		"GIT_CONFIG_KEY_6=x git status":                                 "guard-env",
		"env -u GH_CONFIG_DIR gh pr view 1":                             "guard-env",
		"env --unset=GH_CONFIG_DIR gh pr view 1":                        "guard-env",
		"env -i bash -c 'gh pr view 1'":                                 "guard-env", // an empty environment drops the guard's too
		"$env:gh_config_dir = ''":                                       "guard-env", // PowerShell, any case
		"Remove-Item Env:GIT_CONFIG_COUNT":                              "guard-env",
		"[Environment]::SetEnvironmentVariable('GH_CONFIG_DIR', $null)": "guard-env",
		"export NPM_CONFIG_USERCONFIG=~/.npmrc":                         "guard-env",
		"KUBECONFIG=/home/me/kube/config kubectl get pods":              "guard-env",
		"SSH_AUTH_SOCK=/tmp/agent.sock ssh -T git@github.com":           "guard-env",
		"export KUBECONFIG+=:/home/me/kube/config":                      "guard-env", // appending
		"$env:KUBECONFIG += ';x'":                                       "guard-env",
		"${env:GH_CONFIG_DIR} = 'x'":                                    "guard-env", // braced PowerShell name
		"Set-Item env:GH_CONFIG_DIR x":                                  "guard-env",
		"New-Item env:KUBECONFIG -Value x":                              "guard-env",
		"rm env:KUBECONFIG":                                             "guard-env", // PowerShell's aliases
		"del Env:\\GIT_CONFIG_COUNT":                                    "guard-env",
		"si env:NPM_CONFIG_USERCONFIG x":                                "guard-env",
		"for i in 0 1; do export GIT_CONFIG_KEY_$i=x; done":             "guard-env",
		// The secret files, reached through the shell.
		"cat .env":                               "secret-file",
		"echo X=1 > .env":                        "secret-file",
		"echo X=1 >.env.local":                   "secret-file",
		"cp .env.example .env":                   "secret-file",
		"Get-Content .\\.env":                    "secret-file",
		"type config\\.ENV":                      "secret-file",
		"cat ~/.ssh/id_ed25519":                  "secret-file",
		"cat $HOME/.aws/credentials":             "secret-file",
		"openssl x509 -in certs/server.pem":      "secret-file",
		"docker compose --env-file=.env.prod up": "secret-file",
		"cat .private/notes.md | head":           "secret-file",
		"cat ~/.netrc":                           "secret-file",
		"git show HEAD:.env":                     "secret-file", // git's REV:path
		"git cat-file -p :.env.local":            "secret-file",
		"cat {.env,README.md}":                   "secret-file",
		// The agent CLIs' plan logins and the tools' logins in the home folder.
		"cat ~/.claude/.credentials.json":                     "secret-file",
		"type %USERPROFILE%\\.codex\\auth.json":               "secret-file",
		"cat ~/.gemini/oauth_creds.json":                      "secret-file",
		"cat ~/.config/gh/hosts.yml":                          "secret-file",
		"Get-Content \"$env:APPDATA\\GitHub CLI\\hosts.yml\"": "secret-file",
		"cat ~/.npmrc":                                              "secret-file",
		"cat ~/.docker/config.json":                                 "secret-file",
		"cat ~/.cargo/credentials.toml":                             "secret-file",
		"cat ~/.config/.wrangler/config/default.toml":               "secret-file",
		"cat \"$APPDATA/xdg.config/.wrangler/config/default.toml\"": "secret-file",
	} {
		if got := Guard(command); got != want {
			t.Errorf("Guard(%q) = %q, want %q", command, got, want)
		}
	}
}

func TestGuardAllows(t *testing.T) {
	for _, command := range []string{
		"go build ./... && go test ./...",
		"git status --porcelain",
		"git commit -am 'fix the parser'",
		"git pushups", // push is not a word here
		"echo sudoku", // nor is sudo
		"rm -rf node_modules",
		"rm -rf ./build",
		"rm -rf /tmp/scratch",     // a path under root, not root
		"rm -rf /c/Users/me/proj", // a path under a drive, not the drive
		"rm -rf ~/project/dist",
		"rm -f /etc/hosts.bak", // no -r, and not a root
		"curl -o data.json https://example.com/api",
		"dd bs=1M count=1 if=/dev/zero of=zero.bin", // a file, not a device
		"npm run format",
		"gh pr view 34 --json state",
		"gh pr list --state open",
		"gh pr create --title t --body b", // not this rule's; the guarded environment has no gh login
		"gh release list",
		"gh api repos/o/r/pulls/1",
		"npx wrangler dev",
		"kubectl get pods",
		"docker build -t app .",
		"npm run build",
		"npm run predeploy-check", // deploy inside a word is not the script's name
		"git config user.name hrn",
		"git config --get remote.origin.url",
		"GIT_TERMINAL_PROMPT=0 git fetch origin", // the prompt switches are not logins
		// Reading or naming the guard's variables changes nothing.
		`echo "$GIT_CONFIG_KEY_0"`,
		"echo $SSH_AUTH_SOCK",
		"rg GIT_CONFIG_COUNT internal/",
		"k3d kubeconfig get lab",
		"kind get kubeconfig",
		"kubectl --kubeconfig k.yaml get pods",
		"kubectl --kubeconfig=k.yaml get pods", // a flag, not a variable
		"cat docker_config.go",
		"env | sort",
		// Files whose names only resemble the secret ones.
		"cat src/env.go internal/private/key.go",
		"source .venv/bin/activate",
		"jq -r .version package.json",
		"rm -rf .wrangler/state .wrangler/tmp", // a worker's local state, not wrangler's login
		"npx wrangler dev --persist-to .wrangler/state",
		"cat docker-compose.yml config.json",
		"go test ./internal/tools -run TestSecretFile",
		"terraform plan",
		"tofu validate",
		"helm template ./chart",
	} {
		if got := Guard(command); got != "" {
			t.Errorf("Guard(%q) = %q, want allowed", command, got)
		}
	}
}

func TestGuardErrorNamesTheRuleAndTheSetting(t *testing.T) {
	err := GuardError("git-push")
	for _, want := range []string{"git-push", "--allow-dangerous", "allow_dangerous", "Do not retry"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("GuardError text is missing %q: %v", want, err)
		}
	}
}

func TestRunCommandGuard(t *testing.T) {
	ws, tl := setup(t)
	run := tl["run_command"]

	// The tool refuses before the shell runs, and says so before an approval
	// is asked for, so the two answers cannot disagree.
	if rule := run.Refuse(ws, []byte(`{"command":"touch marker.txt; sudo true"}`)); rule != "sudo" {
		t.Errorf("Refuse = %q, want sudo", rule)
	}
	out, err := call(t, ws, run, map[string]any{"command": "touch marker.txt; sudo true"})
	if err == nil || !strings.Contains(err.Error(), "hrn guard (sudo)") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if _, statErr := os.Stat(filepath.Join(ws.Root, "marker.txt")); statErr == nil {
		t.Fatal("a blocked command line must not run at all")
	}

	ws.AllowDangerous = true
	if rule := run.Refuse(ws, []byte(`{"command":"sudo true"}`)); rule != "" {
		t.Errorf("Refuse with allow_dangerous = %q, want none", rule)
	}
	out, err = call(t, ws, run, map[string]any{"command": `echo "git push"`})
	if err != nil || !strings.Contains(out, "git push") {
		t.Fatalf("allow_dangerous must lift the guard: out=%q err=%v", out, err)
	}
}

func TestRefuseIgnoresUnreadableInput(t *testing.T) {
	ws, tl := setup(t)
	// A malformed call is the loop's problem to report, not the guard's.
	if rule := tl["run_command"].Refuse(ws, []byte(`{"command":`)); rule != "" {
		t.Errorf("Refuse on broken JSON = %q, want none", rule)
	}
}

// A command the agent runs gets the guarded environment too: no model key to
// print into the run, no token to push or merge with. A finding of the PR W
// security review, which saw an env command put the API key into a run.
func TestRunCommandRunsUnderTheGuardedEnvironment(t *testing.T) {
	if name, _ := Shell(); name == "powershell" {
		t.Skip("the command below is for a POSIX shell")
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-should-not-leak")
	t.Setenv("GH_TOKEN", "ghp_should_not_leak")
	ws, tl := setup(t)
	run := tl["run_command"]
	// env prints what the command was started with; the prefixes keep the
	// output to the three variables that matter.
	show := map[string]any{"command": `env | grep -E '^(ANTHROPIC_API|GH_TOK|GIT_CONFIG_C)'`}
	out, err := call(t, ws, run, show)
	if err != nil || strings.Contains(out, "should-not-leak") || strings.Contains(out, "should_not_leak") || !strings.Contains(out, "GIT_CONFIG_COUNT=") {
		t.Fatalf("under the guard the command saw %q (%v); want no key, no token, git's push guard", out, err)
	}
	ws.AllowDangerous = true
	if out, err := call(t, ws, run, show); err != nil || !strings.Contains(out, "ANTHROPIC_API_KEY=sk-ant-should-not-leak") || !strings.Contains(out, "GH_TOKEN=ghp_should_not_leak") {
		t.Fatalf("with allow_dangerous the command saw %q (%v); want the operator's environment", out, err)
	}
}
