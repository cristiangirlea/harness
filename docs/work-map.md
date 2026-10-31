# Work map

The work map collects open `PLAN.md` checklist items, local Git branches and worktrees,
and hrn run and batch records. It joins records that refer to the same branch and
reports failed batch gates. Results use the furthest known stage, in this order:
idea, branch, agent, gates, review, pr, ci, merged, released. Local sources cover
idea, branch, agent, gates and merged; slices 2–5 will add the remaining sources
and views.

Set `work_map_repos` to a list of repository paths in `backends.json` under HRN_HOME.
When omitted, repositories are discovered from run and batch records. Unreadable
sources are returned as errors alongside any results that could be collected.
