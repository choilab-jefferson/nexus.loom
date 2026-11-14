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
| 1 | `task_failure` | Task failure (provider error, max turns, tool-error strikes, `constraint_failure`, ...) |
| 2 | `harness_or_policy_error` | Harness invariant or policy violation, bad flags |
| 3 | `escalation_required` | Model called `escalate`; `EscalationPayload` JSON on stdout |
| 4 | `budget_exhausted` | Token, deadline or tool-call budget exhausted |
| 130 | `interrupted` | SIGINT / SIGTERM |

### Run locks (parallel runs)

`run`, prompt mode and `resume` take an advisory `flock(LOCK_EX|LOCK_NB)` on the `--trace` file (when non-empty) for the whole run. If another live loom process holds it, loom prints one line to stderr (`loom: trace <abs> is in use by another loom process (parallel runs need distinct --trace)`) and exits 2. The workspace is not locked: parallel runs may share one `--workspace` (a team working in one project directory). Instead `write_file` and `edit_file` take an exclusive advisory `flock` on the file they write, for the whole write (`edit_file`: read, replace and write under one lock), waiting up to 30 s while another process writes the same file (then a tool error `<path> is being written by another process; try again`). Reads take no lock. The kernel releases locks on process death; no lock files are created. Unix only (build tag `unix`); a no-op on other platforms. Code: `cmd/loom/lock*.go`, `pkg/tools/filelock*.go`.

### Write tools

After earendil-works/pi (`packages/coding-agent/src/core/tools/edit.ts`, `edit-diff.ts`), plus a stale-write guard.

- `edit_file {path, edits: [{target, replacement}, ...]}` (legacy top-level `target`/`replacement`, and `oldText`/`newText` or `old_text`/`new_text` inside a block, are accepted). Under the file's write lock loom reads the file as it is now, finds every block, and writes once. Each `target` must occur exactly once: exact match first (CRLF and a BOM ignored), else a tolerant match that ignores trailing whitespace and maps typographic quotes, dashes and special spaces to ASCII (a tolerant match that ends a line also replaces that line's trailing whitespace). All blocks are matched against the current file, not against each other's results. Errors change nothing: `the target|edits[i] in <path> is empty`, `<target|edits[i]> not found in <path>: it must match the file exactly, including whitespace and line breaks (read the file again)`, `found N occurrences of <target|edits[i]> in <path>; a target must be unique: add more context around it`, `edits[a] and edits[b] overlap in <path>: ...`, `no change in <path>: ...`. Only the matched bytes are replaced; a BOM, CRLF endings and untouched bytes are kept, and newlines in a replacement take the file's line ending (CRLF when most lines use it). Success: `edited <path>: replaced N block(s)` followed by a unified diff (3 context lines). `edit_file` does not create files.
- `write_file {path, content}` creates a file, or replaces an existing one only when this run has seen its current content: the run records the sha256 of each file it reads (`read_file`) or writes, and before overwriting loom re-reads the file under its lock and compares. Refusals: `<path> exists and was not read in this run: read it first, or change it with edit_file`, `<path> changed since you read it (another process wrote it): read it again before overwriting, or use edit_file`. Success: `wrote N bytes to <path> (created|replaced)`. The record lives for one process (a `resume` starts empty).


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
| `builtin_tools` | string[] | Built-in tool names, sorted; includes `job_kill`, `job_log`, `job_start`, `job_status`, `job_wait` (additive, see "Background jobs") |
| `providers` | string[] | Accepted `--provider` values (`mock2` = a second mock for cross-provider tests) |
| `agents_md_autoload` | bool | |
| `capability_restriction` | object | `flags`, `task_keys`, `refusal_kinds`, `terminal_status` (`constraint_failure`), `note`. Additive; see "Capability restriction" below. |
| `exit_codes` | object | name -> int, table above |
| `read_only_commands` | bool | `true` when `run_readonly` exists. Additive, from v0.3.0. |
| `readonly_commands` | object | `tool` (`run_readonly`), `task_key` (`constraints.readonly_commands`), `default` (`off`), `enable` (executables that can be enabled: `git`, `glab`), `allowlist` (executable -> allowed argv shapes), `shell` (`false`), `env`. Additive, from v0.3.0. See "Read-only commands". |
| `tool_result_cap` | object | `task_key` (`constraints.max_tool_result_bytes`), `flag` (`--max-tool-result-bytes`), `default` (16384), `min` (256), `max` (262144), `marker`. Additive. |
| `keep_tool_results` | object | `task_key` (`constraints.keep_tool_results`), `flag` (`--keep-tool-results`), `default` (0 = keep all), `elided` (stub format). Additive. |

