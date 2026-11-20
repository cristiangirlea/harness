# Plugins: the plugin protocol, version 1

[← README](../README.md) · [Repository extraction](repository-extraction-spec.md)

A plugin is a program named `hrn-<name>` that a host starts as a child process and talks to over
JSON lines on stdin and stdout. This page is the wire contract. Its Go types and golden fixtures
are the `contracts/pluginproto` package of the nested module
`github.com/cristiangirlea/harness/contracts`, which any repository can import.

hrn does not start plugins yet: discovery, installation and the policy a plugin runs under come
with the plugin host. A plugin written against this page and the golden fixtures will work with
it.

## Framing

- stdin and stdout carry UTF-8 JSON, one object per line, ended by `\n`. stderr carries human
  diagnostics only and is not part of the protocol.
- A line is at most 4 MiB (4,194,304 bytes) without its newline, in both directions. The host
  repeats the limit in `hello.limits.max_line`.
- Every line has `type`. Every line the plugin writes also has `v` (always `1`) and `seq`, which
  is 1 on the plugin's first line and increases by exactly one on every line it writes, so a gap
  or a repeat means lines were lost or duplicated.
- Field names are case-sensitive. A reader ignores fields it does not know. An unknown `type` is
  an error: see [Compatibility](#compatibility).

## Sequence

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
  entries.
- `host.name` and `host.version` identify the host program. `limits` is optional.

### `ready` (plugin, first line, after reading `hello`)

```json
{"v":1,"seq":1,"type":"ready","protocol":"v1","plugin":{"name":"search","version":"0.1.0"},"capabilities":[{"name":"search","description":"Find an expression that fits examples."}]}
```

- `protocol` is the first entry of `hello.protocols` that the plugin speaks.
- `plugin.name` is the name in the executable `hrn-<name>`: 1 to 64 characters from `[a-z0-9-]`,
  starting with a letter. `plugin.version` is 1 to 64 characters from `[A-Za-z0-9._+-]`. The host
  program in `hello` follows the same two rules.
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
`bad_request`.

### `result` (plugin, last line of a call)

```json
{"v":1,"seq":3,"type":"result","id":"c1","status":"ok","output":{"expression":"x*y+1"},"evidence":[{"kind":"validation","source":"held-out examples","hash":"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08","excerpt":"12 of 12 held-out examples match"}]}
```

- `status` is `ok`, `failed` or `canceled`.
- `output` is optional and, when present, a JSON object owned by the capability.
- `error` is required when the status is `failed` and absent otherwise: an object with a `code`
  from the table below and a `message`.
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
flight. It may carry the `id` it is about. Only `unsupported_protocol` ends the process.

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
| 0 | stdin closed and nothing was in flight, or the last call was answered. |
| 1 | The plugin failed for a reason of its own. |
| 2 | The handshake was refused (`unsupported_protocol`). |

A host treats the end of stdout before a call's `result` as that call having failed.

## Compatibility

Version 1 grows only by adding optional fields. A reader ignores fields it does not know, so an
older plugin works with a newer host and the other way round. A new line `type`, a new required
field, a changed meaning or a new error code is a new version: the host adds it to
`hello.protocols` and keeps offering `v1` for at least one release.

Within version 1 an unknown `type` is an error. A plugin answers a host line of unknown type
with `error` code `bad_request` and keeps reading. A host that reads a plugin line of unknown
type stops the plugin and fails the call in flight.

## Using the Go package

```go
import "github.com/cristiangirlea/harness/contracts/pluginproto"
```

The package holds the frame types, the constants above, `Negotiate` for the handshake, and
`ParseHostLine` and `ParsePluginLine`, which decode one line and apply every rule on this page.
It starts no process and reads no file: a plugin in any language needs only this page and the
fixtures under `contracts/pluginproto/testdata/v1`, one line per file, which a conforming
implementation reads and writes byte for byte.

The module is standard library only and has no dependency on hrn. hrn's own copy of the package
is `internal/pluginproto`; a test keeps the two byte-identical, so the root module needs no
`require`.
