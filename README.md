# Loom

Loom is a small, dependency-free (Go stdlib only) agent CLI. It runs one bounded coding task against a model of your choice (Ollama, Anthropic, OpenAI, Gemini, ...) with a fixed tool set (read, search, edit, write, allowlisted commands, background jobs), a workspace sandbox, hard turn/token/time budgets, and a JSONL trace. It is built to run unattended as a leaf worker, and works standalone with zero configuration files.

Loom is a single-agent CLI: one agent, one bounded task per run. It has no sub-agent or orchestration features; multi-agent orchestration belongs to nexus (see [Use from nexus.lite](#use-from-nexuslite)). Switching providers is one flag: see [`examples/provider_switch.py`](examples/provider_switch.py).

**Running in Google Colab?** See the [Colab quickstart](docs/colab-quickstart.md) (notebook cells: prebuilt binary, Colab Secrets, Ollama, nexus.lite) or drive Colab from your terminal with the [Colab CLI guide](docs/colab-cli.md).

## Install

```bash
go install github.com/choilab-jefferson/nexus.loom/cmd/loom@latest   # needs Go 1.22+
make build            # from a clone; produces ./bin/loom
# or download a prebuilt binary: https://github.com/choilab-jefferson/nexus.loom/releases/download/v0.2.3/loom-<os>-<arch>
# or install via nexus-lite (PyPI): pip install nexus-lite && nexus-lite loom install
```

Maintainers: see [docs/release.md](docs/release.md) for the GitHub snapshot publish procedure.

## 60-second quickstart

```bash
# Smoke test, no model, no network
./bin/loom -p "hello" --provider mock

# Local model (Ollama must be running: `ollama serve`)
./bin/loom -p "add a unit test for foo()" --provider ollama

# Hosted model
export ANTHROPIC_API_KEY=sk-...
./bin/loom -p "fix the failing test" --provider anthropic --allow-cmd "go test"
```

The default `--provider auto` picks the first of `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`/`GOOGLE_API_KEY`, `OPENAI_API_KEY` that is set, else a running Ollama (`OLLAMA_HOST`, default `127.0.0.1:11434`). If none is available, loom exits 2 with `no model provider configured: ...`. With `--provider ollama` and no server, it reports `ollama not reachable at ... (start it with ollama serve, ...)`.

## Usage

Run `loom` with no arguments (exit 2) or `loom --help` (exit 0) for the full list.

| Flag | Meaning |
|---|---|
| `-p "<text>"` | one-shot instruction (or `loom run --task-file task.json`) |
| `--provider` | `auto`, `ollama`, `openai`, `anthropic`, `gemini`, `google-agent-platform`, `typesafe`, `mock` |
| `--model` | model id (provider default otherwise; see "Models" below) |
| `--workspace` | workspace root; all file access is confined here |
| `--allow-cmd "git status"` | allow a command prefix for `run_command` (repeatable; none by default). On `loom run` it is unioned with task.json `constraints.allow_commands`, then intersected with global/agent layers |
| `--pass-env NAME[,NAME\|PREFIX*]` | extra env vars for `run_command` children (repeatable; also `run`/`resume`, spec `constraints.pass_env`). ML defaults (PYTHONPATH, CUDA_*, HF_HOME, ...) pass already; secrets (`*TOKEN*`, `*_API_KEY`, ...) only by exact name, with a warning |
| `--tools a,b` | expose only these built-in tools to the model (repeatable; spec `constraints.tools`; default all). `run_command` is never exposed without `--allow-cmd`. A call to an unexposed tool returns a `tool_not_available` observation |
| `--read-glob G` / `--write-glob G` | confine file tools to matching workspace paths (repeatable; spec `constraints.read_globs` / `write_globs`; default `**`). A miss is a recoverable `path_not_allowed` observation. **Does not confine `run_command` children** |
| `--max-disallowed-tool-requests N` | end with status `constraint_failure` (exit 1) once one tool is refused more than N times (not exposed, not allowlisted, path not allowed); spec `constraints.max_disallowed_tool_requests`; unset = no limit |
| `--max-cmd-timeout S` | `run_command` `timeout_s` clamp (default 300): integer seconds or duration like `10m`; flag > `constraints.max_cmd_timeout_sec` > `LOOM_MAX_CMD_TIMEOUT` |
| `--max-tool-result-bytes N`, `--keep-tool-results K` | cap each tool result fed back to the model (default 16384, 256..262144); keep only the last K results verbatim, older ones become their first lines (also `run`/`resume`, spec `constraints.max_tool_result_bytes` / `constraints.keep_tool_results`) |
| `--max-output-tokens N` | per-response output cap (also `run`/`resume`); flag > spec > `LOOM_MAX_OUTPUT_TOKENS` > 8192 |
| `--max-turns`, `--max-total-tokens`, `--deadline`, `--max-tool-calls` | budgets |
| `--trace file.jsonl`, `--json` | trace output, machine-readable result |
| `loom resume <trace>` | continue an interrupted run |
| `--context-in F` / `--context-out F` | continue a conversation from / write it to a `loom.context.v1` file (`-` = stdin/stdout; spec keys `context_in`/`context_out`) |
| `loom capabilities --json` | tools and providers as JSON |

Keys: `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GEMINI_API_KEY`/`GOOGLE_API_KEY`, `TYPESAFE_API_KEY`.

## Conversations: one call = one user turn

Each loom call answers one user prompt (any number of model turns and tool calls). The caller owns the
prompt loop and the history: `--context-out` writes the conversation as `loom.context.v1` JSON, and
`--context-in` continues from it (the new `-p` prompt is appended). Both accept `-` for stdin/stdout, so
calls compose in a shell pipeline; the file is plain JSON you may trim, summarise, redact or merge
between calls, and it works across providers.

```sh
loom -p "create notes.txt" --provider anthropic --context-out ctx.json
loom -p "now show what you wrote" --provider gemini --context-in ctx.json --context-out ctx.json
loom -p "turn 1" --context-out - | loom -p "turn 2" --context-in - --context-out - > ctx.json
```

With `--context-out -` stdout carries only the context; the result/`--json` output goes to stderr.
A dangling `tool_call` or orphan `tool_result` is dropped with a note on stderr; invalid JSON or an
unknown `schema_version` exits 2. `loom resume <trace>` (interrupted run) is unchanged. Format and rules:
[docs/contract.md](docs/contract.md#conversation-context-loomcontextv1).

## Background jobs

For long ML runs that exceed the `run_command` timeout clamp. Jobs run allowlisted argv (no shell) and outlive a single tool call.

- `job_start(argv, detach?)` returns a `job_id`; `job_status(job_id)` gives status (`running`/`exited`/`killed`), exit code, pid.
- `job_log(job_id, tail_n?)` returns the last lines of the log (default 50); `job_wait(job_id, timeout_s?)` blocks until exit (default 30, clamped like `run_command`) and returns status plus log tail.
- `job_kill(job_id)` kills the job's process group.
- State and output live in `<workspace>/.loom/jobs/<id>.{json,log}`; the checkpoint records `job_ids`, so `loom resume` can inspect, wait on, or kill them.
- Non-detached jobs are killed when the run exits. `detach: true` jobs survive it and can be inspected from a later invocation on the same workspace.
- The command must match `--allow-cmd`, same as `run_command`.

```sh
loom -p "train with train.py, detached, then report the log tail" --allow-cmd "python train.py"
```

## Loop mode

`loom serve` keeps one loom alive and controls it through a directory instead of a terminal: the nexus **file bus** (spec: nexus.lite `docs/specs/file-bus.md`, sections 3-7 and 13). A host sends messages into `msg/<name>/`, loom answers into the host's inbox. There is no TTY, no stdin and no chat UI, and loom never opens SQLite; the bus client is Go stdlib only (`pkg/filebus`). One-shot `loom -p` / `loom run` are unchanged.

```sh
loom serve --bus-dir .nexus/bus --name loom-a1 --workspace . --provider anthropic \
  --allow-cmd "go test" --max-turns 20 --max-wall 2h --idle-timeout 30m
```

| Flag | Meaning |
|---|---|
| `--bus-dir DIR`, `--name ID` | bus directory (or `NEXUS_BUS_DIR`) and the session name; hosts send to `msg/<ID>/` |
| run flags | `--provider`, `--model`, `--workspace`, `--allow-cmd`, `--phi`, budgets and capability flags, as for `loom -p`; they apply to every user turn |
| `--max-turns N` | user turns for the whole session (0 = unlimited). The per-turn agent step cap is `--max-agent-turns` (default 8) |
| `--max-wall D`, `--idle-timeout D` | session wall-clock budget (exit 4); no message for D (exit 0) |
| `--on-job-exit notify\|turn` | default `notify`: a job exit only emits a `job` message; `turn` also starts one synthetic user turn |
| `--report-to NAME` | target for events outside a request (`started`, `job`, ...); default is the last requester |
| `--poll-interval`, `--notify-tcp host:port` | poll fallback (default 1s, backs off while idle); optional loopback TCP wake-up endpoint |

How it works: loom registers `sess/<ID>.json` (heartbeat every ttl/3), wakes on a Unix datagram (or the poll), and reads its inbox (cursor `_default`) **only at turn boundaries**. `loom.task` / `loom.prompt` runs one user turn on the persistent `loom.context.v1` (`state/<ID>.context.json`, kept across restarts) and answers with `loom.result` (status, exit code, usage, trace pointer). Control messages are handled and acked immediately:

| Host -> loom | Effect |
|---|---|
| `loom.task` / `loom.prompt` | one user turn; `data.prompt` (or the body), `context_files`, `constraints` (budgets can only be lowered) |
| `loom.inject` | append `messages` and workspace files (read under the usual path policy) to the context |
| `loom.extract` | write `blob/<requester>/<id>.json` (`context` or `summary`) and reply `loom.extracted` with path and sha256 |
| `loom.stop` | ack, emit `loom.state stopped`, exit 0; later messages stay unacked |
| `loom.resume` | continue the interrupted or budget-stopped turn from its checkpoint |

Loom -> host: `loom.result`, `loom.extracted`, `loom.state` (`started|idle|busy|stopped|exiting`) and the shared `job` kind for `.loom/jobs` jobs (`RUNNING` on start, `OK|FAIL|GONE|KILLED` on exit). A crash during a turn is reported on restart as `loom.result` `status: interrupted`, exit code 130; the turn is re-run only on `loom.resume`. Jobs started with `job_start` are killed when their turn ends unless `detach: true`. Exit codes: 0 stop or idle timeout, 4 budget, 130 signal, 2 config error. Details: [docs/contract.md](docs/contract.md#loop-mode-loom-serve-additive).

## Models

Defaults: gemini `gemini-3.8-flash`, anthropic `claude-sonnet-5-5`, openai `gpt-4o-mini`, ollama `qwen3:4b` (`qwen3.5:4b-mlx` on macOS). Override with `--model` or `LOOM_MODEL`.
List the ids your key can use (network call, only when invoked): `loom models --provider gemini|openai|anthropic|ollama [--json]`.
An unknown id fails fast on the first call with exit 2 and `loom: model "X" not found or not served by provider P; run "loom models --provider P" ...` (no retry, no allowlist). See docs/contract.md.

## Safety model

- File tools resolve symlinks and refuse paths outside `--workspace` or matching deny globs; `--read-glob` / `--write-glob` narrow them further (recoverable refusal).
- Only the tools in `--tools` are offered to the model; `run_command` is offered only when a command is allowlisted.
- `run_command` children are not confined by the read/write globs: they run with your filesystem permissions. Do not rely on the globs to protect inputs when `run_command` is exposed.
- `run_command` runs only allowlisted argv prefixes (global ∩ agent ∩ task), never through a shell; interpreter eval flags are rejected.
- `job_start` uses the same allowlist, env scrub and process-group kill as `run_command`; non-detached jobs are killed when the run exits, `detach` jobs outlive it.
- API keys are scrubbed from child-process environments and redacted in errors.
- Budgets (turns, tokens, tool calls, deadline) end the run with a defined exit code and trace.
- Three consecutive tool errors stop the run.

## Parallel runs

Many loom processes can run at once (e.g. one per task) if each has its own `--trace` and its own `--workspace`.

- Distinct `--trace` paths never collide (the checkpoint is `<trace>.ckpt`, written atomically). `--trace ""` means no trace and no resume.
- A second process on the same `--workspace` or the same `--trace` is refused immediately with exit 2 and a one-line error (advisory `flock`, released by the kernel when a process dies, so there are no stale locks and no lock files). Unix only; elsewhere the check is a no-op.
- Background job state (`.loom/jobs`) is per workspace; detached jobs keep running after the lock is released.
- Provider rate limits and API-key quotas are shared across processes, so many parallel runs may hit 429s.

## Exit codes

| Exit Code | Constant | Meaning |
|---|---|---|
| `0` | `policy.ExitOK` | Task completed within turn budget |
| `1` | `policy.ExitTaskFail` | Task failure: provider error, `max_turns` exhausted, tool-error strikes, or `constraint_failure` (repeated refused tool requests) |
| `2` | `policy.ExitHarnessErr` | Fatal harness error, missing key, or `PolicyViolationError` |
| `3` | `policy.ExitEscalate` | Structured `escalation_required` JSON payload emitted on stdout |
| `4` | `policy.ExitBudget` | Token, deadline or tool-call budget exhausted |
| `130` | `policy.ExitInterrupted` | SIGINT/SIGTERM; resumable via `loom resume <trace>` |

`loom capabilities --json` is the machine-readable contract (tools, providers, exit codes). Details: [`docs/spec/loom-v0.1.md`](docs/spec/loom-v0.1.md). Colab: [`docs/colab-quickstart.md`](docs/colab-quickstart.md).

## Nexus integration (optional)

Nothing here is needed to use loom. These features only activate when Nexus files exist or when you opt in.

- **AGENTS.md autoload**: `AGENTS.md` found in the workspace or up to 4 parents is loaded into the prompt prefix. Disable with `--no-agents-md` or `LOOM_NO_AGENTS_MD=1`.
- **Host-managed context**: loom reads no Nexus state. The host passes context via the prompt, `task_slice.context_files`, or `--context-in` / `--context-out`.
- **`--phi`**: local-only model confinement (non-local endpoints are refused).
- **Token store**: key lookup order per provider is the standard env var, then the `NEXUS_*` alias (`NEXUS_ANTHROPIC_API_KEY`, `NEXUS_OPENAI_API_KEY`, `NEXUS_GEMINI_API_KEY`, `NEXUS_GOOGLE_AGENT_API_KEY`), then `$NEXUS_CONFIG_HOME/tokens.env` (default `~/.config/nexus/tokens.env`) only if that file exists. `IKB_MCP_TOKEN` from it is injected only into `ikb` subprocesses.

- **google-agent-platform**: the only provider with extra required config: a GCP project via `NEXUS_GCP_PROJECT` (optional `NEXUS_GCP_LOCATION`, default `global`) or a JSON file `{"project","location"}` at `NEXUS_GAP_CREDIT_CONFIG` / `$NEXUS_CONFIG_HOME/google-agent-platform-credit.json`. Without it the provider fails with a clear error; all other providers are independent of nexus.

### Use from nexus.lite

[nexus.lite](https://github.com/choilab-jefferson/nexus.lite) is the intended orchestrator. Loom is invoked as a plain subprocess; no nexus.lite code is needed on the loom side:

```bash
loom run --task-file task.json --workspace . --provider auto --json   # check the exit code
loom capabilities --json                                             # feature discovery
```

## License

MIT, see [LICENSE](LICENSE).

## Hermetic tests

Packages that can touch Nexus state carry a `hermetic_test.go` `TestMain` that points `HOME` and `NEXUS_CONFIG_HOME` at a temp dir and unsets `NEXUS_SENSITIVITY`, `LOOM_NO_AGENTS_MD`. Today that is six packages: `pkg/compact`, `pkg/engine`, `pkg/filebus`, `pkg/policy`, `pkg/providers`, `pkg/tools`. `cmd/loom` and `pkg/trace` have none because their tests touch no Nexus state; add one when a new package starts reading Nexus env or files.
