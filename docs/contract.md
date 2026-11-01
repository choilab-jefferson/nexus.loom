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
| `tool_call` | `name`, `kind`, `args_hash`; `run_command` additionally `argv` (array of strings, redacted: secret-named `KEY=value` and `--token`/`--password`/`--api-key`-style flag values (`--f x` or `--f=x`) become `***`, `Bearer <tok>` becomes `Bearer ***`; capped at 64 elements, 256 bytes each, ~4 KiB total, a final `...[truncated]` element marks a cut; `args_hash` is still over the unredacted args) |
| `tool_result` | `name`, `status` + `consecutive` (tool unavailable) or `bytes_raw`, `bytes_passed`, `truncated`; `run_command` additionally `exit_code` (int; `-1` = killed by signal/timeout or not started), `timed_out` (bool) |
| `policy_violation_recovered` | `kind` (`allowlist` \| `eval_flag`), `tool`. Non-allowlisted argv0, shell wrapper or interpreter eval flag: returned to the model as a tool error and counted toward the tool-error strike limit (`tool_error_strikes`). Path escape, symlink write and PHI cloud refusal stay fatal (`policy_violation`, exit 2). |
| `escalation_required` | `reason`, `attempts`, `cloud_eligible` |
| `checkpoint` | `turn`, `run_id` |
| `done` | `status`, `exit_code`, `turns`, `wall_ms`, `run_id`, `usage`, optional `reason`, `error` |

Exactly one `done` event is written on every exit path. Argument values are never traced, only `args_hash` (exception: redacted `argv` of `run_command`).

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

## Tool output truncation and run_command result

- Tool output over `max_output_bytes` keeps ~1/3 head and ~2/3 tail (UTF-8 safe) joined by `\n[loom:truncated K bytes in middle]\n`; `bytes_passed` counts head+tail only. `run_command` buffers 1 MiB the same way (head + ring tail), so the end of long output survives.
- `run_command` results carry structured `exit_code`, `timed_out` and `truncated` (also in the `tool_result` trace event, additive keys). On timeout `timed_out=true` and `exit_code=-1`. The text `[exit_error: ...]` suffix remains for the model.

## Operator flags: env, output cap, allow-cmd (additive)

- `--pass-env NAME[,NAME|PREFIX*]` (repeatable; `run`, prompt mode, `resume`) and `constraints.pass_env` extend the `run_command` child env allowlist. Defaults now include `PYTHONPATH`, `PYTHONUNBUFFERED`, `PYTHONHASHSEED`, `CUDA_VISIBLE_DEVICES`, `CUDA_HOME`, `CUDA_*`, `HF_HOME`, `HF_HUB_CACHE`, `HF_DATASETS_CACHE`, `TRANSFORMERS_CACHE`, `OMP_NUM_THREADS`. Secret-looking names (`*TOKEN*`, `*SECRET*`, `*PASSWORD*`, `*_API_KEY`, ...; `policy.IsSecretEnvName`) are never matched by a prefix; only an exact name passes one, with a stderr warning and an `env_secret_passed` trace event (`name`).
- `--max-output-tokens N` (`run`, prompt mode, `resume`): precedence flag > `constraints.max_output_tokens` > env `LOOM_MAX_OUTPUT_TOKENS` > default 8192 (was 2048). On a `max_tokens` stop, status/exit are unchanged and `output` ends with a hint naming `--max-output-tokens` / `LOOM_MAX_OUTPUT_TOKENS`. No automatic retry.
- `loom run --allow-cmd "<prefix>"` (repeatable) is a UNION with `constraints.allow_commands`; the union is then intersected with the global/agent layers (`LOOM_GLOBAL_ALLOW_COMMANDS`, `LOOM_AGENT_ALLOW_COMMANDS`) as in prompt mode. Without the flag, behavior is unchanged.
