# `hrn exec`: the supervisor protocol, version 1

`hrn exec --protocol v1` runs one task for a supervising program and reports it as a stream of
JSON lines. It exists so a program such as Ada can start, follow, cancel and record a run without
parsing terminal output. People use `hrn run`; programs use this.

One process runs one task. The supervisor owns the process: when it goes away, the run is
cancelled.

## Framing

- stdin and stdout carry UTF-8 JSON, one object per line, ended by `\n`. stderr carries human
  diagnostics only and is not part of the protocol.
- An input line longer than 4 MiB is refused (`bad_request`). Before `start` it is a rejection;
  after `accepted` hrn discards through the next newline, reports a non-fatal error and keeps
  reading. An unterminated line at end of stdin still cancels the accepted run.
- Every output line has `v` (always `1`), `seq`, `type` and `time` (RFC 3339 with nanoseconds).
  `seq` is 1 on the first line and increases by exactly one on every line hrn writes, so a gap or
  a repeat means lines were lost or duplicated.
- An event's text longer than 256 KiB is cut to its first 256 KiB on a UTF-8 boundary and marked
  `"truncated": true`. An event's `input` larger than 256 KiB is omitted and also marks the event
  `"truncated": true`. The run record keeps the full text and input.
- Field names are case-sensitive. Unknown or duplicate fields in a caller's message are refused,
  so a misspelt field cannot be ignored.

## Sequence

```text
hrn       -> hello
caller    -> start
hrn       -> accepted | rejected (then exit 2)
hrn       -> event, event, ... (and error lines if something fails)
caller    -> cancel                (optional, at any time after start)
hrn       -> result                (always the last line after accepted; then exit 0)
```

### `hello` (hrn, first line, before reading stdin)

```json
{"v":1,"seq":1,"type":"hello","time":"...","protocol":"v1",
 "hrn":{"version":"0.2.0","revision":"<vcs revision or unknown>","go":"go1.26.4"},
 "pid":1234,
 "limits":{"max_input_line":4194304,"max_event_text":262144},
 "backends":[
   {"name":"codex","available":true,"detail":"<path or reason>",
    "modes":["readonly","auto","yolo"],
    "efforts":["low","medium","high","xhigh","max"],
    "sessions":"fork",
    "options":["model","mode","effort","timeout"],
    "approvals":false}
 ]}
```

- `revision` comes from the binary's build information; it is `unknown` when Go did not stamp
  one. A supervisor identifies the binary by its hash, not by this field alone.
- `modes` lists the modes the backend accepts in exec. `ask` is never listed in v1: nothing can
  answer an approval yet.
- `efforts` is empty for a backend that cannot apply a reasoning effort.
- `sessions` is `fork` (a continuation copies the native session), `resume` (it continues the
  same session in place), or `none`.
- `options` includes the required `mode` field and lists the optional task fields the backend
  honours. A task that sets any other optional field is rejected as `unsupported`. A field counts
  as set when it is present, even with a zero or empty value (`"max_turns":0`, `"mcp":[]`), so a
  caller leaves out what it does not mean to set.

### `start` (caller, exactly one, the first line)

```json
{"type":"start","attempt_id":"att-01J...","task":{
  "prompt":"...","dir":"C:\\work\\project","backend":"codex","mode":"auto",
  "model":"gpt-6.1-sol","effort":"high","timeout":"30m"}}
```

- `attempt_id` is the caller's identity for this attempt: 1 to 128 characters from
  `[A-Za-z0-9._-]`. hrn records it with the run and echoes it on every later line.
- `prompt`, `dir`, `backend` and `mode` are required. There is no default backend and no default
  mode: a supervisor states both.
- `continue_run_id` continues a finished run, as `hrn run --continue` does; `accepted` says how
  (`notes`).
  It is accepted for every backend independently of `options` and is not listed there.

### `accepted` or `rejected` (hrn)

```json
{"v":1,"seq":2,"type":"accepted","time":"...","attempt_id":"att-01J...","run_id":"20261011-...",
 "requested":{"backend":"codex","model":"gpt-6.1-sol","mode":"auto","effort":"high"},
 "continues":"","notes":[]}
```

