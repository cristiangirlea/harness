# Capability kernel (experimental)

This is the first incremental step toward HRN as a domain-independent problem-solving engine (issues #555–#559).

The `internal/capability` package is a small, standard-library-only registry. It registers versioned capabilities with a domain, explicit side-effect labels, and a context-aware handler. A capability can be an algorithm, solver, tool, or adapter to a model. It is not necessarily an agent.

## Safety and boundaries

- The registry performs *in-process policy checks* on declared effects; it is **not a sandbox**. Untrusted implementations require OS isolation and independent verification.
- Callers must explicitly authorize every declared side effect. No authorization means pure operations only.
- Outputs and evidence are untrusted until independently verified.
- No implicit LLM calls, network access, persistent learning, or automatic promotion.
- Handlers are responsible for respecting context cancellation. An in-process handler cannot be forcibly terminated by the registry.

## Next steps

Wire the registry into the existing run pipeline behind a compatible adapter; add typed schemas, budget limits, structured provenance, external process isolation, routing, and independent verification. Keep existing backends unchanged until those contracts are tested.

Run `go test ./internal/capability` to test this foundation.

## Experimental CLI routing

`hrn capability list` shows registered deterministic capabilities.
`hrn capability run math.add 1 '{"a":12,"b":30}'` executes one directly.

Opt in to task matching with:

```sh
hrn run --prefer-capabilities "add 12 and 30"
```

The only supported natural-language grammar in this prototype is exactly
`add <signed-int64> and <signed-int64>` (case-insensitive, optional outer whitespace).
A recognized request executes without a model call; integer overflow fails
rather than falling through. Unrecognized requests use the existing agent
path. Explicit backend/model selections and continued runs also keep the
existing path. This is a narrow proof of concept, not a general natural
language understanding system.

Note: the capability fast path currently returns a CLI result directly; it
does not create a standard agent run record. Durable capability run records,
policy decisions and independent verification are follow-up work.
