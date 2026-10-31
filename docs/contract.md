# Loom machine contract (schema_version 1)

Consumers (for example nexus.lite) pin to this document. Everything here is derived from code; the
`file:line` citations are the source of truth if the two ever disagree.

## Versioning rules

- Three documents carry an integer `schema_version`: the `capabilities --json` object, the
  `TaskResult` / `EscalationPayload` JSON (`--json`, exit 3), and every trace JSONL event (`--trace`).
  All are `1` today. They are separate constants (`engine.SchemaVersion`, `trace.SchemaVersion`,
  `capabilitiesMap`) so they can move independently.
- Additive changes (new optional key, new event type, new `status` string) keep the version.
  Removing or renaming a key, or changing a type or meaning, bumps it.
- Consumers must ignore unknown keys and unknown trace event types `t`.
- A missing `schema_version` means a binary older than this document; treat it as `1`.

## 1. Exit codes

Constants: `pkg/policy/policy.go:15-22`. Names map: `cmd/loom/main.go:130` (`exitCodeNames`).

| Code | `exit_codes` key | Meaning |
|---|---|---|
| 0 | `ok` | Success |
| 1 | `task_failure` | Retryable task failure (provider error, max turns, tool-error strikes, ...) |
| 2 | `harness_or_policy_error` | Harness invariant or policy violation, bad flags |
| 3 | `escalation_required` | Model called `escalate`; `EscalationPayload` JSON on stdout |
| 4 | `budget_exhausted` | Token, deadline or tool-call budget exhausted |
| 130 | `interrupted` | SIGINT / SIGTERM |

## 2. `loom capabilities --json`

Built by `capabilitiesMap()` (`cmd/loom/main.go:160`). Always a JSON object.

| Key | Type | Notes |
|---|---|---|
| `schema_version` | int | `1` |
| `version` | string | Same ldflags value as `loom version` (`cmd/loom/version.go:11`); `"dev"` when unstamped |
| `commit` | string | Same ldflags value as `loom version`; `"unknown"` when unstamped |
| `harness` | string | `"loom"` |
| `spec_version` | string | `"0.1"` |
| `invariants` | string[] | `I1`..`I4` |
| `builtin_tools` | string[] | Built-in tool names |
| `providers` | string[] | Accepted `--provider` values |
| `stm_integration`, `agents_md_autoload` | bool | |
| `exit_codes` | object | name -> int, table above |

Pinned by `cmd/loom/capabilities_test.go`.

## 3. TaskResult JSON (`--json`)

Struct: `pkg/engine/engine.go:120` (`TaskResult`), printed by `runAndReport` (`cmd/loom/main.go`, `res` marshalled
when `--json`). Exit code of the process equals `exit_code`.

| Key | Type | Notes |
|---|---|---|
| `schema_version` | int | `1` |
| `exit_code` | int | Table in section 1 |
| `status` | string | Open set: `success`, `max_turns_exhausted`, `budget_exhausted`, `interrupted`, `escalation_required`, `provider_error`, `auth_error`, `model_stop_error`, `unknown_stop_reason`, `max_tokens`, `tool_error_strikes`, `policy_violation`, `prefix_violation`, `harness_error`, `paused`. Key off `exit_code`; treat unknown statuses as opaque. |
| `output` | string | Final assistant text or error message |
| `turns` | int | |
| `prefix_hash` | string | |
| `compactions` | int | |
| `escalation` | object, optional | `EscalationPayload`; present when `exit_code` is 3 |
| `run_id` | string, optional | |
| `usage` | object | `prompt_tokens`, `completion_tokens`, `total_tokens` (ints), `pkg/engine/engine.go:134` |

On exit 3 the process prints the `EscalationPayload` itself (not the full `TaskResult`) to stdout, with or
without `--json` (`cmd/loom/main.go`, `runAndReport`). Struct: `pkg/engine/engine.go:103`. Keys:
`schema_version`, `t` (`"escalation_required"`), `reason`, `attempts`, `last_error`, `patch`, `branch`
(the last three optional), `cloud_eligible`, `task_slice` (`seed_symbols`, `context_files`,
`minimal_context`, `attempted`, `artifact_path`; `pkg/engine/engine.go:94`).

Pinned by `TestSchemaVersionOnResultAndTrace` (`pkg/engine/stopreason_test.go`).

## 4. Trace JSONL (`--trace <file>`)

One JSON object per line, file mode 0600, append-only. `pkg/trace/trace.go` (`Emit`) stamps
`schema_version` on every event. `t` is the event type. Emit sites are in `pkg/engine/engine.go`.

| `t` | Keys besides `t` and `schema_version` |
|---|---|
| `run_start` | `run_id`, `task_id`, `agent`, `daemon`, `frozen_prefix_hash`, `cloud_eligible` |
| `turn_start` | `turn`, `prompt_tokens`, `compacted_this_turn`, `frozen_prefix_hash`, `rendered_prefix_hash` |
| `stop` | `reason`, `raw_reason`, `provider`, `prompt_eval_count`, `prompt_tokens`, `output_tokens` |
| `tool_call` | `name`, `kind`, `args_hash` |
| `tool_result` | `name`, `status` + `consecutive` (tool unavailable) or `bytes_raw`, `bytes_passed`, `truncated` |
| `escalation_required` | `reason`, `attempts`, `cloud_eligible` |
| `checkpoint` | `turn`, `run_id` |
| `done` | `status`, `exit_code`, `turns`, `wall_ms`, `run_id`, `usage`, optional `reason`, `error` |

Exactly one `done` event is written on every exit path. Argument values are never traced, only `args_hash`.

## 5. Snapshot publish procedure (loom#15)

Documentation only; no script in this repo pushes anything. The public snapshot is a squashed single
commit plus a tag, published by the owner:

1. Start from a clean checkout of `main` with CI green.
2. Create an orphan branch, `git add -A`, and make one commit (`loom <version> snapshot`).
3. Tag `<version>` on that commit. Build the four binaries with
   `-ldflags "-X main.version=<version> -X main.commit=<sha>"` and write `sha256sums`.
4. Review the diff for secrets and internal hostnames before the push.
5. The owner pushes the branch and tag to the public remote and uploads the binaries to the release.

Tagging and release are an owner decision and are not automated here.
