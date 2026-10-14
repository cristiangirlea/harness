# Run records

[← README](../README.md)

## Run records

Records hold the original text; human-facing displays mask secret-looking values, while JSON record output remains unchanged.

Routed runs include `routed_by` (`category`, source `eval` when present, and `set` date) in `task.json` and `result.json`; it is omitted otherwise, and old records still load.

`~/.hrn/runs/<id>/` (or `$HRN_HOME/runs`):

- `task.json` — prompt, dir, backend, and the run options by their flag names; when retrieval
  rewrote the prompt the run started with, `original_prompt` holds the task as typed, read back
  by a continuation and by `hrn retrieval compare`
  (`timeout` as a duration such as `20m0s`; records written before 0.3 hold it as
  `timeout_ns` and still load; `lean` is `true`, `false` when a spec or task turned it
  off, or absent; `compact_window` is a number or `"auto"`)
  and, for exec runs, the supervisor's `attempt_id`. Optional `task.purpose` stores
  the declared category from `--purpose`, MCP `purpose`, a batch task's `purpose`,
  or a valid leading `Purpose: <category>` header. It is omitted when undeclared;
  old records without it still load and use the header or classifier for display
- `events.jsonl` — text, thinking, tool calls, tool results, info, errors and session ids (timestamped),
  one per line, appended while the run goes (`hrn show -f` and `hrn watch` follow it)
  and, where the backend reports them, each model call's `usage` and each `compaction`
  (see [Usage per turn](#usage-per-turn))
- `messages.json` — the api backend's message history, for `hrn run --continue`. It
  repeats the tool results the transcript already holds, so an api run costs roughly
  twice the disk of its transcript
- `retrieval.json` — the opt-in packet, scope and version pin, saved before a backend
  starts. An unfinished run keeps this pin; a continuation checks it against the
  conversation before reuse.
- `handoff.md` — for a follow-up, the transcript of earlier turns its backend was
  given before the prompt (see [Continuing a run](backends.md#continuing-a-run))
- `result.json` — status (`ok|error|refused|max_turns|timeout|budget|canceled|limited`), final answer, turns,
  tokens, cache, cost (`cost_is_estimate` for the api backend), duration, the
  claude or codex `session` id, and for a `limited` run `limit_resets`, when its plan's
  limit lifts

`result.json` includes `recovery_required: true` when a durable api operation needs
reconciliation before it can resume; the field is omitted otherwise.

For opted-in retrieval, `result.json` includes `retrieval` with its packet or
failure, and `messages.json` includes the conversation's retrieval pin. Older
records without these fields still load.

`result.json` includes `usage_metadata` when the adapter supplies it. Its
`input_semantics` is `inclusive_cache` (Codex input already includes cached input)
or `exclusive_cache` (api and Claude cache counters are separate). `status` is
`reported`, `estimated`, or `missing`; `cost_status` distinguishes a reported
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

Beside `runs/`, `limits.json` holds the reset time of each backend whose usage limit is in
force, which every run consults before it starts. Deleting it forgets them.

`result.json` includes `model_reported: true` only when the backend reports its
model: api responses, claude init lines and gemini model stats. A requested/default
model alone does not set it; codex and opencode do not report one. The field is
omitted when false, and older records without it still load. Exec uses it to send
an empty `resolved_model` when the backend did not report its model.

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
  event once per message id, with the first line's figures (the output count there can be
  lower than the message's final one). A sub-agent's messages fill its own context and are
  left out; they still count in the run's totals.
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
