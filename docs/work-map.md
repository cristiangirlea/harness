# Work map

The map joins work by branch and only raises its stage. Sources, in stage order:

- **idea**: open PLAN.md items and GitHub issues.
- **branch**: local branches, worktrees and sessions.
- **agent**: runs, batches and active sessions.
- **gates**: batch gate results.
- **review**: a finished run with declared purpose `review` raises, and links, the local branches
  that contain the commit it started on; nothing when the default branch already contains that
  commit or the run has no recorded start commit.
- **pr** and **ci**: GitHub pull requests and checks.
- **merged**: Git ancestry and GitHub merged pull requests.
- **released**: a merged pull request's merge commit is contained in a local `v*` tag.

A failed gate, red CI, requested changes, or a same-repository non-draft pull request with passing checks and no requested changes marks work as stuck. The last case says it is ready for you to merge.

Set `work_map_repos` to a list of repository paths in `backends.json` under HRN_HOME.
When omitted, repositories are discovered from run and batch records. Discovery quietly skips missing folders and non-repositories; explicitly configured entries report errors. Other unreadable sources are returned as errors alongside partial results.

## GitHub

For GitHub origin remotes, the map reads open issues and pull requests and recently
merged pull requests using the logged-in `gh` CLI. It joins same-repository pull requests to local
branches (or uses a separate `pr#N` piece when no local branch exists), reports failing checks and requested changes, and caches each repository
under `workmap/github` in HRN_HOME for five minutes. A failed refresh returns the
cached data and an error, and retries after five minutes. Set `work_map_github` to `false` in `backends.json` to
disable this source. Only titles, states, numbers, links, check names, branch names, dates, merge commit oids and the last failure text are kept;
only the local owner can open the work map page.

## Watch page

An owner can open `/map` in hrn watch for repository swimlanes and a list of work that waits for attention. The page updates from `/stream/map` every 30 seconds when the map changes. The page, its stream and Ada share one build per 30 seconds. Ada's page links to the map and shows how many pieces wait. Both routes are owner-only; other viewers receive 404. Source errors appear alongside any partial results.

The bot also checks the work map every 30 seconds for an owner and announces newly stuck work and new pieces or advances at gates or later after the first look, using its usual message settings. On the first look Ada gives one `waits` summary of existing stuck pieces, naming up to three; when none wait she says nothing. `waits` is enabled for speech and notifications by default, while `map` is disabled for both.

## Claude Code sessions

Recent local Claude Code transcripts under `$CLAUDE_CONFIG_DIR/projects` (or `~/.claude/projects` when unset) contribute branch sessions. Only `cwd` and `gitBranch` are read from the last 64 KiB of each transcript; the session ID comes from its UUID-shaped filename. Links inside the projects folder are skipped. Up to 200 files modified in the last 24 hours are considered; sessions modified in the last 15 minutes are active and raise the piece to agent if it is earlier. A cwd inside a linked Git worktree joins its repository. Set `work_map_sessions` to `false` in `backends.json` to disable this source. Session IDs stay local and are not shown on the page.
