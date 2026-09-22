
- 2026-09-22: PR I done (#10). Built in a worktree (`E:\__Workdir\harness-i`, branch
  `i-guard`) because another session had the main checkout on PR E. Deviation: the refusal
  text does not say "rerun with --allow-dangerous". That sentence is read by the model,
  which cannot rerun anything and does have a shell, so it invited a retry or a recursive
  hx call; it now names the rule, says only the operator can lift it, and tells the model
  to explain rather than retry. Design note the spec left open: the guard needs per-run
  state, but `APIBackend.Tools` is built once in `NewAPIBackend` and shared by every run,
  so the switch rides on `tools.Workspace` (already the per-run policy object for path
  confinement) rather than in a tool closure. `pipe-to-shell` also matches `powershell`
  as written in the spec. The dirty check lists up to five paths so the message is
  actionable, and runs before any worktree is created.
