# Experience

[← README](../README.md)

Status: design. Nothing here is built yet; the slices are listed at the end.

The person earns experience by working with agents through hrn, from real outcomes only,
never from keystrokes, turns or chatter. Ada has her own growth, from what she has learned,
shown apart. Everything is local to the machine, derived from records hrn already keeps, and
can be turned off with one switch.

## Two tracks, one level

- **Delivery**: work that landed and held. Merged pull requests, reviews without a blocking
  finding, failed gates that later passed, runs inside their ceiling, routed runs that
  passed, kept scripts reused, and the first use of each hrn feature.
- **Practice**: one short exercise a day on the person's own code, most of which agents
  wrote. It keeps the person able to read and judge that code, which is what the review
  before every merge depends on.

Both add to one experience total and one level. The streak counts practice only.

## Delivery events

Each event has a source key, which makes it count once however often the ledger is rebuilt.

| Event | Counted when | Source key | XP | Cap |
| --- | --- | --- | --- | --- |
| Pull request merged | GitHub reports it merged in a repository hrn has worked in (a run's or batch's directory is inside it), with every check passing on the merge head | `pr:<owner>/<repo>#<n>` | 50 | 4 a day |
| Review without a blocking finding | a run whose `task.purpose` is `review` ends `ok`, and its final answer marks no finding `blocking` | `run:<id>` | 20 | 4 a day |
| Bug squashed | a batch task fails a gate on one attempt and passes every gate on a later attempt | `batch:<id>/<task>` | 15 | 3 a day |
| Run inside its ceiling | a run with `timeout` or `max_cost_usd` in `task.json` ends `ok` | `ceiling:<id>` | 5 | 10 a day |
| Routed run passed | a run with `routed_by` ends `ok`; in a batch, its task also passes its gates | `routed:<id>` | 10 | 3 a day |
| Kept script reused | a shell call in a run's `events.jsonl` runs a file under `scripts/` that carries the kept-script header | `script:<id>/<name>` | 5 | 5 a day |
| First use | the first eval, batch, routed run, `hrn ws` slot, remote job, or `hrn flow` call | `first:<feature>` | 25 | once each |

