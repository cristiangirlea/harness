# Evaluations and reports

[← README](../README.md)

## Routing by evidence

`$HRN_HOME/route.json` (default `~/.hrn/route.json`) maps purpose categories to
`backend`, `model`, and `effort`. Each row also has `eval`, the source eval id
(absent for a hand-set row), and `set`, its date in `YYYY-MM-DD` form.
A missing file is an empty table; a malformed file is an error naming the file.
No initial table is supplied: the first evidence table follows a separately
approved hrn-evals run.

```sh
hrn route show
hrn route set implement -b codex -m sol --effort medium
hrn route from-eval <eval-id> --margin 0.05
```

`show` lists one row per purpose, sorted by category, or `(empty)`. `set` replaces
one row; any subset of backend, model and effort is accepted, with at least one
nonempty value. Backend names and effort levels use the usual run validation.
Purposes are review, debug, implement, refactor, test, research, plan, ops and unknown.
`from-eval` prints the proposed rows, then saves them, preserving existing rows
for purposes the eval did not measure. It reads saved results and calls no model.

For each purpose, task prompts are classified with the same rules as
`report --by purpose`. Pass rates sum that purpose's task results across repeats.
The margin is an absolute fraction from 0 to 1 (default 0.05): a best pass rate of
0.95 admits variants at 0.90 or above. Effort comes before model: first choose
the cheapest qualifying effort within each backend/model family, then compare
those winners by cost. Equal costs prefer plan-based claude or codex over api,
then lower effort, with variant names breaking remaining ties. Cost is the
variant's recorded median across the eval, since task rows do not store separate
costs. Variant choices inherit task and spec defaults as in the measured run;
evidence that mixes different choices within one purpose/variant is skipped.
The row carries the eval's id and date. Codex list-price estimates participate
in eval cost comparisons and routing through the same cost fields as api estimates.
Older records are not repriced.

`hrn run`, batch tasks (after inheriting spec options), and MCP `run` apply the
purpose's row only when backend, model **and** effort are all empty. The purpose is
the declared one (`--purpose`, the MCP `purpose` field, a batch task's `purpose`,
or a leading `Purpose: <category>` header), else the prompt's classified category.
Naming any of the three disables routing: routing never substitutes a named
model. Missing rows keep the existing backend default. An invalid row, whether it
names an unknown or disabled backend or an unknown effort, is skipped with a
warning on stderr and the run goes on unrouted; it never refuses the run.
Continued runs retain inherited choices, and the existing plan-limit switching
rules still apply. When a plan limit moves a routed run to the partner backend,
the routed model and effort are dropped and the run records no `routed_by`, so the
partner's results are not credited to the row. A batch attempt on any backend other
than the one its model and effort were chosen for (a limit switch or a retry) drops
them and `routed_by` the same way, whether they were routed or named.

The additive `routed_by` record field contains `category`, `declared` (true when
the purpose was declared rather than classified; omitted otherwise), `eval`
(absent for a manual row), and `set`. It is omitted for unrouted runs; older
records still load.
`hrn report --by routed` groups them as `<category> via <eval id>` or
`<category> via hand`, with unrouted runs under `-`, using the usual runs, ok,
and cost columns.

## Reports

`hrn report` reads every run and batch record and prints three tables: runs, their tokens, and batches.

```
runs by backend, since 2026-09-14 17:40 (7d)
backend  runs  ok  ok%  med dur  med turns  total cost  med cost  cache
claude      6   5  83%    1m2s          9      ~$1.03     $0.17    96%
api         3   3 100%     48s          6       $0.42     $0.14    71%
all         9   8  89%     55s          8      ~$1.45     $0.15    90%

tokens by backend, since 2026-09-14 17:40 (7d)
backend  input  output  cache read  cache write  med context  peak context  from
claude   41.2k  210.4k        9.8M       610.3k        96.1k        212.0k  turns
api       3.1k   40.2k      870.0k        52.0k        48.5k         91.3k  mixed
all      44.3k  250.6k       10.7M       662.3k        81.0k        212.0k  mixed

batches by repository, since 2026-09-14 17:40 (7d)
repository              batches  tasks  pass%  med attempts  total cost
~/src/example-app                1      3   100%             1      ~$1.03
all                           1      3   100%             1      ~$1.03
```