Pinned by `cmd/loom/capabilities_test.go`.

## 3. TaskResult JSON (`--json`)

Struct: `pkg/engine/engine.go:120` (`TaskResult`), printed by `runAndReport` (`cmd/loom/main.go`, `res` marshalled
when `--json`). Exit code of the process equals `exit_code`.

| Key | Type | Notes |
|---|---|---|
| `schema_version` | int | `1` |
| `exit_code` | int | Table in section 1 |
| `status` | string | Open set: `success`, `max_turns_exhausted`, `budget_exhausted`, `interrupted`, `escalation_required`, `provider_error`, `auth_error`, `model_stop_error`, `unknown_stop_reason`, `max_tokens`, `tool_error_strikes`, `constraint_failure`, `policy_violation`, `prefix_violation`, `model_not_found`, `harness_error`, `paused`. Key off `exit_code`; treat unknown statuses as opaque. |
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
| `tool_call` | `name`, `kind`, `args_hash`; `run_command`, `job_start` and `run_readonly` additionally `argv` (array of strings, redacted: secret-named `KEY=value` and `--token`/`--password`/`--api-key`-style flag values (`--f x` or `--f=x`) become `***`, `Bearer <tok>` becomes `Bearer ***`; capped at 64 elements, 256 bytes each, ~4 KiB total, a final `...[truncated]` element marks a cut; `args_hash` is still over the unredacted args) |
| `tool_result` | `name`, `status` + `consecutive` (tool unavailable) or `bytes_raw`, `bytes_passed`, `truncated`; `run_command` and `run_readonly` additionally `exit_code` (int; `-1` = killed by signal/timeout or not started), `timed_out` (bool) |
| `policy_violation_recovered` | `kind` (`allowlist` \| `eval_flag` \| `path_not_allowed` \| `tool_not_available`), `tool` (`run_command`, `job_start` or `run_readonly` for `allowlist`/`eval_flag`; the refused tool for the other kinds). Non-allowlisted argv0, shell wrapper or interpreter eval flag: returned to the model as a tool error and counted toward the tool-error strike limit (`tool_error_strikes`). Path escape, symlink write and PHI cloud refusal stay fatal (`policy_violation`, exit 2). |
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

Exactly one `done` event is written on every exit path. Argument values are never traced, only `args_hash` (exception: redacted `argv` of `run_command`, `job_start` and `run_readonly`).

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
they use distinct `--trace` and context files and may share a `--workspace` (locks as above). `--provider mock2` is a built-in alias
of `mock` that reports itself as `mock2`, for cross-provider continuation tests.

## 5. Release snapshots

The public repository is a release mirror: one squashed commit per release, with a tag
(`vX.Y.Z`). Release binaries (linux and darwin, amd64 and arm64) and `checksums.txt` are built
by `.github/workflows/release.yml` on a tag push, with
`-ldflags "-X main.version=<tag> -X main.commit=<sha>"`. Version strings come from the tag, not
from files in the tree.

## Background jobs (additive)

Long runs (ML training, sweeps) outlive one tool call. Five built-in tools; `schema_version` stays 1 (new tool names only). Code: `pkg/tools/jobs.go`, schemas `pkg/tools/tools.go`.

| Tool | Args | Result |
|---|---|---|
| `job_start` | `argv []string`, `detach bool` (default false) | Job record JSON (below); `status` `running` |
| `job_status` | `job_id` | Job record JSON |
| `job_log` | `job_id`, `tail_n` (default 50; <=0 -> 50) | Last `tail_n` lines of the log (reads at most the last 1 MiB) |
| `job_wait` | `job_id`, `timeout_s` (default 30; <=0 -> 30) | Job record JSON, newline, last 50 log lines. `timeout_s` is clamped like `run_command` (`--max-cmd-timeout`, default 300). Still running at the deadline: `timed_out=true`, status stays `running`. |
| `job_kill` | `job_id` | Job record JSON after SIGKILL of the job's process group (no-op if not running) |

