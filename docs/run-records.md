# Run records

[← README](../README.md)

## Run records

Records hold the original text; human-facing displays mask secret-looking values, while JSON record output remains unchanged.

Batch record saves retry permission errors during rename with growing pauses for up to
two seconds. On Windows, sharing and byte-range lock violations are retried too.
Other rename errors return immediately.

Batch gate output is stored as UTF-8 after decoding detected UTF-16 shell output.
A gate whose shell cannot start or be waited for includes an optional `infrastructure_error`
reason; the task fails with that reason without retrying. Older gate records
without the field still load. See [Batches](batches.md) for the launcher errors
that count as infrastructure failures.

A batch attempt carries `did_not_start` when a backend or gate could not start
before any turn or tool call. Its value is the first line of the recorded error;
it is omitted otherwise. Older attempts without the field keep their previous
classification. Eval variant and task rows count these cells in the optional
`did_not_start` integer, separately from `did_not_act`.

A batch attempt with `protect` globs carries an optional `protected` list: the
protected files it changed or removed against the batch's base, as slash paths.
Under `protect_mode: fail` the attempt also has a failed gate named `protect ...`, marked
`guard: true` so a user gate with a similar command is never taken for it.
Records without the field still load. See [Batches](batches.md#batches).

A batch attempt whose agent moved its branch or `HEAD` with git (a commit, reset or rename) carries
`agent_committed: true`, and an eval's variant row counts those cells in
`agent_committed`. Records without either field still load.

Eval records retain each variant's optional `lean` field in `spec.matrix`: `true`
or `false` when explicitly set, omitted otherwise. Older records without it still load.
Each `table.variants` row also holds `cells_without_cost`, the cells whose cost is
unknown (left out of the total and median), and `cost_counted`, set when the row was
counted that way. Older records have neither and show a zero median cost as `-`.
Rows may also hold `median_cost_usd_passed` (with `passed_cost_known`) and
`median_context_tokens_passed`, the medians over passing cells only, and
`did_not_act` (on variant and task rows), the cells among `attempts` or `total` that
ended `ok` in one turn with no tool call and no change; a
`retrieval_comparison` may hold `passing_pairs`, the passing-pair medians and
reduction, `baseline_median_movement` and `saving_threshold`. All are optional;
older records without them still load.
The candidate's record of `hrn eval setup` also holds `setup_comparison`: the source ref and
`source_commit` (where the branch left it), the branch head, the setup commit and the paths it
changed, the task ids (and the `held_out` share and `seed` that picked them), `unpaired` cells, each
side's eval id, cells that acted and passed, tokens on failures and per success, the
movements and threshold, the verdict and its reason. It is optional; other records lack it.
Variants may hold `prompt_prefix`, `append_system_prompt` and `no_skills`, and a judge
`repeats`. With repeats, `judgements` holds one verdict per passing cell and repeat, each
with its `repeat` number; variant and task rows add `judge_repeats`, `score_min` and
`score_max` (the lowest and highest mean of one repeat, over the cells scored in every
repeat). Older records have one verdict
per cell, no repeat number and no spread, and still load.

Routed runs include `routed_by` (`category`, `declared` when the purpose was declared, source `eval` when present, and `set` date) in `task.json` and `result.json`; runs without an applied route may include `route_note` explaining the choice, and old records still load.

Every run records `setup`, the agent setup it started with, read when the record is
created (an edit the agent makes later does not change it). It holds hashes and names,
never file contents:

- `backend`: whose files these are.
- `files`: each instruction file the backend loads, mapped to its sha256. Keys are
  workspace-relative, `~/` for a user-level file (`$CLAUDE_CONFIG_DIR/` or `$CODEX_HOME/`
  when that variable names a folder other than the default; a relative one is read from the
  workspace), and `../` per folder for a file above the workspace. api: `HRN.md`, `AGENTS.md`, `CLAUDE.md`. claude: `CLAUDE.md`, `CLAUDE.local.md`,
  `.claude/CLAUDE.md`, `.claude/settings.json` and `settings.local.json`, `.claude/rules/**.md`,
  each skill's `SKILL.md`; `CLAUDE.md` and `CLAUDE.local.md` in every folder above the
  workspace (the main checkout above a batch worktree, for one); `CLAUDE.md`,
  `settings.json`, `rules/**.md` and skills under `~/.claude` (or `$CLAUDE_CONFIG_DIR`); and
  the files those Markdown files import by `@path` inside the same tree or, for `@~/`, a
  Markdown file under the home folder, up to five hops.
  codex: `AGENTS.override.md` and `AGENTS.md`, also under `~/.codex` (or `$CODEX_HOME`).
  gemini: `GEMINI.md` and `~/.gemini/GEMINI.md`. opencode: `AGENTS.md`, `CLAUDE.md` and
  `~/.config/opencode/AGENTS.md`. agy: `AGENTS.md`, `GEMINI.md`, `.agent/rules/**.md` and
  `~/.gemini/GEMINI.md`. A file that cannot be read maps to `unreadable`. Linked rules
  folders and linked folders inside them are followed. A link that leads out of its tree
  is hashed when it reaches a file of the same kind (a `.md` or `.json` from a dotfiles
  checkout); any other maps to `outside` and is not hashed, since the hash of a guessable
  secret would confirm it.
- `mcp`, `plugins`, `skills`: the names of the run's MCP servers (its `mcp` configs, only
  the stdio ones on api, all of them on codex; for claude also the project's `.mcp.json` and the user-wide and
  this project's servers in `~/.claude.json` (or `$CLAUDE_CONFIG_DIR/.claude.json`), of
  which only the names are read, listed whether or not `lean` keeps them from starting), the plugins the claude settings turn on
  (local over project over user), and the claude skills present. claude.ai connectors are
  not visible locally and are not listed.
