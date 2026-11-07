# Retrieval pilot report (increment 4)

The pilot planned in [retrieval-pilot.md](retrieval-pilot.md) ran on 2026-11-07, on the maintainer's
word, as an override of the step 0 no-go. It made 60 codex runs (`gpt-6.1-sol`) on the codex plan.
No `api` backend and no API key were used.

**Result.** Required text retrieval (`simple-rare-floor-v4`) answered as many tasks as no
retrieval did and used 44% fewer model tokens per task at the median (paired median 50%). When the
packet held the answer itself (5 of the 10 tasks), a task took about 25,500 tokens and no tool
calls, against about 94,000 tokens and three tool calls without retrieval. When the packet missed
the answer's guide, it cost about 7% more.

**Verdict.** The comparison's reason is "higher success" (29 of 30 against 28 of 30). The plan's
table reads that as "supported: retrieval raises success" and asks for the margin. The margin is
one cell, and it is not real: all three failures, two `off` and one `text`, were correct answers
that the `absent-command` pattern did not accept (see "Hand reading"). Read by hand, both variants
answer 30 of 30. On equal success, the 44% median reduction is well above the threshold (10%; the
baseline moved 0.6% between repeats), so the row that applies in substance is "token saving of 44%
for codex `gpt-6.1-sol` on these tasks". The gate itself stays `pending` until maintainer decision 2
(binding) is taken.

| | `off` | `text` |
|---|---|---|
| pairs (task × repeat) | 30 | 30 |
| passed (pattern) | 28 | 29 |
| passed (read by hand) | 30 | 30 |
| median model tokens | 98,231 | 54,861 |
| p95 model tokens | 121,601 | 108,632 |
| tokens per success (pattern) | 102,299 | 63,586 |
| total model tokens | 2,864,370 | 1,843,998 |
| median uncached input | 15,135 | 12,022 |
| median cached share of input | 84% | 76% |
| median tool calls | 3 | 1 |
| p50 / p95 time | 20.4 s / 26.2 s | 9.6 s / 23.8 s |
| median packet | none | 7,184 bytes |
| estimated USD (plan estimate, not billed) | 1.30 | 1.01 |

Model tokens are input plus output; codex counts cached input inside input. p95 is interpolated linearly between ranks. Both variants report
usage on every cell (`usage_metadata.status: reported`), so all 30 pairs are comparable.

## Per task

| Task | Kind | `off` passed | `text` passed | `off` median tokens | `text` median tokens | `text` tool calls | Answer's guide in packet |
|---|---|---|---|---|---|---|---|
| `gate-timeout` | in corpus | 3/3 | 3/3 | 94,226 | 25,424 | 0 | 3/3 |
| `markdown-max-words` | in corpus | 3/3 | 3/3 | 96,360 | 25,617 | 0 | 3/3 |
| `fixture-port` | in corpus | 3/3 | 3/3 | 101,275 | 25,547 | 0 | 3/3 |
| `job-cost-cap` | in corpus | 3/3 | 3/3 | 98,507 | 107,660 | 3 | 0/3 |
| `postgres-boundary` | in corpus | 3/3 | 3/3 | 74,196 | 25,463 | 0 | 3/3 |
| `retrieval-record` | in corpus | 3/3 | 3/3 | 121,469 | 78,566 | 2 | 3/3 |
| `slot-lock-wait` | in corpus | 3/3 | 3/3 | 92,550 | 25,731 | 0 | 3/3 |
| `watch-log-rotation` | outside corpus | 3/3 | 3/3 | 98,239 | 107,151 | 3 | n/a |
| `query-bytes` | code only | 3/3 | 3/3 | 103,222 | 106,011 | 3 | n/a |
| `absent-command` | does not exist | 1/3 | 2/3 | 79,021 | 85,028 | 2 | n/a |

- **Relevant context, by kind:** the answer's guide was in the packet for 18 of the 21 `text`
  cells of in-corpus tasks (6 of 7 tasks, the same as step 0). The other kinds (outside corpus,
  code only, does not exist) have no answer guide, so the rate does not apply; they get related
  entries instead. For the 18 cells the median paired change was −67%. `retrieval-record` got its guide
  but not the line with the answer, so the agent still searched (−35%).
- **Misses:** `job-cost-cap` never got its guide, and the three tasks with no answer in the corpus
  got 2–3 related entries every time. Those 12 cells cost a median 7% more than `off`: the packet
  is added and the agent searches anyway.

## Hand reading

The plan asks for every passing answer to `postgres-boundary`, `absent-command` and the duration
tasks to be read by hand.

- `postgres-boundary`: all 6 answers are "Control service.".
- `gate-timeout`, `slot-lock-wait`: "15 minutes." and "2 minutes" or "Up to two minutes" in all 12.
- `absent-command`: all 6 answers say there is no `hrn teleport` command and invent no flags. Four
  say it as "This repository has no `hrn teleport` command…", which the pattern does not match
  (it accepts "there is no", "no such", "does not exist" and similar, not "has no"); one of the
  four passes only because it also says "no such command". The other three are the pilot's only
  failures: `3796b.absent-command.off.r1`, `3796b.absent-command.text.r1`,
  `6e3f1.absent-command.off.r1`. Per the plan, a cell that ran and failed is a result; none was
  rerun and the pattern was not changed.

