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

### Run locks (parallel runs)

`run`, prompt mode and `resume` take an advisory `flock(LOCK_EX|LOCK_NB)` on the `--workspace` directory and on the `--trace` file (when non-empty) for the whole run. If another live loom process holds either, loom prints one line to stderr (`loom: workspace <abs> is in use by another loom process (parallel runs need distinct --workspace and --trace)`, or `trace` likewise) and exits 2. The kernel releases locks on process death; no lock files are created. Unix only (build tag `unix`); a no-op on other platforms. Code: `cmd/loom/lock*.go`.

## 2. `loom capabilities --json`

Built by `capabilitiesMap()` (`cmd/loom/main.go:160`). Always a JSON object.

| Key | Type | Notes |
|---|---|---|
| `schema_version` | int | `1` |
| `version` | string | Same ldflags value as `loom version` (`cmd/loom/version.go:11`); `"dev"` when unstamped |
| `commit` | string | Same ldflags value as `loom version`; `"unknown"` when unstamped |
| `harness` | string | `"loom"` |
| `context_continuation` | object | `schema` (`loom.context.v1`), `flags`, `task_keys`, `stdio`, `broken_rule`. Present from v0.2.3. |
| `subcommands` | array | `run`, `resume`, `capabilities`, `models`, `version` (additive) |
| `default_models` | object | provider -> default model id (additive); `LOOM_MODEL` / `--model` override |
| `spec_version` | string | `"0.1"` |
| `invariants` | string[] | `I1`..`I4` |
| `builtin_tools` | string[] | Built-in tool names |
| `providers` | string[] | Accepted `--provider` values (`mock2` = a second mock for cross-provider tests) |
| `agents_md_autoload` | bool | |
| `exit_codes` | object | name -> int, table above |

Pinned by `cmd/loom/capabilities_test.go`.

## 3. TaskResult JSON (`--json`)

Struct: `pkg/engine/engine.go:120` (`TaskResult`), printed by `runAndReport` (`cmd/loom/main.go`, `res` marshalled
when `--json`). Exit code of the process equals `exit_code`.

| Key | Type | Notes |
|---|---|---|
| `schema_version` | int | `1` |
| `exit_code` | int | Table in section 1 |
| `status` | string | Open set: `success`, `max_turns_exhausted`, `budget_exhausted`, `interrupted`, `escalation_required`, `provider_error`, `auth_error`, `model_stop_error`, `unknown_stop_reason`, `max_tokens`, `tool_error_strikes`, `policy_violation`, `prefix_violation`, `model_not_found`, `harness_error`, `paused`. Key off `exit_code`; treat unknown statuses as opaque. |
| `output` | string | Final assistant text or error message |
| `turns` | int | |
| `prefix_hash` | string | |
| `compactions` | int | |
| `escalation` | object, optional | `EscalationPayload`; present when `exit_code` is 3 |
| `run_id` | string, optional | |
| `usage` | object | `prompt_tokens`, `completion_tokens`, `total_tokens` (ints), `pkg/engine/engine.go:134` |
| `user_turn` | int | 1-based user-turn index of this call within its context (1 for a fresh run). Additive. |
| `cumulative_usage` | object | Same shape as `usage`: prior user turns from `--context-in` plus this call. `usage` is THIS call only; budgets (`--max-total-tokens`, deadline, tool calls) apply per call. Additive. |
| `context_path` | string, optional | The `--context-out` file, when one was written (not set for `-`). |
| `files_changed` | object | `added`, `modified`, `deleted` (sorted, workspace-relative, slash paths; always arrays, empty when nothing changed) and `truncated` (bool). Additive; `schema_version` stays 1. See below. |