Runs group by backend, or by `--by model` (the model that answered), `--by dir` (the
workspace), `--by purpose` (what the run was for: review, debug, implement, refactor, test,
research, plan, ops or unknown) or `--by day` (the day the run started, on this computer's
calendar; days are listed in date order rather than largest first). The purpose is the one the run declared (`--purpose`, the MCP `purpose`
field, or a valid leading `Purpose: <category>` header); otherwise it comes from ordered keyword
rules on the prompt's first paragraph, or its second when the first matches no rule, with leading
`Label: value` lines skipped for an anchored instruction before trying the paragraph
as written, then with labels skipped as a fallback. A label's colon must be followed by a space or the
end of the line; URLs and Windows drive paths stay intact. Each lead is lowercased
from a bounded prefix and capped at 600 bytes without splitting a UTF-8 rune.
Since a brief's later paragraphs mention every keyword, the first rule
that matches wins, and a prompt no rule matches (a vague one) is `unknown`. Columns: runs, how many ended `ok`, median duration, median turns, total and
median cost, and the cache share (cache-read tokens over every input token, cache writes included;
`-` when the backend reported no token counts). A `~` marks a list-price estimate
computed by hrn. The api and codex estimates come from the Claude table in
`internal/agent/api.go` and the OpenAI table in `internal/agent/prices_openai.go`.
Every cost hrn shows should be treated as an estimate, never a bill; other CLIs
may supply their own cost figures rather than use either hrn table. Codex uses
standard short-context prices, without service-tier or regional adjustments.
It also shows `-` when Codex usage is marked missing; Codex's cache share divides
cache reads by input tokens that already include those reads.
Unknown costs are never summed as zero or included in the cost median. When no cost
is known, cost cells show `-`; otherwise the total counts runs without a known cost
(or batch tasks that ran without a known cost), for example `$1.23 (2 unknown)`. An explicitly
reported zero is known and shows `$0.00`. JSON rows include `runs_without_cost` or
`tasks_without_cost` when nonzero; older records without usage metadata retain a
known cost when it is positive.
Batches group by repository; their cost sums every attempt, passed or not. Runs without
a result (still running or killed), batches with a task still running, and records that
cannot be read are counted under the tables and left out of them.
The tokens table sums each group's input, output, cache-read and cache-write tokens from the
runs' results, and gives the median and peak context a model call carried (input including cache
reads and writes, counted once). Those come from a run's `usage` events when it has them (see
[Usage per turn](run-records.md#usage-per-turn)): every turn counts. A run without them (codex,
gemini, older records) counts once, as its average, its totals over its turns, and its peak
is the larger of that average and its recorded last-call context. `from` says which: `turns`,
`totals`, `mixed`, or `-` when no run in the group reported tokens. In a `mixed` row the
median is over one average a run (a run with `usage` events gives the mean of its turns), so
per-turn values and per-run averages are never counted together; the peak is still the
largest of any run.
`--since` takes a window such as `7d`, `36h` or `90m`; `--json` prints the same rows as
JSON for scripts.

## Measuring what shaping can touch

Run `bash scripts/tool-shapes.sh --home /path/to/.hrn --days 30 --json` from the
repository root. `--home` defaults to `$HRN_HOME`, then `~/.hrn`; `--days` defaults
to 30 (a rolling window of 24-hour days), and JSON is the only output form.
It reads finished runs, including unsuccessful ones, started in that window.
The record envelope's `started` timestamp selects the window; flat records fall
back to the local timestamp in their run directory name. Unfinished runs are
excluded; unreadable finished records in the window are counted and excluded.

`shapes` classifies UTF-8 tool-result bytes as `test`, `log`, `json`, `table`,
`terminal`, or `other`, in that priority order. Tests have `go test`, `ok`, `FAIL`,
`=== RUN`, or `--- PASS|FAIL|SKIP|BENCH` lines; unified diff headers do not match.
Logs have timestamp or level prefixes; JSON must parse; CSV/TSV
needs at least two rows with a consistent field count. Shell results otherwise
count as terminal output. For unnamed results, a pending shell call (or a batch
entirely of shell calls) establishes that origin; mixed pending calls remain
`other` when their origin is ambiguous. The `claude` tables use only Claude runs
with recorded tool calls.

