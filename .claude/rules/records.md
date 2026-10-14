---
paths:
  - "internal/agent/agent.go"
  - "internal/agent/options.go"
  - "internal/runlog/**"
  - "internal/batch/**"
  - "internal/eval/**"
  - "internal/followup/**"
---
# Run, batch and eval records

Records under `$HRN_HOME` (`runs/`, `batches/`, `evals/`) outlive the code that wrote them.

- New fields on `agent.Task`, `agent.Result`, `batch.*` or eval records are additive and
  `omitempty`. Never rename or retype a stored field; when a format must change, keep reading the
  old one (`timeout_ns` in `agent.go` and `options.go` is the example).
- Records cite each other (an eval its batch, a batch its runs, a run the run it continues).
  Pruning works top down and removes a record before the runs it cites, so a failure partway never
  leaves a reference to nothing; keep that order.
- A change to what a record holds updates `docs/run-records.md`.