## Isolation

No isolation failure in any of the 60 cells:
- no cell changed or committed files in its read-only worktree (`isolation_failure` is false in every
  sample, and no cell branch kept a commit);
- every `text` packet came from the pilot's tenant, project and version (`compare --runs`);
- no run's `events.jsonl` mentions the pilot folder, the credentials folder or the task file
  (searched for all three in every run).

## Cache

| Variant | order | cells | median cached share |
|---|---|---|---|
| `off` | `off` first | 15 | 85% |
| `off` | `text` first | 15 | 84% |
| `text` | `off` first | 15 | 54% |
| `text` | `text` first | 15 | 80% |

The variants' medians are 8 points apart, inside the plan's 10-point limit, so the check holds.
Within `text` the order matters (54% against 80%), and the uncached input medians are given above
(15,135 against 12,022, 21% lower for `text`). The six evals ran back to back (06:21–06:41 UTC), so only
the first cell of A0 could be cold; other work on the same account may have warmed it.

## Binding checks

1. **Corpus digest:** the reviewed `seed.json`, the entries `hrn knowledge inspect` returns, and
   the manifest's `corpus_digest` are all
   `3fb2fe351b62b1c94bc9c190579b43a8ffcfa057ada54308f83d9679508fa4de`, computed by the plan's Python
   definition and by `hrn retrieval corpus-digest` on both files.
2. **Task file:** SHA-256 `438e83bbf6b6615ffd14c93f3d5ca0617583ba44324c65601293f0ab4aacc827`, as
   in the plan. It is published beside this report.
3. **`compare --runs`:** succeeded (30/30 pairs, 30/30 comparable) with `hrn` built from `main` at
   `056c21f`, which includes the fix for #867. At R it could not run: see "Deviations". The eval
   binary is a build of R (`hrn-version.txt`).
4. **Cache:** holds (above).

Each measured sample still reads `supplied_unverified`, so the tool's gate is `pending` until
maintainer decision 2 is taken.

## Deviations from the plan

- **Revision.** R is `11a50948fe9b644f9a4601d957a60f6e95a47bce` (#857's merge), not the newest
  `main` (`b6fe55e`). At `b6fe55e`, `docs/workspaces.md` takes the 11 guides past the 100-entry
  import limit; `11a5094` is the newest `main` commit where they fit (99 entries), and it holds #847
  and #843. Every task's answer was checked at both commits and is unchanged.
- **Import refusals (#825).** The same 7 as in step 0: identifiers `.env` (4 times), `.env.example`
  and `.ENV`, and the entry `docs/backends.md > … > What a run loads`. 98 entries were imported.
- **Step 0 at R.** Answer's guide in the packet 6 of 7, answer text 5 of 7, no-answer tasks empty
  0 of 3: a no-go by the plan's rule, as before. The maintainer chose to run anyway. One of the ten
  searches returned HTTP 500 once and succeeded on the retry; nothing in the service or database
  log explains it.
- **`compare` could not run at R (#867).** Every real eval cell failed `batch and run result
  differ`: the run record gains `end_head`, changed files and commits when it closes, and batch
  appends gate evidence to its copy. Fixed in PR #870; the plan allows a later `hrn` for
  `samples` and `compare`.
- **The plan's samples command** read the eval records without UTF-8, which failed on the B evals
  (the `absent-command` pattern holds `’`). It now passes `-X utf8` and `encoding='utf-8'`.

## Records

Evals (local, under the pilot's own `HRN_HOME`): A0 `d2468`, B0 `3796b`, A1 `b5fa4`, B1 `382b1`,
A2 `5a0d2`, B2 `6e3f1`. Every run ID is in `samples.json`. Beside this report in
[`retrieval-pilot/`](retrieval-pilot/): the task file, the manifest, the samples, the comparison
(`compare.json`) and the per-cell table (`cells.json`). The pilot used about 1% of the codex plan's
weekly allowance.

## What follows

- **Increment 6 (pgvector)** must beat this keyword baseline by at least 10% lower median tokens at
  equal success on these tasks: a `text` median of 54,861 tokens, so at most 49,375.
- **Where it loses:** a packet with no answer costs about 7%. A score floor that still admits
  related entries for in-domain questions (step 0's open condition) is the place to gain.
- **Scope of the claim:** 10 short factual questions about this repository, one model, three
  repeats. It shows that a packet holding the answer saves most of a lookup's tokens, and that a
  packet without it costs a little; how often the packet holds the answer (here 5 of 10 tasks, plus
  one with the right guide but not the line) decides the net saving on other task sets. It says
  nothing yet about coding tasks.
- **`absent-command`'s pattern** misses a correct phrasing. A later pilot should replace it before
  its runs, as the plan requires.