Each shape reports its fraction of all result bytes and an **estimate**:
bytes / 4 divided by input tokens from the same runs. Each run uses its `usage`
event sum when present, otherwise its `result.json` totals, otherwise no token
figure. Input includes `cache_read_tokens` for api and Claude; Codex's
`input_tokens` already includes cache reads, so those are not added again.
Cache writes are excluded. A missing denominator is `null`. All runs contribute
to `bytes` and `tool_result_share`; only runs with a token figure contribute bytes
to input-share estimates, including inline-script estimates. `runs.with_tokens`
reports that coverage beside `runs.with_usage` and `runs.read`. Existing estimates
count bytes once. `input_token_share_replayed_estimate` instead multiplies each
result's bytes / 4 by the number of later model calls and divides their sum by
the same input-token denominator. Later calls are the usage events
after the result when any are present; otherwise result ordinal `i` (starting at
1) and `result.json` total turns `n` give `max(0, n - i)`. The fallback approximates
calls as one-to-one with tool results; batches and calls without results can
break that assumption. Missing turns give zero later calls. Replay assumes the
text remains in context through the run's end. These are candidate byte budgets, not
measured savings or a strict bound on total context tokens.

`inline_scripts` lists the top 20 normalised command groups by count, with the
count, distinct run count, command bytes and label (`heredoc`, `python`, `node`,
`perl`, `go`). It recognises `<<`, Python `-c`, Node/Perl `-e`, and `go run` of
temporary Go files in `tmp`/`temp` directories or common temporary-directory
variables. Quoted strings, paths, numbers and identifiers longer than 12
characters become placeholders; whitespace collapses. `command` and `cmd`
input fields are supported; shell calls without either use the event's `text`
(including Codex command events). Normalisation is a heuristic, not proof that two
scripts are interchangeable. Totals cover all groups, including those beyond
20; repeated bytes include every occurrence of groups seen more than once,
even within one run. Keeping a script still costs its invocation and result.
`inline_script_input_share_replayed_estimate` weights command bytes / 4 by
usage events after the command, or `max(0, n - i)` using its ordinal
among all tool calls when usage is absent, over the same token denominator.
`repeated_inline_script_input_share_replayed_estimate` applies that weighting
to every occurrence of groups seen more than once, including groups beyond 20.
The command fallback likewise approximates one tool call per model call.
Nothing from the records is quoted: no prompt, command, path, normalised form,
or output. The fixture home and Go tests exercise the measurement without model
calls. See [the local 30-day snapshot](tool-shapes.md) for the measured budgets.

## Evals

An eval is a batch with two more dimensions: a matrix of variants to compare and a
number of repeats. Every cell is an ordinary recorded run, scored by the same gates, so
the question "which backend, model and effort for this kind of task" gets an answer with
numbers behind it.

```json
{
  "parallel": 3, "mode": "yolo",
  "gate": ["go build ./... && go test ./..."],
  "matrix": [
    {"name": "opus-high", "backend": "api", "model": "claude-opus-5", "effort": "high"},
    {"name": "opus-low",  "backend": "api", "model": "claude-opus-5", "effort": "low"},
    {"name": "claude",    "backend": "claude"}
  ],
  "repeats": 3,
  "timeout": "20m", "max_cost_usd": 3,
  "tasks": [{"id": "vet", "prompt": "Run go vet and fix every finding."}]
}
```

