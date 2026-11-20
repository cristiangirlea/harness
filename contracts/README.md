# contracts: wire types anyone can import

`github.com/cristiangirlea/harness/contracts` is a Go module of its own inside the hrn repository.
It holds the wire contracts between hrn and programs that live elsewhere: types, the rules that
make a line valid, and golden JSON. It has no behaviour, no dependency on hrn and nothing outside
the standard library, so a plugin or a host in another repository can import it.

```bash
go get github.com/cristiangirlea/harness/contracts
```

Until the module has a release tag, `go get` resolves it to a commit of the default branch.

## Packages

| Package | Contract | Guide |
|---|---|---|
| `pluginproto` | The plugin protocol, version 1: `hello` and `ready`, calls, events, results, errors. | [Plugins](../docs/plugins.md) |

## Compatibility

A protocol version grows only by adding optional fields, and a reader ignores fields it does not
know. Anything else is a new version, offered in `hello.protocols` next to the old one for at
least one release.

| Protocol | In this module since | hrn releases that speak it |
|---|---|---|
| plugin `v1` | the first commit of `pluginproto` | none yet: hrn has no plugin host |

The fixtures under `pluginproto/testdata/v1` are the contract in bytes: one line per file. Lines
in `host` and `plugin` are read and written byte for byte by a conforming implementation; lines
in `tolerated` must be accepted and need not be reproduced. A fixture is never edited within a
version. A change that would need one is a new version with its own directory.

## In this repository

hrn's own module takes no `require`, so it does not import this one. `internal/pluginproto` is a
byte-identical copy of `pluginproto`, and a test there fails when the two differ: change both in
the same commit. The architecture test keeps this module from importing anything else in the
repository and anything else from importing it.

Run its checks from this directory:

```bash
go vet ./...
```

```bash
go test -count=1 ./...
```

Tagging a release of this module (`contracts/vX.Y.Z`) is a maintainer's step.
