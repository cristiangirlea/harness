# Plugins: the plugin protocol

[← README](../README.md) · [Repository extraction](repository-extraction-spec.md)

A plugin is a program named `hrn-<name>` that a host starts as a child process and talks to over
JSON lines on stdin and stdout. This page is the wire contract. Its Go types and golden fixtures
are the `contracts/pluginproto` package of the nested module
`github.com/cristiangirlea/harness/contracts`, which any repository can import.

## Framing

- stdin and stdout carry UTF-8 JSON, one object per line, ended by `\n`. A reader also accepts
  `\r\n`. stderr carries human diagnostics only and is not part of the protocol.
- A line is at most 4 MiB (4,194,304 bytes) without its line ending, in both directions. JSON
  whitespace around the object is allowed and counts toward the limit. The host may repeat the
  limit in `hello.limits.max_line`, whose only valid value in version 1 is `4194304`.
- Every line has `type`. Every line the plugin writes also has `v` (always `1`) and `seq`, which
  is 1 on the plugin's first line and increases by exactly one on every line it writes, so a gap
  or a repeat means lines were lost or duplicated.
- Field names are case-sensitive. A reader ignores fields it does not know. An unknown `type` is
  an error: see [Compatibility](#compatibility).
- A key appears at most once in any object the protocol reads, so two readers cannot disagree
  about a line. A line with a repeated key is refused. What is inside `input` and `output`
  belongs to the capability and is not read.
- A `null` value for an optional field means the field is absent. A required field is never
  `null`.

## Version 1 sequence

```text
host    -> hello
plugin  -> ready | error (then exit 2)
host    -> call
plugin  -> event, event, ...       (optional)
host    -> cancel                  (optional, while the call is in flight)
plugin  -> result                  (always, once per call)
host    closes stdin; plugin exits 0
```

One call is in flight at a time. The host sends the next `call` after the previous `result`; a
host that wants two calls at once starts two processes.

### `hello` (host, first line)

```json
{"type":"hello","protocols":["v1"],"host":{"name":"hrn","version":"0.2.0"},"limits":{"max_line":4194304}}
```

- `protocols` lists the protocol versions the host speaks, most preferred first. It has 1 to 16
  entries, each 1 to 16 characters from `[a-z0-9.-]`.
- `host.name` and `host.version` identify the host program. `limits` is optional.

### `ready` (plugin, first line, after reading `hello`)

```json
{"v":1,"seq":1,"type":"ready","protocol":"v1","plugin":{"name":"search","version":"0.1.0"},"capabilities":[{"name":"search","description":"Find an expression that fits examples."}]}
```

- `protocol` is the first entry of `hello.protocols` that the plugin speaks.
- `plugin.name` is the name in the executable `hrn-<name>`: 1 to 64 characters from `[a-z0-9-]`,
  starting with a letter. `plugin.version` is 1 to 64 characters from `[A-Za-z0-9._+-]`. The host
  program in `hello` follows the same two rules. hrn never treats `server`, `worker` or
  `fixture-model` as plugin names: programs of its own are called `hrn-server`, `hrn-worker` and
  `hrn-fixture-model`.
- `capabilities` has 1 to 64 entries. A capability's `name` follows the plugin name's rule and
  is unique in the list; its optional `description` is at most 256 bytes.

A plugin that speaks none of the offered versions writes one `error` line with code
`unsupported_protocol` and the versions it does speak in `protocols`, then exits with status 2.

### `call` (host)

```json
{"type":"call","id":"c1","capability":"search","input":{"examples":[[1,2,3]]},"timeout_ms":30000}
```

- `id` names the call: 1 to 128 characters from `[A-Za-z0-9._-]`. Every line about the call
  repeats it.
- `capability` is a name from `ready.capabilities`.
- `input` is optional. When present it is a JSON object whose meaning belongs to the capability;
  the protocol does not look inside it.
- `timeout_ms` is optional and positive: how long the host will wait before it cancels.

### `event` (plugin, during a call)

```json
{"v":1,"seq":2,"type":"event","id":"c1","kind":"progress","text":"tried 1200 candidates"}
```

- `kind` is `info` or `progress`. `text` is required, may be empty, and is at most 256 KiB
  (262,144 bytes).

### `cancel` (host)

```json
{"type":"cancel","id":"c1"}
```

The plugin stops the call and still writes its `result`, with status `canceled` if it stopped
early. A `cancel` for an `id` that is not in flight gets an `error` line with code
`bad_request` that carries that `id`.

A `cancel` can cross a `result` in the pipe: the plugin finishes, then reads a `cancel` for a
call that is over. It answers with that `error` line before it writes any line of the next call,
and the host ignores that one line.

### `result` (plugin, last line of a call)

```json
{"v":1,"seq":3,"type":"result","id":"c1","status":"ok","output":{"expression":"x*y+1"},"evidence":[{"kind":"validation","source":"held-out examples","hash":"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08","excerpt":"12 of 12 held-out examples match"}]}
```

- `status` is `ok`, `failed` or `canceled`.
- `output` is optional and, when present, a JSON object owned by the capability.
- `error` is required when the status is `failed` and absent otherwise: an object with a `code`
  from the table below and a non-empty `message`.
- `evidence` is optional, with at most 64 entries. Each has any of `kind`, `source`, `hash` (the
  SHA-256 of the content the entry stands for, 64 lowercase hexadecimal characters) and
  `excerpt` (at most 2,048 bytes). A plugin reports evidence; it does not certify itself, and
  the host decides what a result is worth.

```json
{"v":1,"seq":2,"type":"result","id":"c2","status":"failed","error":{"code":"unsupported","message":"no capability named rank"}}
```

### `error` (plugin, outside a call's result)

```json
{"v":1,"seq":1,"type":"error","code":"unsupported_protocol","message":"this plugin speaks v2 only","protocols":["v2"]}
```

An `error` line reports something that is not the outcome of a call: a refused handshake, a
line that could not be read, a `cancel` for an unknown call, a second `call` while one is in
flight. Its `message` is never empty, and it may carry the `id` it is about. `protocols`, when
present, follows the rule of `hello.protocols`. Only `unsupported_protocol` ends the process.

| Code | Meaning |
|---|---|
| `unsupported_protocol` | The plugin speaks none of the offered versions; `protocols` lists its own. |
| `bad_request` | A line was not valid JSON, was too long, or broke a rule on this page. |
| `unsupported` | The capability is not one the plugin offers. |
| `busy` | A `call` arrived while another was in flight. |
| `internal` | The plugin failed for a reason of its own. |

## Exit status

| Status | Meaning |
|---|---|
| 0 | stdin closed and nothing was in flight, or the last call was answered. For a channel, stdin closed and the plugin answered every delivery. |
| 1 | The plugin failed for a reason of its own. |
| 2 | The handshake was refused (`unsupported_protocol`). |

A host treats the end of stdout before a call's `result` as that call having failed.
If a channel's stdout ends before a delivery response, the delivery outcome is unknown.

## Version 2: channel plugins

A channel host such as Ada offers `["v2","v1"]`. A plugin that selects v2 declares its kind in
`ready`. The default kind, `capability`, uses the v1 call, event, cancel and result sequence.
A `channel` stays connected and can send messages before the host has sent it any work.

```json
{"type":"hello","protocols":["v2","v1"],"host":{"name":"ada","version":"1.0"}}
```

```json
{"v":1,"seq":1,"type":"ready","protocol":"v2","plugin":{"name":"echo","version":"1.0"},"capabilities":[],"kind":"channel","channel":{"name":"echo","label":"Echo chat"}}
```

For a channel, `channel.name` is a stable identifier of 1 to 64 characters from
`[a-z0-9-]`. Its optional display `label` is at most 128 bytes. A channel may omit
`capabilities` or send an empty array. Any entries follow the v1 capability rules.
A channel answers a `call` with an `error` whose code is `bad_request`.

```text
host    -> hello
plugin  -> ready | error (refusal)
plugin  -> inbound                 at any time after ready
host    -> ack                     after durable storage
host    -> deliver                 independently of inbound and ack
plugin  -> delivered | delivery_failed
plugin  -> status                  whenever the connection state changes
host    closes stdin
```

The envelope stays at `v: 1` even when the selected protocol is `v2`. The plugin uses one
sequence counter across all its frames, starting at 1 after each restart. Framing, line size,
field names, duplicate keys and optional nulls follow v1.

Channel IDs, names, labels, attachment MIME types and references, `status.text` and
`delivery_failed.error.message` reject C0 controls (U+0000–U+001F), DEL (U+007F)
and C1 controls (U+0080–U+009F). The same rule applies to `inbound.text` and
`deliver.text`, except that both allow tab, line feed and carriage return.

### Receiving and acknowledging messages

```json
{"v":1,"seq":2,"type":"inbound","id":"msg:17","time":"2026-01-15T10:30:00Z","sender":{"id":"user:3"},"chat":{"id":"room:8"},"text":"hello"}
```

```json
{"type":"ack","id":"msg:17"}
```

The service supplies the inbound `id`, the `sender.id` and the `chat.id`. Each is 1 to
128 bytes. `time` is an RFC 3339 timestamp. The optional sender `name` is at most 256 bytes.
The optional chat `type` is `private`, `group` or `channel`.

```json
{"v":1,"seq":2,"type":"inbound","id":"msg:18","time":"2026-01-15T10:30:00Z","sender":{"id":"user:3","name":"Jo"},"chat":{"id":"room:8","type":"group"},"text":"","reply_to":"msg:17","action":{"delivery_id":"d1","button_id":"ok"},"attachments":[{"name":"notes.txt","mime":"text/plain","size":0,"ref":"file:42"}]}
```

`text` is required and limited to 256 KiB. It may be empty for a button press, where
`action.delivery_id` and `action.button_id` identify the delivery and its button using
the ID rules below. Optional `reply_to` is a service message ID of 1 to 128 bytes.
A message may carry up to 16 attachments. Each has a `name` of 1 to 256 bytes, a `mime`
of 1 to 128 bytes and a `ref` that follows the service ID rule (1 to 128 bytes).
Its `size` is a non-negative integer in bytes.

**Delivery guarantee.** The host durably stores the inbound under the unique pair
`(channel.name, inbound.id)` before sending `ack` with that inbound ID. For a duplicate, the
host leaves the stored message intact and acknowledges it again. The plugin advances its
service read position only past acknowledged messages. An outstanding earlier message keeps
the position from advancing past it. After a restart the plugin sends every unacknowledged
inbound again with its original service ID, in order. If the host crashes before storing,
the replay supplies the message. If it crashes between storage and acknowledgement, the
unique key prevents a second insertion.

### Sending a delivery

```json
{"type":"deliver","id":"d1","chat_id":"room:8","text":"hello","reply_to":"msg:17","buttons":[{"id":"ok","label":"Confirm"}]}
```

A host assigns a delivery `id` of 1 to 128 characters from `[A-Za-z0-9._-]`. The
destination `chat_id` and optional `reply_to` follow the service ID rule.
`text` is required and at most 256 KiB. Up to eight optional buttons each have an `id`
of 1 to 64 characters from `[A-Za-z0-9._-]` and a `label` of 1 to 64 bytes.
Button IDs are unique within a delivery so an inbound action identifies exactly one button.

```json
{"v":1,"seq":4,"type":"delivered","id":"d1","message_id":"msg:19"}
```

```json
{"v":1,"seq":5,"type":"delivery_failed","id":"d2","error":{"code":"rate_limited","message":"try again later"},"retry_after_ms":0}
```

The plugin repeats the delivery ID in either response. On success, `message_id` is the
service's ID for the sent message. On failure, `error` carries a code and a non-empty
message. An optional non-negative integer `retry_after_ms` gives the delay in milliseconds
and may be zero. The codes `rate_limited` (service throttle) and `unauthorised`
(authorization failure) are available only in v2. A v2 capability plugin may use either
code in a failed `result` or an `error` line.

If the channel plugin exits or its stdout ends, every `deliver` still awaiting
`delivered` or `delivery_failed` has an unknown outcome. The host may retry it with
the same delivery ID. When the service lets the plugin recognize a previous send, the
plugin should avoid sending that delivery to the service twice. It must answer every
repeat with `delivered` or `delivery_failed`, even if it skips the service send.

### Reporting connection state

```json
{"v":1,"seq":6,"type":"status","state":"reconnecting","text":"waiting for service"}
```

`state` is one of `connected`, `reconnecting`, `unauthorised` or `stopped`. The optional
diagnostic `text` is at most 1,024 bytes.

## Compatibility

Version 1 grows only by adding optional fields. A reader ignores fields it does not know, so an
older plugin works with a newer host and the other way round. A new line `type`, a new required
field, a changed meaning or a new error code is a new version: the host adds it to
`hello.protocols` and keeps offering `v1` for at least one release.

Version 2 adds the channel kind and its frames. Hosts that support v2 keep offering v1 for
capability plugins. A v1 host refuses a v1 `ready` carrying `kind: "channel"`.
hrn offers only v1 and directs a v2-only plugin to a channel host.

Within either version an unknown `type` is an error. A plugin answers a host line of unknown type
with `error` code `bad_request` and keeps reading. A host that reads a plugin line of unknown
type stops the plugin and fails the call in flight.

## Plugins in hrn

hrn looks for `$HRN_HOME/plugins/<name>/hrn-<name>` first, then in the absolute directories
on `PATH`. The executable has an `.exe` suffix on Windows. On Unix the file must be
executable. The names `server`, `worker` and `fixture-model` are reserved for hrn.
`hrn plugin install <name|owner/repo>[@tag] [--name NAME] [--json]` installs a binary from a
GitHub release. With a repository, hrn derives the plugin name from the repository name by
removing a leading `hrn-`. Use `--name` to specify another name. A short name needs a repository
in the effective [plugin settings](backends.md#plugin-settings). Use `@tag` to select a release.
Without it, hrn uses the latest release. The tag can contain up to 64 ASCII letters, digits,
dots, underscores, plus signs and hyphens, and cannot start with a dot.

Publish `hrn-<name>_<version>_<os>_<arch>` with an `.exe` suffix on Windows, plus a
`SHA256SUMS` file listing the binary's digest. The version in the asset name is the release tag
with a leading `v` removed. hrn verifies the checksum, stages the executable under
`$HRN_HOME/plugins/`, and starts it for a handshake that must report the requested name.
A failed download, checksum or handshake cleans up staging and leaves any previous
installation in place.

A successful install can replace an existing copy. It writes `install.json` beside the
executable with the source repository, release tag, asset name, SHA-256 digest and UTC
installation time. The command prints the installed release, executable path and capabilities
and creates no run record. With `--json`, stdout contains `name`, `version`, `tag`, `source`,
`path`, `sha256` and `capabilities`.

Installation can be disabled or restricted to approved repositories in the plugin settings.
You can also place a plugin executable in one of the usual search locations. The settings
govern installation only: hrn still runs a plugin it finds on `PATH` or one installed earlier.

`hrn plugin list` lists the executables it finds without starting them and shows the repository
for installed plugins with readable installation metadata. `hrn plugin info <name>`
starts the plugin to learn its version, protocol and capabilities, then closes it without
recording a run.

The child receives no arguments, starts in the current directory and gets a filtered
environment. hrn removes its own `HRN_` variables and adds the guard's environment
variables ([the guard for every backend](permissions.md#the-guard-for-every-backend)): variables
named as secrets are withheld, the gh, cloud, container and npm logins point at an empty folder,
and `git push` fails. The child still runs with the user's file access and network, so files
under the home folder, such as ssh keys, stay readable. The child has its own process tree and
starts without opening a console window.
The handshake has a 10-second limit. After sending `cancel`, hrn gives the plugin two
seconds to return a result. At close, hrn closes the input stream and gives the plugin
three seconds to exit.

`hrn plugin call <name> <capability>` accepts a JSON object via `--input` or `--input-file`.
Use `--input-file -` to read standard input. The whole `call` line, with the input compacted,
must fit in 4 MiB, so the input must be a little smaller than that. The call deadline defaults
to 10 minutes. Use `--timeout 0` to disable it. The command prints progress and the run ID on
standard error. Successful output goes to standard output. All three commands accept `--json`
for machine-readable output. In JSON printed by these commands, the control characters JSON
allows raw (U+007F to U+009F) and the bidirectional controls (U+061C, U+200E, U+200F,
U+202A to U+202E and U+2066 to U+2069) are written as `\u` escapes, so the data is unchanged
and a terminal cannot act on them.

After a successful handshake, `hrn plugin call` saves a [run record](run-records.md#plugin-calls)
with backend `plugin`. Plugin calls cannot be continued. A missing plugin, a refused handshake,
invalid or oversized input, or a capability the plugin does not offer stops the command with
no run recorded.

### The pinned plugin

The first plugin, `hrn-search` ([cristiangirlea/hrn-search](https://github.com/cristiangirlea/hrn-search)),
offers the capability `search`, a bounded expression synthesizer. `scripts/plugin-pin.sh` builds
this checkout's hrn, installs the pinned `hrn-search` release into a scratch `HRN_HOME` through a
`plugins.sources` short name and an allow list, calls `search` on a fixed task and checks the
verdict and that the run record names the installed executable's SHA-256. The script also pins
each platform's SHA-256: it compares the release's `SHA256SUMS` with the pin before hrn installs
(installing starts the plugin for its handshake) and the installed executable after, so a moved
tag or a replaced asset fails instead of running. hrn fetches the release a second time to
install it; an asset swapped between the two fetches runs once, for the handshake, before the
check of the installed executable fails. CI runs it on Linux and Windows, and so does
the check skill. A broken plugin release does not move the pin; the tag and the hashes change
only in a pull request.

## Using the Go package

```go
import "github.com/cristiangirlea/harness/contracts/pluginproto"
```

The package holds the frame types, the constants above, `Negotiate` for the handshake, and
`ParseHostLine` and `ParsePluginLine`, which decode one line and apply every rule on this page
that a single line can break. Rules that span lines are the caller's to check: `seq` starting at
1 and increasing by one, `ready.protocol` being a version the host offered, a `capability` from
`ready.capabilities`, one call in flight, and a `cancel` that names it.

Use `ParseHostLine` for the initial offer and `ParsePluginLine` for the handshake response.
After a v2 handshake, call `ParseHostLineV2(line, ready.Kind)` or
`ParsePluginLineV2(line, ready.Kind)` to apply the rules for the negotiated kind. The empty
Go string represents an omitted kind. The host tracks sequence continuity, delivery IDs
and durable inbound acknowledgements across lines.

The package starts no process and reads no file: a plugin in any language needs only this page
and the fixtures under `contracts/pluginproto/testdata/{v1,v2}`, one line per file, which a
conforming implementation reads and writes byte for byte.

The module is standard library only and has no dependency on hrn. hrn's own copy of the package
is `internal/pluginproto`; a test keeps the two byte-identical, so the root module needs no
`require`.