A variant's `name` defaults to `backend[-model][-effort]`. Each cell becomes a batch
task with the id `<evalid>.<task>.<variant>.r<n>`, so branches from different evals never
collide. Retries are off for every cell on purpose: an eval measures what one attempt
does, and a retried attempt would score the harness rather than the backend. Everything
else, gates and mode and the budgets, `lean` and `compact_window`, carries through from the
spec; cells on claude are lean unless the spec or task says `"lean": false` (see
[Lean claude workers](backends.md#lean-claude-workers)).

A variant may set `"lean": true` or `"lean": false`, overriding the task and spec.
Omitting it preserves the task/spec setting and the claude batch default. A variant's
`lean` field is refused if any of its cells resolves to a backend other than `claude`.
Give variants distinct names to compare lean within one eval, for example:

```json
"matrix": [
  {"name": "lean", "backend": "claude", "lean": true},
  {"name": "full", "backend": "claude", "lean": false}
]
```

Variants may set `retrieval` (`off`, `optional` or `required`) and
`retrieval_version`. An eval record may include `retrieval_manifest` and
`retrieval_comparison`: held-out tasks, coverage, preparation and online overhead,
and a gate requiring equal success with at least 10% lower median model tokens
across complete comparable cells. Failed attempts count in per-success usage;
zero successes leave that ratio null. The bundled synthetic fixture tests the
accounting and establishes no real model savings.

`hrn eval eval.json` prints what each variant did and how each task scored under it:

```
  variant                pass  attempts  median cost median time   turns   tools   context
  sonnet                   0%         1        $0.15        13s       2       1    101061
  haiku                  100%         1        $0.05        14s       2       1     71814

  task                     sonnet      haiku
  greet                       0/1        1/1
── eval cb867 · batch 20260922-164017-1bbd7c · 2 cells · 14s ──
```

That run is real: asked for a file containing exactly `hrn rocks`, Sonnet wrote `hrn rocks.`
and failed the gate while Haiku passed at a third of the cost. The run records are still
there to say why, which is the point of keeping them.

`tools` is the median number of tool calls, counted the same way on every backend; a run
that takes eight greps to read one file shows up here and not in `turns`. Under a variant,
a dim line gives the run statuses when any cell did not finish `ok`, and how many passes
changed nothing: right for "find the bug, if there is one", suspicious for "fix the
typos".

The record goes to `~/.hrn/evals/<id>.json`; `--json` prints it, `hrn eval show <id>`
reprints it, `hrn eval list` finds it, `-v` streams every agent's tool calls. The worktrees are removed once the
record is safely written, unless `--keep`. A failing variant is data, not an error, so
the exit status is 0 whenever the eval itself ran.

Task ids and variant names take letters, digits, `_` and `-`: dots separate the parts of
a cell id. `retry` is refused in an eval spec, because a retried cell would score the
harness rather than the backend, and for the same reason an eval sets `no_switch`: a cell
whose backend reaches its usage limit ends `limited`. The `context` column counts prompt tokens and cache
reads together, which is what the model was given rather than what was billed at full
rate.

### The judge

Gates prove the build and `expect` proves an answer; neither can tell a typo fix from a
spelling change. A `judge` reads each passing cell's diff against a rubric and scores it
from 0 to 10:

```json
"judge": {
  "backend": "claude", "model": "haiku",
  "rubric": "Only real misspellings are fixed. British spellings such as colour stay.",
  "rubrics": {"notes": "The answer is correct and the repository is unchanged."}
}
```

`rubric` applies to every task and `rubrics` replaces it for the tasks it names; every
task needs one or the other, and both are checked before any agent starts. The judge is
shown the task, the rubric, the agent's final answer and the diff (the first 60 KB, and
the names of untracked files), and runs read-only in the cell's worktree so it can open
a file the diff only touches. `backend` defaults to the default backend, `timeout` to 5
minutes per verdict. Cells that failed are not judged: they already scored where it
counts, and judging them would only spend money.

The scores join the table as a `judge` column per variant (the mean) and after each
task's pass count (`1/1 10.0`). Every verdict is in the record under `judgements`, with
its reason and the run id of the judge's own recorded run; a judge that fails or answers
without a `{"score": ...}` object is recorded as an error rather than a zero, and the
footer counts those. On the `claude` backend the first verdicts cost more than the
answer: Claude Code writes its own system prompt to the cache in the read-only mode the
judge uses, about 64k tokens, once per judge that starts before the cache exists.

### Comparing evals

The records are kept so a change can be scored: run the same task set before and after
a new model, a reworded prompt or a harness change, then `hrn eval diff <before> <after>`.
Variants are matched by name and tasks by id; every cell reads before → after, and a
variant only one side ran gets its own row instead of disappearing. When a task's prompt
differs between the two specs, a note says so: often that is the change being measured,
but those rows do not compare like with like.

```
  eval cb867 → 847f6

  variant              pass             median cost        median time
  sonnet               0% → 100%        $0.15 → $0.14      13s → 13s
  haiku                100% → 100%      $0.05 → $0.05      14s → 16s

  task                 variant              passed
  greet                sonnet               0/1 → 1/1
  greet                haiku                1/1 → 1/1
  note: the prompt differs for task greet, so its rows compare different tasks
```

That is the greeting eval from above, run again with the target line fenced in
backticks. The first prompt ended "containing exactly the line: hx rocks. Do not run
git.", and Sonnet took the sentence's full stop as part of the line; with the text fenced
it passed. A judge score column appears when either eval was judged; `--json` prints the
pairs as data.