`files_changed`: computed by snapshotting path+size+mtime+mode of regular files and symlinks in the workspace at first start and at the end (`.git` skipped; symlinks are recorded themselves, never followed). A size/mtime/mode change = `modified`. Total entries across the three lists are capped at 1000, and the snapshot walk at 20000 files; `truncated` is true when either cap hit. The baseline is stored in the checkpoint (`<trace>.ckpt`, `checkpoint.baseline`), so `loom resume` reports changes since the original start. Old checkpoints without a baseline still resume; their baseline is a snapshot taken at resume time (earlier changes are not reported). Loom's own files are excluded when they resolve inside the workspace: the `--trace` file, `<trace>.ckpt`, `<trace>.lock`, and checkpoint temp files (`.ckpt-*`) in the trace directory.

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
| `stop` | exactly one per model turn: `turn` (1-based), `reason` (normalized, see below), `raw_reason`, `provider`, `prompt_eval_count`, `prompt_tokens`, `output_tokens`, `total_tokens` (this turn), `cumulative` (`prompt_tokens`, `output_tokens`, `total_tokens` after this turn), optional `cache_creation_tokens`, `cache_read_tokens` (this turn, only when >0) |
| `tool_call` | `name`, `kind`, `args_hash`; `run_command` additionally `argv` (array of strings, redacted: secret-named `KEY=value` and `--token`/`--password`/`--api-key`-style flag values (`--f x` or `--f=x`) become `***`, `Bearer <tok>` becomes `Bearer ***`; capped at 64 elements, 256 bytes each, ~4 KiB total, a final `...[truncated]` element marks a cut; `args_hash` is still over the unredacted args) |
| `tool_result` | `name`, `status` + `consecutive` (tool unavailable) or `bytes_raw`, `bytes_passed`, `truncated`; `run_command` additionally `exit_code` (int; `-1` = killed by signal/timeout or not started), `timed_out` (bool) |
| `policy_violation_recovered` | `kind` (`allowlist` \| `eval_flag`), `tool`. Non-allowlisted argv0, shell wrapper or interpreter eval flag: returned to the model as a tool error and counted toward the tool-error strike limit (`tool_error_strikes`). Path escape, symlink write and PHI cloud refusal stay fatal (`policy_violation`, exit 2). |
| `escalation_required` | `reason`, `attempts`, `cloud_eligible` |
| `checkpoint` | `turn`, `run_id` |
| `done` | `status`, `exit_code`, `turns`, `wall_ms`, `run_id`, `usage`, optional `reason`, `error` |

Normalized stop `reason` set (`pkg/providers/providers.go:RawStopKind`; tool calls present always yield `tool_use`):

| `reason` | Raw values (case-insensitive) |
|---|---|
| `end_turn` | OpenAI `stop`; Anthropic `end_turn`, `stop_sequence`; Gemini `STOP`; Ollama `stop`; `eos`, `completed`, `finish` |
| `tool_use` | OpenAI `tool_calls`/`function_call`; Anthropic `tool_use`; or any response with tool calls |
| `max_tokens` | OpenAI `length`; Anthropic `max_tokens`, `model_context_window_exceeded`; Gemini `MAX_TOKENS` |
| `paused` | Anthropic `pause_turn` |
| `error` | content filter / refusal / malformed: OpenAI `content_filter`; Anthropic `refusal`; Gemini `SAFETY`, `RECITATION`, `BLOCKLIST`, `PROHIBITED_CONTENT`, `SPII`, `MALFORMED_FUNCTION_CALL`, `OTHER` |
| `unknown` | anything else; `raw_reason` keeps the vendor string |

Usage: `prompt_tokens`/`output_tokens` are provider-reported. The sum of per-turn `total_tokens` equals the final `usage.total_tokens` (plus resumed usage on `--resume`). Cache tokens: Anthropic `cache_creation_input_tokens`/`cache_read_input_tokens` (reported separately from `prompt_tokens`), OpenAI `prompt_tokens_details.cached_tokens` and Gemini `cachedContentTokenCount` (both subsets of `prompt_tokens`) map to `cache_creation_tokens`/`cache_read_tokens`; final `usage` carries cumulative values, omitted when zero.

Exactly one `done` event is written on every exit path. Argument values are never traced, only `args_hash` (exception: redacted `argv` of `run_command`).

## Conversation context (`loom.context.v1`)

One loom call is one user turn: the prompt, all model turns and tool calls, up to the point the agent
would wait for the next prompt. The next prompt is the next call. The conversation is an explicit,
caller-editable JSON document, independent of the trace/checkpoint (those only serve `loom resume`).

- `--context-out <path|->` (spec `context_out`): after the run, write the updated context. `-` = stdout;
  then stdout holds ONLY the context and the result/`--json`/escalation output goes to stderr. A file is written
  atomically with mode 0600 and named in `TaskResult.context_path`. Written for every outcome that reached the
  engine (success or not), so a caller can continue after exit 1/3/4.
- `--context-in <path|->` (spec `context_in`; flags override spec): load a prior context (`-` = stdin), append the
  new `-p` instruction as the next user message, run one bounded turn-sequence. System prompt, tool schemas and
  AGENTS.md are NOT part of the context (they come from the current run, so a context moves between providers/models).
  `pkg/compact` runs on the whole history every call; folded tool results (`[loom:folded ...]`) are the persisted
  compaction summaries.