`job_start` uses the same argv allowlist, shell/eval refusal, argv0 resolution and env scrub as `run_command`; cwd is the workspace; no shell; stdout and stderr go to the log file. Unlike `run_command` it has no timeout and returns immediately. `job_id` is `job-` + 12 hex chars; any other value is a tool error (`invalid job_id`), an unknown one `unknown job_id`.

State is in the workspace (mode 0700 dir, 0600 files): `.loom/jobs/<id>.json` (record, replaced atomically) and `.loom/jobs/<id>.log` (append-only output).

Record `<id>.json` (`jobMeta`):

| Key | Type | Notes |
|---|---|---|
| `job_id` | string | |
| `argv` | string[] | Unredacted |
| `pid` | int | Process-group leader |
| `status` | string | `running`, `exited`, `killed` |
| `exit_code` | int, optional | Set on `exited` by the starting process; `-1` = killed by signal. Absent when `killed`, or when settled by a later invocation (code unknown) |
| `detached` | bool | |
| `started`, `ended` | string | RFC3339 UTC; `ended` omitted while running |
| `timed_out` | bool, optional | Only in `job_wait` output when the wait deadline hit; not persisted |

A record marked `running` whose process is gone is settled to `exited` (no `exit_code`) on the next read, including from a resumed run.

Lifecycle:
- Non-detached job: killed (process group SIGKILL, status `killed`) when the run ends, on every exit path of the engine (success, failure, interrupt, pause).
- `detach=true`: not killed on exit; it outlives the loom process. This is an explicit opt-in exception to "run ends => nothing is running" (spec I4).
- `loom resume` and `--context-in` runs can `job_status`/`job_log`/`job_wait`/`job_kill` any job whose `job_id` they know; ids are looked up on disk, not in memory.
- Checkpoint (`<trace>.ckpt`) gains optional `job_ids` (string[], `omitempty`): jobs started in this logical run, merged across resumes, deduplicated, in start order.

Trace: no new event types. `job_start` emits `tool_call` with redacted `argv` like `run_command`; every job tool emits the usual `tool_result` (`bytes_raw`, `bytes_passed`, `truncated`) without `exit_code`/`timed_out` keys.

`files_changed`: `.loom/` is not excluded from the snapshot, so `.loom/jobs/<id>.json` and `.log` appear as `added`/`modified` like any other workspace file.

## Tool output truncation and run_command result

- Tool output over `max_output_bytes` keeps ~1/3 head and ~2/3 tail (UTF-8 safe) joined by `\n[loom:truncated K bytes in middle]\n`; `bytes_passed` counts head+tail only. `run_command` buffers 1 MiB the same way (head + ring tail), so the end of long output survives.
- `run_command` results carry structured `exit_code`, `timed_out` and `truncated` (also in the `tool_result` trace event, additive keys). On timeout `timed_out=true` and `exit_code=-1`. The text `[exit_error: ...]` suffix remains for the model.

## Operator flags: env, output cap, allow-cmd (additive)