```json
{"v":1,"seq":2,"type":"rejected","time":"...","attempt_id":"att-01J...",
 "error":{"code":"unsupported","message":"the gemini backend cannot apply an effort"}}
```

A rejected start creates no run record. Codes: `bad_request` (malformed line, missing or unknown
field or bad attempt id), `unsupported` (a mode, effort or option the backend cannot
honour), `unavailable` (the backend's CLI or credentials are not
found), `storage` (the run record could not be created). hrn exits 2 after `rejected`.

A second `start` after `accepted` receives a non-fatal `error` with code `bad_request`.
It creates no additional record, and the accepted run continues to its final `result`.

### `event` (hrn)

```json
{"v":1,"seq":7,"type":"event","time":"...","attempt_id":"att-01J...","run_id":"...",
 "event":{"t":"...","kind":"tool_call","tool":"shell","text":"go test ./...","truncated":false}}
```

`kind` is one of `text`, `thinking`, `tool_call`, `tool_result`, `info`, `error`, and `session`.
A `session` event carries the backend's native session id in `text`, as soon as the CLI reports
it, so a supervisor knows which session to resume even if the run is interrupted. Streaming
fragments are not sent; each event is complete.

### `cancel` (caller)

```json
{"type":"cancel","attempt_id":"att-01J..."}
```

The run is cancelled and its whole process tree stopped; the `result` that follows has status
`canceled`. A second cancel is ignored. A cancel with another attempt id is answered with a
non-fatal `error` (`bad_request`) and otherwise ignored. End of stdin after `start` counts as a
cancel: the supervisor is gone, so nothing owns the run any more. An interrupt signal does the
same.

### `error` (hrn)

```json
{"v":1,"seq":9,"type":"error","time":"...","attempt_id":"...","run_id":"...",
 "error":{"code":"storage","message":"3 events not recorded: write ...: no space left on device"},
 "fatal":true}
```

A `storage` error means the run record is failing. It is fatal: hrn cancels the run rather than
continue unrecorded, and the `result` follows. Non-fatal errors report a caller mistake that did
not stop the run.

### `result` (hrn, last line)

```json
{"v":1,"seq":42,"type":"result","time":"...","attempt_id":"...","run_id":"...",
 "result":{"backend":"codex","model":"gpt-6.1-sol","status":"ok","final":"...","turns":3,
           "input_tokens":0,"output_tokens":0,"duration_ns":0,"session":"..."},
 "requested_model":"gpt-6.1-sol","resolved_model":"","record_error":""}
```

`result` is the same `agent.Result` a run record holds. `requested_model` is the task's requested
model after applying the continuation plan, including any inherited model; `accepted.requested.model`
reports the same value. `resolved_model` is what the backend reported running (empty when it did not say).
`status` is one of `ok`, `error`, `refused`, `max_turns`, `timeout`, `budget`, `canceled` and
`limited`. `limited` means a claude or codex plan reached its usage limit; `limit_resets` says
when it lifts, when the CLI said so. exec does not carry a limited run on to another backend, as
`hrn run` does: the supervisor decides. A caller treats a status it does not know as not ok.
`record_error` is set when the run record is incomplete. hrn exits 0 after `result`, whatever the
task's own status: the status is in the line.

## Exit status

Missing `--protocol`, any value other than `v1`, or extra command-line arguments are a usage
error on stderr: hrn exits 2 without writing `hello` or any other protocol line.

| Code | Meaning |
|---|---|
| 0 | A `result` line was written. |
| 1 | Internal failure after `accepted` with no `result` possible, for example stdout closed. |
| 2 | `rejected`, or the command line was wrong. |

## Records

Every accepted start is recorded under `$HRN_HOME/runs` like `hrn run`, with the attempt id in the
task (`attempt_id`), so `hrn runs`, `hrn show` and `hrn watch` show exec runs too.

## Compatibility

Version 1 only grows by adding optional fields to hrn's lines and adding backends, modes, efforts
or options to `hello`. A caller must ignore fields it does not know in hrn's lines. Anything else
is a new version, negotiated with `--protocol`.
