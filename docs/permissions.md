# Permissions and command guard

[← README](../README.md)

### Permission modes

| Mode | api backend | claude | codex | gemini | opencode | agy |
|---|---|---|---|---|---|---|
| `readonly` | read tools only (write tools not offered) | `plan` | `--sandbox read-only` | `default` | its config | `--mode plan` |
| `ask` (default) | reads auto; writes + commands prompt y/N | `default` | `--sandbox read-only` | `default` | its config | `--mode plan` |
| `auto` | reads + edits auto; commands prompt | `acceptEdits` | `--sandbox workspace-write` | `auto_edit` | its config | `--mode accept-edits` |
| `yolo` | everything | `bypassPermissions` | bypass sandbox | `yolo` | `--auto` | `--dangerously-skip-permissions` |

agy maps `ask` to plan mode without interactive approval. Its `plan` mode is not a
filesystem sandbox; see [agy's mappings](backends.md#backends).

When the prompt comes from stdin there is no terminal to ask, so anything needing
approval is denied.

### api tools

`read_file`, `list_files` (glob with `**`), `grep` (RE2), `write_file`, `edit_file`
(exact unique match, CRLF-aware), `run_command` (Git Bash when present, else PowerShell;
120s default timeout). All paths are confined to the workspace and judged where their links
lead: symbolic links, and on Windows directory junctions too (Go has not counted a junction
as a link since 1.23, so hrn reads its target itself). A path through a link out of the
workspace is refused, and so is one through a link hrn cannot read; a path through a link
that stays inside works. `grep` over a directory goes into no link or junction and passes
over a file link that leads out. A link to a file that does not exist yet is judged by its
target too, so `write_file` cannot create one outside through it. On Linux and macOS a
target with a `..` after a name (`a/../b`) is refused: the system takes that `..` after
following `a`, which may itself be a link.
Read-only calls in one turn run in parallel; writes and commands run in order.

`read_original` is a read-only api tool, offered only when `--shape-tools` is on,
and allowed in `readonly` mode like `read_file`. It reads saved shell/MCP output from this run's record directory,
rather than a workspace path. Its required `handle` is exactly 16 lowercase hex
characters, with no path separators; an unknown handle is an error naming it.
An optional `range` is `a-b` in zero-based byte offsets, start inclusive and end
exclusive; invalid or out-of-bounds ranges are refused. Without a range it returns
all bytes without truncating or shaping them. Links that leave `originals/` are
refused. The harness stores originals as run records when `--shape-tools` is on;
the tool itself never writes them.

#### The command guard

`run_command` refuses seven kinds of command line before the shell sees them:

| Rule | Refuses |
|---|---|
| `git-push` | any `git` command mentioning `push`, including `git -C dir push`, or setting a `pushurl` or `pushInsteadOf` — a batch commits on its own branch; what leaves the machine is the operator's call |
| `guard-env` | a command that changes a variable of the guarded environment below (`GIT_CONFIG_COUNT`/`KEY_n`/`VALUE_n`/`PARAMETERS`, `GH_CONFIG_DIR`, `NPM_CONFIG_USERCONFIG`, `KUBECONFIG` and the other login places, `SSH_AUTH_SOCK`, gh's stand-in `GH_TOKEN` and `GH_ENTERPRISE_TOKEN`, the `HRN_GUARDED` marker): setting it (`NAME=`, `NAME+=`, `export`, `$env:NAME =`, `${env:NAME} =`, `read`, `printf -v`, `GIT_CONFIG_KEY_$i=` in a loop), unsetting it or hiding it from child processes (`unset`, `export -n`, `declare +x`, `typeset +x`, `env -u`, `SetEnvironmentVariable`), `env -i`, and any use of PowerShell's `env:` drive (or `Environment::`) as a path on one of them or with a wildcard (`Remove-Item env:X`, `rm env:X`, `& {Set-Item env:X …}`), whatever the cmdlet, reads through that path (`Test-Path env:X`) included; any case. Reading one as a variable (`$X`, `$env:X`, `${env:X}`), searching for it, or a flag like `--kubeconfig` or `--read-only` is fine |
| `secret-file` | a command line naming a secret file (the list below) as an argument, a redirect's target or a flag's value: `cat .env`, `echo x > .env`, `cp .env.example .env`, `Get-Content .\.env`, `cat ~/.ssh/id_ed25519` |
| `sudo` | `sudo` in any case — a password prompt hangs a run with no terminal |
| `rm-root` | `rm -r` whose target is a root: `/`, `/*`, `~/`, `"$HOME"`, `C:\`, `C:/`, `/c/` |
| `pipe-to-shell` | `curl`/`wget`/`irm`/`iwr`/`Invoke-RestMethod`/`Invoke-WebRequest` piped into a shell, `iex` or `Invoke-Expression`, however many stages the pipe has, including a shell invoked through PowerShell's `&` call operator or dot-sourced with `.`, quoted or by full path (`& "pwsh"`, `& 'C:/.../powershell.exe'`, `/bin/bash`); also `iex` or `Invoke-Expression` (with or without `-Command`) given a parenthesized or `$(...)` download from those commands or `(New-Object Net.WebClient).DownloadString(...)` (`System.Net.WebClient` too); names match in any case. Invoking a downloaded scriptblock through `& ([scriptblock]::Create((irm u)))`, `.([scriptblock]::Create((irm u)))` or `[scriptblock]::Create((irm u)).Invoke()` (`.InvokeReturnAsIs()` too) is refused too, with `$(...)`, a `(New-Object Net.WebClient).DownloadString(...)` download, or the full type name `[System.Management.Automation.ScriptBlock]`; constructing the scriptblock without invoking it, `&` with ordinary programs, and non-download arguments such as `iex $script` stay allowed |
| `disk` | `mkfs`, `format X:`, `dd of=/dev/…`; program names match in any case |
| `gh-publish` | `gh pr merge`, `gh release create/upload/edit/delete`, `gh repo delete/archive/rename` or `--visibility` (`gh` in any case); `gh api` with PUT, PATCH or DELETE (any case, quoted or escaped); and `gh api` to `/releases`, `/git/refs`, `/git/tags`, `/rulesets`, `/keys`, `/hooks`, `/collaborators`, `/actions/workflows/<x>/dispatches`, `/dispatches`, `/merges`, `/environments`, `/actions/secrets`, `/actions/variables`, `/transfer`, `/deployments`, `/actions/runs/<id>/pending_deployments`, `/statuses/<sha>`, `/actions/runners/registration-token`, `/actions/runners/generate-jitconfig`, `/actions/runners/remove-token`, `/branches/<b>/rename`, `/merge-upstream`, `/forks`, `user/repos`, `orgs/<org>/repos`, `gists` (as the endpoint itself or after a `/`, so a field that only mentions the word stays allowed) or `/pages` (including paths below them), or `gh api graphql` with a mutation in the line (any case). All `gh api` patterns also match `gh.exe`, any case of the executable name, and full paths (quoted too). Endpoint refusals include GET to keep implicit POSTs covered; ordinary POSTs such as issue and pull request creation or comments stay allowed |
| `gh-token` | `gh auth token` (with `--user` too), `gh auth git-credential` and `gh auth status --show-token`/`-t`: gh keeps its login in the system's credential store, which no variable hides, and these hand it out |
| `deploy` | `wrangler deploy` or `publish` (including `pages deploy`/`pages publish`), `npm`/`pnpm`/`yarn`/`bun`/`cargo publish`, a project's deploy script (`npm run deploy`, `deploy:prod`, `deploy-docs`, with `pnpm`, `yarn` or `bun` too; flags and workspace selectors may intervene, as in `pnpm --filter web run deploy`, `npm -w web run deploy`, `npm --prefix site run deploy`, `yarn workspace web deploy`; `npm run-script deploy` too), `deploy/push.sh`, `kubectl apply/delete/replace/patch/rollout`, `docker push`, `terraform`/`tofu apply/destroy`, `helm install/upgrade/uninstall`, `vercel --prod`, `fly`/`netlify deploy` |

The api backend's file tools refuse the secret files as well (rule `secret-file`):
`read_file`, `write_file`, `edit_file` and `grep` on a `.env*` file, `.dev.vars`,
`*.pem`, `*.key`, `*.p12`, `*.pfx`, `*.jks`, `*.keystore`, `.git-credentials`,
`.netrc`/`_netrc`, `.npmrc`, `.pypirc`, anything inside a `.private`, `.ssh`, `.aws`,
`.kube` or `.azure` directory, and the logins that are secret only where they sit: gh's
(`.config/gh/`, `GitHub CLI/` on Windows), gcloud's, `.docker/config.json`,
`.cargo/credentials*`, wrangler's `.wrangler/config/` (a worker project's own `.wrangler`
holds local state and stays open), and the agent CLIs' plan logins
(`.claude/.credentials.json`, `.codex/auth.json`, `.gemini/oauth_creds.json`,
`.local/share/opencode/auth.json`). A path is
judged inside the workspace, both as written and where its links lead, Windows directory
junctions included (`notes.txt -> .env` is `.env`), with Windows' other spellings of a
name (`.env.`, `.env::$DATA`, a short `8.3` name) read as the name; `grep` over a
directory passes over secret files, and a link among them that leads to one.
`.env.example` is refused too: a template costs the agent one turn, and `.env.staging`
or `.envrc` are what the pattern is for. The command rule reads words (and quoted paths,
and git's `REV:path`), so `echo "*.pem" >> .gitignore` is refused as well (`edit_file` on
`.gitignore` is not), and so is a `jq '.key'` filter. Unlike the file tools, the command
rule does not expand short `8.3` names: `cat ~/SSH~1/id_ed25519` gets past it.

`rm-root` reads the arguments rather than matching one pattern, because the spelling
that matters is not the famous one: `rm -rf /` is refused by `rm` itself without
`--no-preserve-root`, while `rm -rf /*` and `rm -rf "$HOME"` are not.

The refusal comes back to the model as a failed tool call naming the rule, so the run
continues and the transcript says what was stopped. It is decided before any approval
prompt, so `ask` mode never asks for a yes it would then ignore. Matching is textual and
leans towards refusing too much: `echo "git push"` and `git commit -m "push later"` are
refused as well, which costs a turn and is the trade this makes on purpose.
Separators inside quotes do not end a pipeline stage for the guard.
A GraphQL query read from a file (`-F query=@m.graphql`) is outside this textual
guard: it does not inspect the file contents.

This is a guard rail, not a sandbox — an agent that wants around a pattern can get
around it. What decides whether commands run at all is the [permission mode](#permission-modes);
this stops the plausible accident. `--allow-dangerous` (task field `allow_dangerous`,
also a batch spec/task field and an MCP `run` argument) lifts it for a whole run, and the
system prompt tells the model which way it is set; `yolo` on its own does not lift it.
The hrn MCP server refuses `allow_dangerous`, and does not offer it, unless you started it
with `hrn mcp --allow-dangerous-runs`: its caller is an agent, perhaps one under the guard
whose CLI loaded it (the claude CLI loads your user-scope servers, hrn's among them), and
whether a CLI passes its environment on to the servers it starts is that CLI's business.
An hrn that does see the guard refuses it too: the guarded environment sets `HRN_GUARDED`
(it reaches codex's commands, checked live), the api loop sets it for its own MCP servers,
and `hrn run --allow-dangerous` from an agent's shell is refused. A refused command tells
the model only that the operator can lift the guard.

Two things it does not cover, by design: tools from an MCP server are the server's
business, and batch gate commands are not checked, because the operator wrote those.
Gates keep hrn's full environment (less the git repository variables below) and execute the code the agent wrote, including
tests, so that code can use the operator's credentials. hrn's own batch `git status`,
`git add` and `git commit`, and in the operator's repository `hrn batch merge` and the
`git worktree add` of a fresh task worktree, neutralise repository config keys that run programs
(`-c core.hooksPath=<empty temporary directory>`, `-c maintenance.auto=false`,
`-c gc.auto=0`; `GIT_NO_LAZY_FETCH=1` always, and `-c protocol.allow=never` when the
repository config names a promisor remote (`extensions.partialClone`, `remote.<name>.promisor`,
`remote.<name>.partialCloneFilter`),
so that remote cannot have git fetch a missing object through its `uploadpack` program; `-c core.fsmonitor=false`
when the repository config sets `core.fsmonitor`; `-c commit.gpgSign=false` and
`-c merge.verifySignatures=false` when it sets a `gpg.*program` or `gpg.ssh.defaultKeyCommand`,
so neither signing the merge commit nor verifying a branch tip's signature runs that program
(with the operator's own `gpg.program`, a `merge.verifySignatures` still applies); and empty `filter.<name>.clean`, `filter.<name>.smudge`,
`filter.<name>.process` with `filter.<name>.required=false` for every filter driver it
defines; and `merge.<name>.driver=false` for every merge driver it defines, so a path that
driver would merge becomes a conflict and `hrn batch merge` undoes that merge), so settings
an agent writes to the shared repository config (`core.hooksPath`, `core.fsmonitor`, filter
and merge drivers, `gpg.*` with `merge.verifySignatures`, promisor remotes,
`gc.recentObjectsHook`) cannot make that commit run a command. "Repository config" is the local and worktree
scope (`git config --show-scope`): what the operator's global or system config defines,
such as the Git LFS filter, still runs, unless the repository config redefines it.
In the operator's repository the hooks path is the `core.hooksPath` from the operator's global
or system config when one is set, and otherwise the repository's own hooks directory
(`<common-dir>/hooks`), rather than an empty one: the operator's hooks still run there and a
`core.hooksPath` in the shared config does not, which also means a hooks path set in the
repository config by the operator (as husky does) is not followed. The limit this leaves: an
agent that can write files outside its worktree can place a `post-merge` or `post-checkout`
hook in `<common-dir>/hooks` (or change the operator's global config), and hrn's merge and
worktree add run it; an agent limited to `git config` cannot. hrn reads the repository config again right before each of those commands, which leaves
a process the agent left running only the moment between that read and the command; and it
pins `GIT_DIR` and `GIT_WORK_TREE` to the tree it means, so a `core.worktree` the agent sets
cannot make that commit take files from outside the task's worktree. The next `hrn batch`
finds the repository root from the `.git` entry above its directory, not from
`rev-parse --show-toplevel`, so that `core.worktree` cannot move the batch to another tree.
These overrides travel as `GIT_CONFIG_*` environment variables, so they also reach the
operator's own hooks during `hrn batch merge` and `git worktree add`: in a repository whose own
config names a promisor remote, a hook that fetches or pushes fails there. In a partial clone
hrn does not fetch missing objects either; if a merge reports a missing object, run
`git fetch` (or `git backfill`) first. Slots, the default
(`"worktrees": "slots"`), are still made and switched with plain git (`internal/ws`), so the
shared config's hooks and filters can run there.
`hrn batch diff` runs `git diff --no-ext-diff --no-textconv --ignore-submodules=all`, so
`diff.external`, a diff driver's command or textconv, and a submodule's config cannot make
it run a command either. Neither command guard refuses `git config` for those keys,
and the settings remain in the shared config for other git commands, including the
operator's later commits.
The agent CLIs run their own shells; the next section is their guard.

Guarded or not, hrn drops `GIT_DIR`, `GIT_WORK_TREE`, `GIT_INDEX_FILE`, `GIT_COMMON_DIR`,
`GIT_OBJECT_DIRECTORY`, `GIT_ALTERNATE_OBJECT_DIRECTORIES` and `GIT_NAMESPACE` from the
environment of its own git commands, of agent CLIs, of the api backend's commands and MCP
servers, and of gates (an MCP server's own `env` in its config still applies). A git hook or a `!` alias that starts hrn sets some of them, and git would then act
on that repository instead of the task's.

#### The guard for every backend

A command line can be spelled many ways, so every backend also runs under a guarded
environment. A CLI backend (claude, codex, gemini, opencode) inherits it, and so does
every command the api backend's `run_command` runs:

- tokens, keys, secrets and passwords are left out, by name (`GH_TOKEN`,
  `CLOUDFLARE_API_TOKEN`, `AWS_SECRET_ACCESS_KEY`, `NPM_TOKEN` …) and by pattern
  (`*_TOKEN`, `*_KEY`, `*_PAT`, `*_AUTH`, `*SECRET*`, `*PASSWORD*`, `*CREDENTIAL*`,
  `*PRIVATE_KEY*`, Terraform's `TF_TOKEN_*`, 1Password's `OP_SESSION_*`,
  `BW_SESSION`), model API keys among them: a command has no use for them, and a CLI
  that finds one bills per token instead of using its own login. Git Credential
  Manager's `GCM_*` settings stay: they say where its store is, not what is in it, and a
  private fetch needs them. git's askpass helpers
  go too (`GIT_ASKPASS`, `SSH_ASKPASS`, VS Code's `VSCODE_GIT_*`), since they hand out
  a login when git asks for one. `CLAUDE_CODE_OAUTH_TOKEN`, claude's plan login,
  reaches the claude CLI and no other. Claude's own Bash inherits it too: a plain
  `env` can put the token into the run record;
- the logins tools keep on disk are looked for in an empty directory of the run's own:
  gh (`GH_CONFIG_DIR`), npm (`NPM_CONFIG_USERCONFIG`), kubectl (`KUBECONFIG`), docker
  (`DOCKER_CONFIG`), aws (`AWS_SHARED_CREDENTIALS_FILE`, `AWS_CONFIG_FILE`), gcloud
  (`CLOUDSDK_CONFIG`) and az (`AZURE_CONFIG_DIR`); the directory goes when the run ends.
  gh also keeps its login in the system's credential store (Windows Credential Manager,
  the macOS Keychain) and asks it when no token is set, so `GH_TOKEN` and
  `GH_ENTERPRISE_TOKEN` are set to a token that is not one;
- the ssh agent is out of reach (on Windows `SSH_AUTH_SOCK` names a pipe nobody
  listens on, since Windows' ssh otherwise falls back to its agent's own pipe);
- git points every push at `push-refused-by-hrn://`, which it cannot reach, through
  `GIT_CONFIG_*` (your own `GIT_CONFIG_*` entries stay): the usual prefixes and an empty
  one that catches the rest (scp-style aliases, shorthands, upper case, local paths),
  so `git push`, `git -C dir push` and `bash -c "git push"` fail at once without
  touching the network unless a longer existing `pushInsteadOf` wins (see below).
  Fetching keeps its address.

The claude backend also gets the guard as Claude Code's own deny rules
(`--disallowedTools`), which hold in every permission mode, `bypassPermissions`
included:

- the publishing and deploying commands of the table above, in both its Bash and
  PowerShell tools, and the commands that would steer a push around the environment
  (`git config …push…`, `git -c …push…`, `git remote set-url --push`, `unset
  GIT_CONFIG…`, `env -u`); a test keeps the two lists refusing the same commands;
- the secret files, with a `Read` rule and an `Edit` rule each, since a Read rule does
  not stop a Write that creates the file: the same list as the api backend's, anywhere
  on the disk, the agent CLIs' plan logins among them (the CLIs read those with their
  own code, not through these tools).

The command rules match command text, so the environment is what stops the other
spellings; what they add is an early, plain refusal and the secret files. Checked live
against the claude CLI in `yolo`, all refused by the rules: `git push` with and without
arguments, setting a `pushurl` or a `PushInsteadOf`, `npm run deploy:prod`, `gh api …
-X DELETE`; creating `.env.production` with the Write tool, editing `.env` with the Edit
tool, and writing secret files from Bash (`echo X=3 > .env.staging`, `printf … >> .env`,
`cp .env .env.bak`); reading `.env`, gh's `hosts.yml`, a `.private/` file and a `*.key`
(with the Read tool and with `cat`). `git -C . push`, which no rule matches, failed at
the environment's dead end, and the remote stayed empty. A PowerShell `Set-Content` of
`.env.test` was refused too, but by Claude Code's working-directory check rather than a
deny rule, so that path is not shown to hold by the rules alone.
Claude's Read and Edit globs have not been checked live with short `8.3` names such as
`SSH~1`, stream suffixes such as `x.key::$DATA`, trailing dots such as `auth.json.`, or
case variants such as `.ENV`; the api file tools' handling of these spellings does not
establish Claude Code's behavior.

What the guard does not stop, said plainly. It is a guard rail, not a sandbox:

- an agent that sets out to undo the environment can: a `pushurl` or a longer
  `pushInsteadOf` in the repository's own config, a `git -c`, or an unset
  `GIT_CONFIG_COUNT` gives git its address back, and git's credential helper then signs
  the push in (Git Credential Manager keeps its login in the system's credential store,
  where no variable reaches). Both guards refuse the plain spellings of those commands;
  the api backend's `guard-env` reads text, so a spelling it has no pattern for gets past
  it (a relative name after `Set-Location env:`, `GIT_CONFIG_KEY_"$i"=` with the index
  quoted, `declare -g +x`).
  The credential helper itself is left alone, because fetching private repositories and
  Go modules needs it;
- an operator's existing longer `pushInsteadOf` in `~/.gitconfig` can already beat the
  guard's prefixes: git uses the longest matching prefix, even when the guard's entry
  comes from `GIT_CONFIG_*`. An HTTPS-to-SSH or SSH-to-HTTPS rewrite for a particular
  host or repository can therefore restore a usable push address without the agent
  changing any config;
- an ssh key file on disk without a passphrase still works for an ssh remote;
- logins kept where no variable moves them: wrangler's, cargo's, fly's, netlify's and
  vercel's, Terraform Cloud's `~/.terraform.d/credentials.tfrc.json`, `~/.pypirc`
  (read by twine and uv themselves), and yarn berry's `~/.yarnrc.yml`. Under codex,
  gemini or opencode a `npx wrangler deploy` or `npm run deploy` can still deploy;
  claude and the api backend refuse those commands, the others get the environment only.
  `yarn npm publish` is not in claude's command rules. File protection and command
  refusal are separate: protecting `.pypirc` from a direct read does not stop a
  publishing tool from loading it itself;
- gh's login in the system's credential store: `gh auth token --user NAME` reads it
  whatever `GH_TOKEN` says. Claude and the api backend refuse `gh auth token`; under
  codex, gemini or opencode an agent that runs `gh auth token --user NAME` gets your
  GitHub token (plain `gh auth token` gets the stand-in, which reaches codex's commands);
- secret files stay readable and writable under codex, gemini and opencode; under the
  api backend and claude, a command that opens one without naming it (a recursive
  `grep -r` from the shell, a tool that loads `.env` itself) is not seen, and nor is a
  login named by a relative path after a `cd` (`cd ~/.codex && cat auth.json`), since
  the logins that are secret only with their folder are matched in the command's text;
- secrets inside ordinary variables stay: a `DATABASE_URL` with a password in it, since
  local tests need it;
- a CLI that logs in with an API key (opencode or gemini with a key, claude on Bedrock or
  Vertex) needs `--allow-dangerous`, which lifts the push guard as well;
- handing work over is hrn's (batch keeps a branch per task) or the operator's; the
  guard is intended to prevent backend pushes and pull requests, subject to the gaps above.

Every run says whether the guard is on, names the variables it withheld, and says what
its backend leaves open: on codex, gemini and opencode, the secret files, the wrangler
and cargo logins, and gh's login in the credential store. `--allow-dangerous` lifts all
of it.