- `--pass-env NAME[,NAME|PREFIX*]` (repeatable; `run`, prompt mode, `resume`) and `constraints.pass_env` extend the `run_command` child env allowlist. Defaults now include `PYTHONPATH`, `PYTHONUNBUFFERED`, `PYTHONHASHSEED`, `CUDA_VISIBLE_DEVICES`, `CUDA_HOME`, `CUDA_*`, `HF_HOME`, `HF_HUB_CACHE`, `HF_DATASETS_CACHE`, `TRANSFORMERS_CACHE`, `OMP_NUM_THREADS`. Secret-looking names (`*TOKEN*`, `*SECRET*`, `*PASSWORD*`, `*_API_KEY`, ...; `policy.IsSecretEnvName`) are never matched by a prefix; only an exact name passes one, with a stderr warning and an `env_secret_passed` trace event (`name`).
- `--max-cmd-timeout S` (`run`, prompt mode, `resume`): upper clamp for `run_command` `timeout_s` (default 300). Accepts integer seconds or a duration (`10m`). Precedence flag > `constraints.max_cmd_timeout_sec` > env `LOOM_MAX_CMD_TIMEOUT` > 300. `make build` stamps `main.version`/`main.commit` from `git describe`/`git rev-parse` via ldflags (`loom --version`).
- `--max-tool-result-bytes N` (`run`, prompt mode, `resume`) / `constraints.max_tool_result_bytes`: cap on each tool result fed back to the model (default 16384, clamped to 256..262144). An oversized result keeps about a third from the head and two thirds from the tail with `[loom:truncated N bytes in middle]` between; the trace `tool_result` event reports `bytes_raw`, `bytes_passed`, `truncated`. Every result is re-sent on every later turn, so a lower cap cuts prompt tokens roughly by turns x cap.
- `--keep-tool-results K` (`run`, prompt mode, `resume`) / `constraints.keep_tool_results`: keep only the last K tool results verbatim in the running context; older ones become `[loom:elided bytes=N lines=M]` plus their first three non-empty lines (at most 400 bytes). Tool-call pairing is unchanged. Default 0 = keep all (the token-threshold folding of Invariant I3 still applies). Each pass that elides something emits a `tool_results_elided` trace event (`turn`, `count`, `keep`).
- `--max-output-tokens N` (`run`, prompt mode, `resume`): precedence flag > `constraints.max_output_tokens` > env `LOOM_MAX_OUTPUT_TOKENS` > default 8192 (was 2048). On a `max_tokens` stop, status/exit are unchanged and `output` ends with a hint naming `--max-output-tokens` / `LOOM_MAX_OUTPUT_TOKENS`. No automatic retry.
- `loom run --allow-cmd "<prefix>"` (repeatable) is a UNION with `constraints.allow_commands`; the union is then intersected with the global/agent layers (`LOOM_GLOBAL_ALLOW_COMMANDS`, `LOOM_AGENT_ALLOW_COMMANDS`) as in prompt mode. Without the flag, behavior is unchanged.

## Capability restriction (additive)

Restrict what the model can do at exposure time; prompts are not the protection.

| Flag (`run`, prompt mode) | task.json `constraints` key | Default | Effect |
|---|---|---|---|
| `--tools a,b` (repeatable) | `tools` | all built-ins | Only these tool schemas are sent to the provider. Unknown name: exit 2 (`harness_error`). The flag replaces the spec value. |
| `--read-glob G` (repeatable) | `read_globs` | `**` | File tools (`read_file`, `list_dir`, `search`) may read only matching workspace paths. When set, `write_globs` are added so written files stay readable. |
| `--write-glob G` (repeatable) | `write_globs` | `**` | `write_file` / `edit_file` may write only matching workspace paths. |
| `--max-disallowed-tool-requests N` | `max_disallowed_tool_requests` | unset (no limit) | See below. `0` ends the run on the first refusal. |

- Tools that could never succeed in the run are not exposed, even if listed in `tools`: `run_command` and the job
  tools (`job_start`, `job_status`, `job_log`, `job_wait`, `job_kill`) while the effective allowlist (`--allow-cmd` /
  `constraints.allow_commands`, after the global/agent layers) is empty, and `run_readonly` while
  `constraints.readonly_commands` is unset. Without `--tools` the exposed set is otherwise unchanged.
- The exposed schemas are frozen in the run prefix (`tool_schemas_json`) and every provider renders tools from it, so
  `frozen_prefix_hash` covers exactly what is sent. A resumed run keeps the spec from its checkpoint.
- A call to a built-in tool that is not exposed runs nothing and returns
  `{"status":"tool_not_available","tool":"<name>"}`; the run continues. Non-built-in names keep the older
  `tool_unavailable` path.
- A file-tool path outside the globs returns `error: policy violation: path "<p>" not permitted for <read|write> by
  allowlist` (kind `path_not_allowed`); the run continues and the file is untouched. Workspace escape, symlink
  writes, deny globs (`.git`, `.env*`) and PHI refusals stay fatal (exit 2).
- Refusals of kind `tool_not_available`, `allowlist` (exposed `run_command` with a non-allowlisted argv) and
  `path_not_allowed` are counted per tool name and never reset by other tools' successes. When
  `max_disallowed_tool_requests` is N and one tool's count exceeds N, the run ends: `status` `constraint_failure`,
  exit 1, `done.reason` `constraint_failure`. Unset keeps the previous behaviour (these refusals still count toward
  the consecutive tool-error strike limit, `tool_error_strikes`). `eval_flag` rejections are not counted.
