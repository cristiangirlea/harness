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
the watch page is readable on the network without a login.
