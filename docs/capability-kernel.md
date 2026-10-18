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