- **Not a sandbox for `run_command`.** The read/write globs confine loom's file tools only. A child started by
  `run_command` runs with the user's filesystem permissions and can read or write anything they can, inside or
  outside the globs and the workspace. Read-only inputs need filesystem-level enforcement (permissions, a separate
  mount, or not exposing `run_command`).

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

## Read-only commands (`run_readonly`, additive, v0.3.0)

A tool for read-only lookups (git history, GitLab MRs/issues/CI) without granting `run_command`. `schema_version` stays 1 (new tool name, new optional task key, new capabilities keys). **Off by default**: the tool is always listed in `builtin_tools`, but a call fails with a recoverable `allowlist` tool error unless the task enables it:

```json
{"constraints": {"readonly_commands": ["git", "glab"], "pass_env": ["GITLAB_TOKEN"]}}
```

`readonly_commands` takes any subset of `git`, `glab`. It is **mutually exclusive with writing tools**: a task that sets both `readonly_commands` and `allow_commands` fails with `harness_error` (exit 2) before any model call, and in a task with `readonly_commands` the `write_file`, `edit_file`, `run_command` and `job_start` tools are refused with a recoverable `allowlist` error. Optional companions: `readonly_git_common_dirs` (absolute common dirs outside the workspace a linked-worktree workspace may use) and `readonly_glab_groups` (groups allowed in three-segment `-R`). Code: `pkg/policy/readonly.go` (validation), `pkg/tools/readonly.go` (exec).

Args: `argv` (string array; `argv[0]` is the bare name `git` or `glab`, never a path), `timeout_s` (default 30, clamped like `run_command` by `--max-cmd-timeout`).

Exact allowlist (anything else is refused, no prefix matching, no aliases):

| Exe | Allowed |
|---|---|
| `git` | `log`, `show`, `diff`, `status`, `rev-parse`, `ls-files`, `blame`, and `branch --list` (display flags only: `-a -r -v -vv --show-current --contains --merged --no-merged --points-at --sort=... --format=... --abbrev ...`; creation, deletion, copy, rename are refused) |
| `glab` | `mr view`, `mr list`, `mr diff`, `issue view`, `issue list`, `ci status`, `ci list`, and `api <path>` (GET only, endpoint allowlist below; `ci view` is a TUI and is not offered) |

Refusals:

- git flags: global options other than `--no-pager` and `--git-dir`/`--work-tree` (the latter two only when the path resolves, symlinks included, inside the task workspace); `-c` anywhere; `-o`; `-O*`; `-X*`; `--output`, `--exec`, `--exec-path`, `--ext-diff`, `--textconv`, `--no-index`, `--git-dir`/`--work-tree` after the subcommand (bare `rev-parse --git-dir` is a query and stays allowed), `--contents`, `--ignore-revs-file`, `--orderfile`, `--exclude-from`, `--exclude-per-directory`, `--open-files-in-pager`, `--resolve-git-dir`, `--show-signature`, and any unambiguous abbreviation of those; `blame -S`. Short-flag clusters are not decomposed: the only dangerous short options are `-c` (global, parsed before the subcommand), `-o` (exact), `-O*` and `-X*` (prefix-checked), so a cluster cannot hide one. For `log`, `show`, `diff` and `blame` loom appends `--no-ext-diff --no-textconv` (before any `--`).
- git path-like arguments (every non-flag argument, everything after `--`, and `--opt=value` / `-Xvalue` values): any `..` segment (also after `:` as in `rev:path`), magic pathspecs `:(...)` and `:/...`, and any absolute or relative path whose symlink-resolved target leaves the task workspace (so `/dev/null`, `/etc/passwd`, `/proc/self/environ` are refused). This closes the implicit `--no-index` mode of `git diff <outside-file> <file>`. Revisions and plain values pass because git cannot resolve them to a file outside the repository.
- git repo config: before every git call loom reads the repository's effective config with `git config --list -z --no-includes` (parsing only) and refuses when it defines `filter.*`, `diff.external`, `diff.*.command`, `diff.*.textconv`, `core.fsmonitor`, `core.hooksPath`, `core.pager`, `core.sshCommand`, `core.askPass`, `core.editor`, `gpg.program`, `gpg.*.program`, `core.worktree`, `core.bare=true`, `extensions.worktreeConfig=true` (so `config.worktree` is never in play) or `include.*`/`includeIf.*`. Aliases are not refused (git never lets an alias shadow a builtin and every allowed subcommand is a builtin). System and global git config are disabled (`GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL=/dev/null`, `GIT_ATTR_NOSYSTEM=1`). Workspaces must deny writes to `.git/**` (loom's own `write_file`/`edit_file` already do) so config cannot be planted between the check and the command.
- glab flags: `--web`/`-w` (also inside a short-flag cluster), `--hostname`, and `-R`/`--repo` values that are not exactly `OWNER/REPO` or `GROUP/SUBGROUP/REPO` (a host-looking first segment, a URL, more segments or a missing value); the three-segment form additionally needs its first segment (the group) listed in `constraints.readonly_glab_groups` (default none, so only `OWNER/REPO` works). The host is always the one glab is configured for (workspace remote or `GITLAB_HOST` set by the host operator).
- `glab api`: one relative path, `-X`/`--method` equal to `GET`, `--paginate`, `-i`/`--include`, `--silent`; fields (`-f -F --field --raw-field`), `--input`, headers (`-H`, which could override the method) and every other flag are refused. The path must match an endpoint allowlist: `projects/:id/{merge_requests,issues,pipelines,jobs,labels,milestones}[/...]`, `projects/:id/repository/{commits,branches,tags,compare,files,tree}[/...]`, `groups/:id/{merge_requests,issues,labels,milestones}[/...]`, where `:id` is numeric or URL-encoded `group%2Fproject`. Only the id segment may carry an encoded slash (`group%2Fproject`); a `%2F` or `%5C` anywhere else is refused, so nested `repository/files/<path>` is not reachable (use git or `read_file`). Refused: any other path (the bare project object, `user`, `users`, `graphql`, `api/v4/...`, leading `/`, URLs), empty/dot/`..` segments (also percent-encoded), any segment equal to `variables`, `deploy_tokens`, `deploy_keys`, `runners`, `hooks`, `access_tokens`, `members`, `user`, `users`, `personal_access_tokens`, `trace`, `artifacts`, `secure_files`, and query parameters `private_token`, `access_token`, `job_token`, `sudo`.
- All: `argv[0]` resolving inside the workspace, NUL bytes, any other executable or subcommand.

Execution: no shell; cwd = the task workspace (resolved); `Setpgid`, whole process group killed on timeout; stdout+stderr share a 256 KiB head+tail buffer, then the usual `max_output_bytes` clamp. Environment is rebuilt from scratch: `PATH`, `HOME`, `LANG`, `LANGUAGE`, `LC_*`, `TZ`, `XDG_CONFIG_HOME`; `GITLAB_*` and `GLAB_*` names **only** when the host lists them in `constraints.pass_env` (a secret-looking name such as `GITLAB_TOKEN` needs its exact name, as for `run_command`); plus fixed `GIT_CEILING_DIRECTORIES`, `GIT_DISCOVERY_ACROSS_FILESYSTEM=0`, `GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL=/dev/null`, `GIT_ATTR_NOSYSTEM=1`, `TERM=dumb`, `NO_COLOR=1`, `NO_PROMPT=1`, `GLAB_CHECK_UPDATE=false`, `GIT_TERMINAL_PROMPT=0`, `GIT_OPTIONAL_LOCKS=0`, `GIT_PAGER=cat` and `GIT_CONFIG_*` forcing `core.fsmonitor=false`, `core.hooksPath=/dev/null`. Nothing else is inherited (no provider keys, no `GIT_*` from the parent).

Denials are recoverable tool errors with `violation_kind` `allowlist` (counted toward the tool-error strike limit); there is no fatal path. Trace: no new event types. `tool_call` carries redacted `argv` (same rules as `run_command`); `tool_result` carries `exit_code` and `timed_out`; a denial adds `policy_violation_recovered` with `tool` `run_readonly`.

Not a sandbox: the config check narrows but does not remove trust in the repository's own files (for example `.gitattributes` only matters if a filter is configured, which is refused). glab sends the passed token to the host in the workspace remote or `GITLAB_HOST` chosen by the host, never to a model-chosen host.