Only the merged-PR event reads GitHub (`gh pr list --state merged` per repository); `hrn gc`
checks merges but keeps no record of them. The repositories asked about are the ones the
run and batch records name. A pull request that a later merged pull request reverts (by
GitHub's revert title or a `git revert` of its merge commit) gets a correction event, key
`revert:<owner>/<repo>#<n>`, that cancels its 50. Nothing else takes experience away: a
run over its ceiling costs nothing, since a penalty there would reward hiding it.

Reading the review verdict from the final answer depends on the reviewer marking blocking
findings with the word **blocking**, as the house review format does. A structured verdict
in the record would be sturdier; until there is one, a review whose answer cannot be read
earns nothing rather than a guess.

## Practice

One exercise a day, from the person's own records:

| Kind | Material | The person writes | Shown after the answer is saved |
| --- | --- | --- | --- |
| Cold read | the diff of a pull request merged in a repository hrn has worked in, without its title, body or review | what it does and what could break | the title, body, review findings and fix commits |
| Find the defect | the diff at the head a reviewer reviewed, for a pull request whose review named a defect that a later commit fixed | what is wrong | the finding and the fix commit |
| Read before merging | the diff of an open pull request the person is about to merge | what it does and what could break | the title, body and the reviews so far |
| Recall | a decision recorded in a repository's decisions log, by its heading only | the decision and its reason | the recorded text |

The rules:

- **One exercise a day**, plus up to three reads before merging (one per pull request). The
  day's material is picked by a seed of the date and an attempt number (FNV-1a), so a reload
  never changes it. The person may swap it before answering; the attempt number goes up.
- **The answer is hidden server-side.** The record holds what is shown afterwards, but no
  response carries it until the answer is saved. A user interface cannot show it early.
- **No grading by a model.** After the reveal the person marks *got it*, *partly* or
  *missed*. That steers what comes next (a missed area comes back sooner) and never changes
  experience. Talking the answer over with Ada is optional and runs on a plan backend,
  never the api backend.
- **Masked.** Diffs can hold secrets, so all material goes through the same masking as the
  pages of hrn watch.
- **Exposure is not authorship.** Agents can commit as the person, with no attribution
  lines, so git alone cannot tell the person's own work from an agent's. A
  file counts as agent-touched when an hrn run or batch changed it; the person's exposure
  is the exercises done, the reviews read and the commits made outside hrn runs. Until that
  cross-check exists, practice does not show per-area familiarity.

Daily practice done gives 30 XP (one a day). A read before merging gives 20 when that pull
request is merged afterwards (one per pull request, three a day).

### Streak

Consecutive days with the day's exercise answered, ending today or yesterday, so a streak
is not lost before the day is over. The display shows the last 35 days as filled or empty
squares. Merges never count towards a streak: that would push towards merging something
every day, against testing before merging. Delivery shows as weekly numbers instead.

## Levels and badges

Level is `floor(sqrt(xp / 50))`: level 1 at 50, level 5 at 1,250, level 10 at 5,000.

Badges are kept for good once earned:

| Badge | Earned by |
| --- | --- |
| First merge | the first merged-PR event |
| Full crew | in one day, a run on every enabled plan backend, all ending `ok` |
| Budget keeper | 20 runs inside their ceiling |
| Cheap and right | 10 routed runs passed |
| Veteran | level 10 |
| Cold reader | 10 cold reads |
| Defect hunter | 10 find-the-defect exercises |
| Fresh eyes | 10 reads before merging |

## Ada's growth

Ada's level is about what she has learned, not what the person did, and is shown on her own
card. She earns from:

- a change to the agent setup (instructions, rules, skills) admitted by `hrn eval setup`,
  read from its `setup_comparison` verdict;
- a lesson from finished runs admitted the same way, once run lessons go through that check;
- a kept script added to a repository's `scripts/`;
- a routing row set from an eval (`routed_by.source` is `eval`).

A rule nobody measured earns her nothing.

## The ledger

`~/.hrn/game/` (or `$HRN_HOME/game/`):

- `events.jsonl`: one event per line, appended, never edited:
  `{"key": "pr:example/app#12", "kind": "pr_merged", "track": "delivery", "xp": 50, "at": "2026-11-02T14:05:00Z", "repo": "example/app"}`.
  `track` is `delivery`, `practice` or `ada`. A correction carries `"cancels": "<key>"`.
- `reps/<date>.json`: the day's exercise, the attempt number, the material, what is shown
  afterwards, the answer, the self-mark and when it was answered.
- `settings.json`: `enabled` (the game switch) and the voice preferences for game lines.

The rules that make it rebuildable and safe to prune:

- **Idempotent by key.** Rebuilding from the records adds only events whose key is missing.
  It never counts one twice and never removes one whose record is gone.
- **Caps at read time.** Caps are applied when the ledger is summed, per kind and day in the
  order of `at`, then `key`. Writing never depends on order, so a rebuild gives the same
  totals.
- **Folded before pruning.** `hrn runs prune` reads the runs, batches and evals it is about
  to delete into the ledger first. If that fails it deletes nothing, as `hrn gc` keeps a
  worktree when in doubt.
- **The switch hides, it does not stop.** With `enabled` false nothing shows anywhere (the
  terminal, hrn watch, the Claude Code pane, Ada's voice, the Ada app), but the ledger
  keeps recording, so turning it back on loses nothing. Practice stays available without a
  score.

## Where it shows

- `hrn bot stats` in the terminal: level, experience by track this week, streak, badges,
  and Ada's growth on its own line.
- hrn watch: a card on the index page, on loopback and `--lan` only, never under `--public`.
  The answers to exercises are never served there.
- The Claude Code side pane: the streak and the day's exercise.
- The Ada desktop app: a practice card, the level, this week's numbers and Ada's card.
- Ada says it aloud when a level, a badge or the day's exercise lands, under her usual mute
  and snooze rules.

## Out of scope

Leaderboards, sharing, anything sent off the machine apart from the `gh` reads above,
experience for activity (turns, tokens, commands), and grading by a model.

## Slices

1. The ledger, its rebuild and fold-before-prune, the switch, and `hrn bot stats`.
2. Practice: the record, the seed, the four kinds, server-side reveal, masking.
3. The hrn watch card.
4. The Ada desktop app's cards (in its own repository).
5. The Claude Code side pane.
6. Agent-touched versus person-touched files, then familiarity per area.
