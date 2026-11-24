# Versioned JSON outputs

[← README](../README.md)

Each output has its own version. Within a version, the only change is to add optional
fields. Readers ignore fields they do not know. Any other change requires a new version.

| Command or file | Version | Top-level keys (including optional fields) |
|---|---|---|
| `hrn runs --json` | 1 | `version`, `runs` |
| `hrn show --json` | 1 | `version`, `record`, `events` |
| `hrn show --usage --json` | 1 | `version`, `turns` |
| `hrn batch --json`, `hrn batch show --json` | 1 | `version`, `id`, `owner`, `started`, `dir`, `base`, `parallel`, `tasks`, `duration_ns`, `ownership` |
| `hrn eval diff --json` | 1 | `version`, `a`, `b`, `variants`, `tasks`, `prompt_changed`, `variant_changed`, `gate_changed`, `task_options_changed`, `judge_changed`, `judged`, `criteria`, `setup_changed` |
| `hrn report --json` | 1 | `version`, `tool_stats`, `since`, `by`, `runs`, `runs_all`, `unfinished`, `batches`, `batches_all`, `running_batches`, `unreadable` |
| `hrn ws list --json` | 1 | `version`, `workspaces` |
| `hrn ws issues --json` | 1 | `version`, `issues` |
| `hrn retrieval fixture --json`, `hrn retrieval compare --json` | 1 | `version`, `token_savings_supported`, `baseline_median_tokens`, `candidate_median_tokens`, `median_token_reduction`, `cost_savings_supported`, `evidence_kind`, `expected_pairs`, `observed_pairs`, `comparable_pairs`, `baseline`, `candidate`, `median_paired_saving`, `passing_pairs`, `passing_baseline_median_tokens`, `passing_candidate_median_tokens`, `passing_median_token_reduction`, `baseline_median_movement`, `saving_threshold`, `gate`, `reason` |
| `hrn flow --json` | 1 | `version`, `issues`, `events`, `tree` |
| `hrn bot stats --json` | 1 | `version`, `enabled`, `level`, `xp`, `week_start`, `ada`, `week`, `streak`, `days`, `badges`, `exercise`, `familiarity`, `repo`, `top` |
| Run record (`task.json`, `result.json`) | 1 | `record_version`, `start_head`, `repo`, `branch`, `routed_by`, `route_note`, `id`, `owner`, `backend`, `started`, `task`, `result`, `continues`, `continue_route`, `record_error`, `setup`, `plugin` |

The workspace, issue and usage lists previously returned bare arrays. Readers that
also support older hrn binaries can recognize an array at the top level. Version 1
puts the array under the key listed above, including when it is empty.

## Outputs without a schema version

- `hrn run --json` writes an event stream, one JSON object per line.
- `hrn eval --json` prints the saved eval record. `hrn eval setup --json` prints the comparison between the two setups. Neither format defines a schema version yet.
- `hrn plugin list|info|call|install --json` uses `version` for the plugin's own version.
- `hrn knowledge propose -json` writes a review summary to stdout and the draft to the file selected with `-o`. The summary has no schema version yet.
- `hrn retrieval samples` and `hrn retrieval corpus-digest` produce inputs that hrn reads back for comparisons and corpus verification.
