# Work map

The work map collects open `PLAN.md` checklist items, local Git branches and worktrees,
and hrn run and batch records. It joins records that refer to the same branch and
reports failed batch gates. Results use the furthest known stage, in this order:
idea, branch, agent, gates, review, pr, ci, merged, released. Local sources cover
idea, branch, agent, gates and merged; GitHub adds pull requests, checks and issues.

Set `work_map_repos` to a list of repository paths in `backends.json` under HRN_HOME.
When omitted, repositories are discovered from run and batch records. Unreadable
sources are returned as errors alongside any results that could be collected.

## GitHub

For GitHub origin remotes, the map reads open issues and pull requests and recently
merged pull requests using the logged-in `gh` CLI. It joins same-repository pull requests to local
branches, reports failing checks and requested changes, and caches each repository
under `workmap/github` in HRN_HOME for five minutes. A failed refresh returns the
cached data and an error, and retries after five minutes. Set `work_map_github` to `false` in `backends.json` to
disable this source. Only titles, states, numbers, links, check names, branch names and dates are kept;
only the local owner can open the work map page.

## Watch page

An owner can open `/map` in hrn watch for repository swimlanes and a list of work that waits for attention. The page updates from `/stream/map` every 30 seconds when the map changes. Both routes are owner-only; other viewers receive 404. Source errors appear alongside any partial results.

The bot also checks the work map every 30 seconds for an owner and announces newly stuck work and advances after a silent first look, using its usual message settings.

## Claude Code sessions

Recent local Claude Code transcripts contribute branch sessions. Only `cwd`, `gitBranch`, `sessionId`, and `timestamp` are read from the last 64 KiB of each transcript. Up to 200 files modified in the last 24 hours are considered; sessions modified in the last 15 minutes are active. Set `work_map_sessions` to `false` in `backends.json` to disable this source. Session IDs stay local and are not shown on the page.
