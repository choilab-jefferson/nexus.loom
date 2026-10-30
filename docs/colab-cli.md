# Loom with the Colab CLI

Drive a Colab VM from your terminal with the official [`google-colab-cli`](https://pypi.org/project/google-colab-cli/) (`colab` command, Apache-2.0). Flags below come from `colab <cmd> --help` and the CLI source (v0.7.5). Nothing here was run against a real Colab runtime: all in-Colab steps are **unverified**. Notebook users: see [colab-quickstart.md](colab-quickstart.md).

Limits: the CLI supports **Linux and macOS only** (no Windows), needs a Google login you set up yourself (`--auth adc|oauth2`), and `colab exec` / `colab run` default to a **30 s timeout**, so always pass `--timeout`.

Release URLs and tags are as in the Colab quickstart.

## Warning: keys and history

`colab exec --env KEY=VALUE` prepends `os.environ[...] = '<value>'` to your code, and `colab exec` logs the full code to `~/.config/colab-cli/history/<session>.jsonl` (also exported by `colab log`). **A key passed with `exec --env` or typed into exec code is stored in plaintext there.** Safer options:

- `colab run --env ...`: `run` does not write an execution record to that history (per source; verify), but the value is in your local process arguments. Use `"KEY=$KEY"` so shell history keeps `$KEY`, not the value.
- For `exec`, upload a key file (upload logs only the paths), read it remotely, then delete it, as below.

## A. Interactive session

```bash
# Linux binary + task (task.json schema: docs/spec/loom-v0.1.md)
curl -fsSL -o loom https://github.com/choilab-jefferson/nexus.loom/releases/download/v0.1.0/loom-linux-amd64
curl -fsSL -o checksums.txt https://github.com/choilab-jefferson/nexus.loom/releases/download/v0.1.0/checksums.txt
grep loom-linux-amd64 checksums.txt | sed 's/loom-linux-amd64/loom/' | sha256sum -c -

colab new -s lm                        # add --gpu T4 for a GPU runtime
colab upload -s lm loom /content/loom
colab upload -s lm task.json /content/task.json

# key file, never echoed: mode 600, deleted after use
umask 077; printf 'ANTHROPIC_API_KEY=%s\n' "$ANTHROPIC_API_KEY" > .keys
colab upload -s lm .keys /content/.keys && rm .keys

cat > job.py <<'PY'
import os, subprocess, sys
for l in open("/content/.keys"):
    k, _, v = l.strip().partition("="); os.environ[k] = v
os.chmod("/content/loom", 0o755); os.makedirs("/content/work", exist_ok=True)
p = subprocess.run(["/content/loom", "run", "--task-file", "/content/task.json",
    "--workspace", "/content/work", "--provider", "anthropic",
    "--trace", "/content/trace.jsonl", "--json"])
os.remove("/content/.keys")
print("loom exit code:", p.returncode)
if p.returncode: raise SystemExit(p.returncode)
PY
colab exec -s lm --timeout 900 -f job.py     # code (without the key) is logged
colab download -s lm /content/trace.jsonl ./trace.jsonl
colab rm -s lm /content/.keys 2>/dev/null; colab stop -s lm
```

Exit code: whether `colab exec` returns non-zero when the cell raises is unverified. The snippet prints loom's code and raises, so check the output. Remote `/content/work` is the workspace; download results the same way as the trace.

## B. One-shot ephemeral run

`colab run` makes a fresh VM, runs one local Python file, and stops the VM. Per the CLI source, `sys.exit(N)` in the script becomes the command's exit code, so loom's exit code propagates. `examples/colab_run.py` downloads loom, verifies the checksum, runs a task and prints the trace tail:

```bash
colab run --timeout 900 --env "ANTHROPIC_API_KEY=$ANTHROPIC_API_KEY" \
    examples/colab_run.py https://github.com/choilab-jefferson/nexus.loom/releases/download v0.1.0 "fix the failing test"
echo "loom exit: $?"
```

`colab run` has no download step, so the script prints the trace. To fetch files, add `--keep -s name`, then `colab download -s name /content/trace.jsonl .` and `colab stop -s name`.

## C. GPU and Ollama on the VM

```bash
colab new -s gpu --gpu T4              # T4, L4, G4, H100, A100 (tier-dependent)
cat > ollama_up.py <<'PY'
import subprocess, time
subprocess.run("curl -fsSL https://ollama.com/install.sh | sh", shell=True, check=True)
subprocess.Popen(["ollama", "serve"]); time.sleep(5)
subprocess.run(["ollama", "pull", "qwen2.5-coder:7b"], check=True)
PY
colab exec -s gpu --timeout 1800 -f ollama_up.py
```

Then run loom as in A with `--provider ollama --model qwen2.5-coder:7b` and no key file. The model tag is a guess; pick one that fits the GPU.