- `base_digest`: a sha256 over all of the above; `digest`: the same plus the harness
  options the backend applies (`append_system_prompt` on api and claude, `retrieval` with
  its version on every backend, `no_instructions` and `shape_tools` on api, `no_skills` on claude,
  `lean` on claude and codex, and `skills` on a lean claude run without `no_skills`). Options do not remove names or files: they count in `digest` only, so
  runs that differ only by those options share `base_digest`.
- `truncated`: set when the scan stopped at 500 files; files past the cap are not covered.

Two runs with the same files, servers, plugins, skills and options have the same `digest`
wherever their workspaces are, except on claude when the folders above them hold
`CLAUDE.md` files at another depth (their keys count the `../`). Records written before the
field have no `setup` and still load. Eval variant rows copy the `setup` of the first cell of
the spec's first task, named in `setup_task` (none when that run is gone); for a variant
with a `prompt_prefix`, its `digest` covers the prefix too. `setup_mixed: true` says repeats of one task started with different `base_digest`s
(tasks may set their own MCP configs, so different tasks may differ).

A follow-up names the run it continues in `continues`. One started with `--fresh` (MCP
`fresh`, exec `fresh`) also has `continue_route: "fresh_handover"`: it started a new
session with a hand-over instead of resuming or forking one. The field is omitted for
an ordinary follow-up, and old records without it still load.

`~/.hrn/runs/<id>/` (or `$HRN_HOME/runs`):