```json
{
  "schema_version": "loom.context.v1",
  "user_turn": 2,
  "messages": [
    {"role": "user", "content": "..."},
    {"role": "assistant", "content": "...", "tool_calls": [{"id": "c1", "name": "write_file", "arguments": {}}]},
    {"role": "tool", "tool_call_id": "c1", "tool_name": "write_file", "content": "..."}
  ],
  "runs": [{"run_index": 1, "run_id": "...", "status": "success", "exit_code": 0, "turns": 2,
            "usage": {}, "files_changed": {"added": [], "modified": [], "deleted": [], "truncated": false}}],
  "cumulative_usage": {"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
  "compactions": 0
}
```

`messages[].role` is `user`, `assistant` or `tool`; other keys follow `compact.Message` (`name`, `tool_name`,
`tool_call_id`, `tool_calls[]` with `id`, `name`, `arguments`, optional `thought_signature`). `runs[]` holds one
record per user turn; `files_changed` there is that turn's delta (its baseline is the workspace at the start of the turn).
Consumers must ignore unknown keys.

### Validation and the broken-structure rule

Exit 2 with one diagnostic line on stderr (`loom context error: ...`): unreadable file, invalid JSON, missing or
unknown `schema_version`, unknown message role. Everything else is repaired deterministically, one stderr line per
repair (`loom context repair: ...`), before the history reaches any provider:

1. A `tool_call` with no `tool_result` directly after its assistant message is dropped; an assistant message left with
   neither text nor calls is dropped.
2. A `tool_result` that matches no call of the directly preceding assistant message (orphan, duplicate, or
   out of position) is dropped.
3. Adjacent plain messages of the same role merge with a blank line (roles alternate; caller notes join the new prompt).

Rendering (Anthropic `tool_use`/`tool_result` pairing and merged result turns, Gemini `model`/`user` roles and
`functionResponse`, OpenAI `tool` messages) is loom's job; the caller never writes provider shapes.

### Pipelines

```sh
loom -p "turn 1" --context-out - | loom -p "turn 2" --context-in - --context-out - > ctx.json
```

`loom resume <trace>` (interrupted run) is unchanged and uses the internal checkpoint; a context-continued run keeps
its prior totals through resume and writes its `--context-out` when it finishes. Two conversations are independent when
they use distinct `--workspace`, `--trace` and context files (run locks as above). `--provider mock2` is a built-in alias
of `mock` that reports itself as `mock2`, for cross-provider continuation tests.

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
- `--max-cmd-timeout S` (`run`, prompt mode, `resume`): upper clamp for `run_command` `timeout_s` (default 300). Accepts integer seconds or a duration (`10m`). Precedence flag > `constraints.max_cmd_timeout_sec` > env `LOOM_MAX_CMD_TIMEOUT` > 300. `make build` stamps `main.version`/`main.commit` from `git describe`/`git rev-parse` via ldflags (`loom --version`).
- `--max-output-tokens N` (`run`, prompt mode, `resume`): precedence flag > `constraints.max_output_tokens` > env `LOOM_MAX_OUTPUT_TOKENS` > default 8192 (was 2048). On a `max_tokens` stop, status/exit are unchanged and `output` ends with a hint naming `--max-output-tokens` / `LOOM_MAX_OUTPUT_TOKENS`. No automatic retry.
- `loom run --allow-cmd "<prefix>"` (repeatable) is a UNION with `constraints.allow_commands`; the union is then intersected with the global/agent layers (`LOOM_GLOBAL_ALLOW_COMMANDS`, `LOOM_AGENT_ALLOW_COMMANDS`) as in prompt mode. Without the flag, behavior is unchanged.

## Models: defaults, unknown ids, discovery

Default model id per provider (override with `--model` or `LOOM_MODEL`; also in `capabilities --json` `default_models`):

| Provider | Default |
|---|---|
| `gemini`, `gemini-api`, `google-agent-platform` | `gemini-3.8-flash` |
| `anthropic`, `anthropic-api` | `claude-sonnet-5-5` |
| `openai` (any OpenAI-compatible endpoint) | `gpt-4o-mini` |
| `ollama` | `qwen3:4b` (`qwen3.5:4b-mlx` on macOS) |
| `typesafe` | `jev-latest` |

Loom keeps no allowlist of ids. An unknown or unserved id is detected from the provider's first response
(HTTP 404, or 400 saying the model is unknown/unsupported) and fails the run with no retry: exit 2,
`status` `model_not_found`, `done.reason` `model_not_found`, and the one-line message
`loom: model "X" not found or not served by provider P; run "loom models --provider P" to list available ids`.

`loom models --provider <ollama|openai|anthropic|gemini> [--json]` (opt-in; the only call that lists models over the
network) prints one id per line, or `{"schema_version":1,"provider":P,"models":[...]}` with `--json`. Errors exit 2.