- `task.json` — prompt, dir, backend, and the run options by their flag names; when retrieval
  rewrote the prompt the run started with, `original_prompt` holds the task as typed, read back
  by a continuation and by `hrn retrieval compare`
  (`timeout` as a duration such as `20m0s`; records written before 0.3 hold it as
  `timeout_ns` and still load; `lean` is `true`, `false` when a spec or task turned it
  off, or absent when the run left it to the default, which is lean; `skills` is `true` when a
  lean claude run kept its skills, absent otherwise; `compact_window` is a number or `"auto"`;
  `append_system_prompt` and `no_skills` are omitted when unset; `issue` and `take_over` are
  the leased GitHub issue and whether the run took it over, omitted when unset, see
  [Issue leases](workspaces.md#issue-leases))
  and, for exec runs, the supervisor's `attempt_id`. Optional `task.purpose` stores
  the declared category from `--purpose`, MCP `purpose`, a batch task's `purpose`,
  or a valid leading `Purpose: <category>` header; a continuation without one stores
  the purpose it inherits from the earlier run (see
  [Declared purpose](commands.md#declared-purpose)). It is omitted when undeclared;
  old records without it still load and use the header or classifier for display
- `events.jsonl` — text, thinking, tool calls, tool results, info, errors and session ids (timestamped),
  one per line, appended while the run goes (`hrn show -f` and `hrn watch` follow it)
  and, where the backend reports them, each model call's `usage` and each `compaction`
  (see [Usage per turn](#usage-per-turn)). Codex's `Reconnecting... n/5` line is an `info`
  event: the CLI retries its dropped model stream itself, and the run's status is what the
  turn's end says. It is the run's error only when the stream ends on it (an `error`
  event with the notice is then written at the end, since the CLI exits clean, and the
  usage is `missing` as after `turn.failed`), or the turn then fails with no word of its
  own; an error before it stays the run's error (a usage limit then ends the run `limited`).
  When a CLI exits non-zero with the same text as the stream's last `error` event, that
  event is not written a second time
- `messages.json` — the api backend's message history, for `hrn run --continue`. It
  repeats the tool results the transcript already holds, so an api run costs roughly
  twice the disk of its transcript
- `originals/<handle>.txt` — with `--shape-tools`, the full text of a shell or MCP
  result over 30,000 bytes. The handle is the first 16 lowercase hex characters of
  its SHA-256; files are created on demand with mode `0644`.
  Originals are written and closed in temporary files inside `originals/` before
  being renamed to the handle; failed writes remove the temporary file.
  A checked summary or a cut after failed checks ends with
  `original: <handle> (<n> bytes); read_original reads it` whenever the original was saved. The `read_original`
  tool returns the whole original or one part of it: a byte range (`range: "a-b"`,
  zero-based, end exclusive), a line range (`lines: "a-b"`, 1-based, inclusive; a
  last line without a newline still counts), or, for an original that is one JSON
  value, an RFC 6901 pointer (`pointer: "/items/3/name"`, `~1` for `/` and `~0` for
  `~` in a key), which returns that value's own bytes with the whitespace removed,
  numbers spelt as they were. An empty `pointer` means the whole original, as the
  empty pointer does in RFC 6901. At most one of the three; a range or line range
  outside the original, or a pointer that does not resolve, is an error naming
  what was asked. Tool-result events and messages contain the summary the model
  saw. If checks fail or the original cannot be saved, the existing head-and-tail
  cut is used; without a run record directory, nothing is saved. File reads are
  not shaped. Old records without this directory or `shape_tools` still load.
  The store is bounded and refuses rather than evicts: a new original that would
  take one run's originals past `HRN_ORIGINALS_RUN_MAX` bytes (default 64 MiB), or
  all of `$HRN_HOME/runs/*/originals/` past `HRN_ORIGINALS_MAX` bytes (default
  1 GiB), is not saved, and that result gets the head-and-tail cut without a
  handle. An invalid value (not a whole number of bytes, or negative) leaves the
  default. Saved originals are never removed while their run exists, so a handle
  the model was given always reads; `hrn runs prune` removes them with their run.
  The machine total is measured once per run, at its first save, and then kept
  up to date in memory, so runs going at the same time do not see each other's
  saves and together may pass the cap a little. When the runs directory cannot be
  read, nothing new is saved until it can.
  A continuation that resumes an api conversation first copies that run's
  originals into its own `originals/`, so the handles in the conversation still
  read. hrn picks that run from the record, never from a path the model gives;
  only regular files named for their content's SHA-256 prefix are copied, never
  links. The run's events say how many were copied and how many were skipped (a
  link or other non-regular file, bytes that do not match the name, or a file
  larger than the run cap, or than 64 MiB when the cap is set lower). The resumed
  run holds its own predecessor's copies, so one hop covers the chain. Copies
  count toward the new run's cap but are never refused.
  Shell capture keeps the whole output up to 8 MiB, including on nonzero exit
  and timeout. Above that ceiling it uses the existing head-and-tail cut, says
  the original is unavailable, and saves no incomplete original.
  Capturing and shaping one output just under that ceiling peaks at about 75–90 MiB of
  heap beyond the input itself, with 270 MB allocated in total (`go test -bench
  NearCeiling ./internal/tools`, which reports `peak-heap-MiB`; Go 1.26, a 16-core desktop).
- `retrieval.json` — the opt-in packet, scope and version pin, saved before a backend
  starts. An unfinished run keeps this pin; a continuation checks it against the
  conversation before reuse.
- `handoff.md` — for a follow-up, the transcript of earlier turns its backend was
  given before the prompt (see [Continuing a run](backends.md#continuing-a-run)), or for
  a fresh hand-over the brief built from the earlier run's record
  ([A fresh hand-over](backends.md#a-fresh-hand-over))
- `result.json` — status (`ok|error|refused|max_turns|timeout|budget|canceled|limited`), final answer, turns,
  tokens, cache, cost (`cost_is_estimate` for the api backend), duration, the
  claude, codex or agy `session` id, and for a `limited` run `limit_resets`, when its plan's
  limit lifts

`result.json` includes `recovery_required: true` when a durable api operation needs
reconciliation before it can resume; the field is omitted otherwise.

For opted-in retrieval, `result.json` includes `retrieval` with its packet or
failure, and `messages.json` includes the conversation's retrieval pin. Older
records without these fields still load.

`result.json` includes `usage_metadata` when the adapter supplies it. Its
`input_semantics` is `inclusive_cache` (Codex and gemini input already includes cached
input) or `exclusive_cache` (api, Claude and opencode cache counters are separate). The api,
gemini and opencode adapters write it only to mark usage `missing`, when a turn's counter is
negative or would overflow the run's total; that turn is not added. Codex writes it on every
turn; usage and cost are `missing` from a turn that completes without usage, a `turn.failed`,
a stream that ends while reconnecting, or a negative or overflowing counter, even when no
earlier turn completed. `status` is `reported`, `estimated`, or `missing`; `cost_status` distinguishes a reported
zero from missing cost and may also be `estimated` or `missing`. Older results
without metadata still load; known backends infer token semantics from their
existing counters.

`result.json` also includes `context_tokens` when known: the last model call's input
tokens, including cache reads and writes, rather than the sum across the run. The
api backend records its last message's usage; claude records its main loop's last assistant
message's usage; opencode records the last step's usage. Codex's `turn.completed`
usage totals the model calls in its agent turn, and gemini's final stats total the
run, so neither supplies a last model call's context size. A backend with no usage
leaves it unset too. The field is omitted when zero; older records still load.

When a continuation carries a saved session or conversation with more than 150,000 tokens
of context, the new run records the fresh-run warning once as an `info` event on every
entry path (CLI, MCP and exec). It appears in `hrn show` and the run page like other info
events. Passing earlier turns only as a transcript does not produce this warning.

Beside `runs/`, `limits.json` holds the reset time of each backend whose usage limit is in
force. `hrn run`, the MCP `run` tool and batch attempts consult it before starting when
switching is enabled. `hrn exec` neither reads nor writes it. Deleting it forgets the
remembered limits.

`result.json` includes `model_reported: true` only when the backend reports its
model: api responses, claude init lines, gemini model stats and codex's session
file. A requested/default model alone does not set it; opencode does not report one.
The field is omitted when false, and older records without it still load. Exec uses
it to send an empty `resolved_model` when the backend did not report its model.

`codex exec --json` names neither the model nor the CLI version, so after the run hrn
reads the session's rollout file, `$CODEX_HOME/sessions/YYYY/MM/DD/rollout-*-<session>.jsonl`
(default `~/.codex`): the last `turn_context` model becomes `model` with
`model_reported: true` (and prices the cost estimate), and `session_meta.cli_version`
becomes `result.cli_version`. Without that file, or when its `session_meta` names
another session, the result keeps the requested model, unreported, and no
`cli_version`. hrn never reads `config.toml` for this: it says what codex would use,
not what it ran.

When complete events fail to write or `events.jsonl` fails to close, `result.json`
includes `record_error` with the number of events not recorded, the first event error
and any close error; `hrn show` and MCP `show` report it and recover intact events
after broken lines. The result is still saved to mark the run finished, the field is
omitted on success, and older records still load.

`hrn runs prune --older-than 30d` removes runs, batch records and eval records that
started before the cutoff, and prints what it freed; `--dry-run` lists them instead and
removes nothing, and `--keep N` spares the newest N runs whatever their age. It refuses to
run without `--older-than`. Records cite each other, so it works from the top down and
never breaks a reference: an eval it keeps keeps its batch and its judge's runs, a batch it keeps keeps every
run it made, a run it keeps keeps the run it `--continue`d from, and a batch whose
worktrees are still on disk (for a fresh task, one its record's `branch_made` says it made, or in a record without `ownership` one it did not fail on, before any attempt, because it was there already; for a task that ran in a slot, which outlives the batch, or
that was waiting for one: whose branch is still there and, its record's `branch_made` says, the task's own) stays with its runs, because `hrn batch rm` needs that record
to remove them (it says so, and names the command). A run with no result that wrote in
the last hour is taken to be still running and stays; a directory in `runs/` that is not
a run id is never touched. If any batch or eval record cannot be read, prune refuses:
that record may cite anything. Records go before the runs they cite, so a failure partway
never leaves a record pointing at nothing.

The one-hour activity rule lives in `internal/runlog/activity.go` and is shared by
prune and watch. It uses `events.jsonl`'s modification time, falling back to the run
directory when the transcript is absent. Watch displays an unfinished quiet run as
`stopped`; this adds no stored status or fields and leaves older records unchanged.
For an unfinished batch, watch considers the newest write to its record or any of
its referenced runs, including a run's result: an active first attempt keeps the
batch running even when the batch record itself is over an hour old, and a running
gate has the batch save its record again every ten minutes. See
[watch states](watch.md) for the display and Ada behaviour.

## Ownership and execution flow

Run records (`task.json`, `result.json`) and batch records carry an optional `owner`: who the
record belongs to, as `kind:value`. A local install writes `local:<OS user name>`, or
`$HRN_OWNER` when it holds a valid `local:`, `email:` or `user:` principal. Records written
before the field have none and belong to the local user, so nothing on disk is rewritten. Every
read that crosses an access boundary (the watch server, `hrn flow`) goes through one decision,
`owner.Viewer.CanReadRecord` (or `CanRead` without a directory): a record's owner reads
it, an admin grant reads all, and a project grant permits the named viewer to read
the named owner's records in that project. Anyone else is told the record does not
exist, so ids cannot be probed.

Project grants are a JSON list in `$HRN_HOME/grants.json` (default
`~/.hrn/grants.json`), for example:

```json
[{"viewer":"email:reader@example.com","owner":"local:me","project":"/absolute/path/to/repo"}]
```

`viewer` is an `email:` principal with a lowercase address or a `user:` principal.
`owner` is optional and defaults to the local owner; this also covers records written
before ownership. `project` is an absolute repository directory. A run's task
directory is resolved to its repository root by looking for `.git` in its parents
when available, falling back to the task directory. A batch uses `Result.Dir`.
The directory must equal the granted project or be below it after path cleaning,
with case ignored on Windows and a directory boundary required (`/repo2` is outside
`/repo`). Batch timelines check child runs independently; worktrees inside the
granted repository qualify, while runs outside it require their own access. The batch
record's own attempt summaries and gate output stay visible to anyone who may read
the batch.

hrn never writes the grants file. Public watch loads it at startup; absent means no
project grants, and malformed means no project grants plus one stderr error. Restart
watch after editing it. Local offline and LAN ownership behavior is unchanged.
Public record requests audit the requested id, decision and access basis in
`watch-access.log`; see [public watch](watch.md#behind-cloudflare-access).

A batch also appends its execution events to `batch-flows/<id>.jsonl`, one JSON object per
line. Each event has a version `v` (1), a sequence `seq` that continues when the file is reopened,
a time `t`, the `owner`, `batch`, `task`, `attempt` and `run` it concerns, a `step` id and its
`parent`, a `kind` (`batch`, `task`, `slot`, `attempt`, `route`, `decision`, `run`, `tool`, `gate`,
`commit`, `info`, `error`) and optionally a `status` (`started`, `waiting`, `ok`, `failed`,
`canceled`, `skipped`), `name`, `text`, `reason` and `next`. Later events for a step update it.
`reason` is only ever a recorded cause, such as a route row or a gate's exit; hrn never writes
model reasoning there. Text is masked for secrets and capped before it is written.

Step ids are fixed: `batch`, `task:<id>`, `task:<id>/slot`, `task:<id>/a<n>` for attempt n,
`task:<id>/a<n>/gate<k>`, and `<attempt>/run` for the attempt's run, whose tool calls are
`<attempt>/run/tool<n>`. A run outside a batch is `run:<id>`. Runs keep no flow file: their steps
are rebuilt from `events.jsonl` and the record (marked `derived`), and so is the flow of a batch
written before the file existed. A step whose parent is missing, such as a pruned run, is shown
as a gap rather than dropped. Readers skip a torn last line, duplicate sequence numbers and
versions newer than they know. Pruning a batch record removes its flow file too.
Prune also removes orphan `batch-flows/<id>.jsonl` files whose batch records are already
gone, regardless of age, and reports their count as `orphan_flows` (omitted when zero)
in `PruneResult` and in the command's text; `--dry-run` counts them without removing them.

## Usage per turn

A `usage` event in `events.jsonl` is one model call's tokens, written when the backend
reports it, so a run's context can be followed turn by turn:

```json
{"t":"...","kind":"usage","usage":{"model":"claude-opus-5","input_tokens":5,"output_tokens":40,
 "cache_read_tokens":12000,"cache_write_tokens":30000,"context_tokens":42005}}
```

`context_tokens` is what that call carried: its input plus cache reads and writes. Zero
fields are left out. Which backends write them:

- **api**: one per request, as the response reports it.
- **claude**: one per main-loop assistant message. stream-json writes each content block of a
  message as a line of its own and repeats the message's usage on each, so hrn writes the
  event once per message id, after the message's last line, with the first line's figures
  and the largest output count of its lines (the count grows as the message is written).
  A sub-agent's messages fill its own context and are left out; they still count in the
  run's totals.
- **opencode**: one per step (`step_finish`).
- **codex** and **gemini**: none. Codex's `turn.completed` totals every model call of an
  agent turn and gemini reports the run's totals, so neither says what one call carried.

A `compaction` event is a compaction the backend reported: Claude Code's `compact_boundary`
line (from `compact_window`, or its own threshold). `text` says what set it off (`auto` or
`manual`) and `usage.context_tokens` holds the context just before it. No other backend
reports one. `hrn watch` and `hrn show --usage` also mark a turn whose context fell by more
than half from the turn before as a *probable* compaction; that is a reading of the
numbers, never written to the record.

Runs recorded before these events existed have none; the result's totals still hold.
