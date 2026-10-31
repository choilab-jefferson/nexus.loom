#!/usr/bin/env python3
"""Provider switch with loom: ONE agent, same task, different model providers.

Loom is a single-agent CLI. This script does not orchestrate anything: it runs
the same `loom -p ... --json` task once per provider to show that switching
providers is just the `--provider` flag. Multi-agent orchestration belongs to nexus.

    make build
    python3 examples/provider_switch.py                    # anthropic, gemini; mock if no key
    python3 examples/provider_switch.py --mock             # never touches the network
    python3 examples/provider_switch.py --providers anthropic,gemini,ollama

Stdlib only. Runs in an empty temp workspace, so no local files are sent.
API keys are resolved by loom itself and are never read or printed here.
"""
import argparse, json, os, subprocess, sys, tempfile

LOOM = os.environ.get("LOOM_BIN", os.path.join(os.path.dirname(__file__), "..", "bin", "loom"))
MODELS = {"anthropic": "claude-haiku-5-5", "gemini": "gemini-3.8-flash"}  # cheap defaults


def run(provider, prompt, ws):
    """One loom run; return (ok, text, tokens)."""
    cmd = [LOOM, "-p", prompt, "--provider", provider, "--workspace", ws, "--json",
           "--no-agents-md", "--no-stm", "--max-turns", "2", "--max-total-tokens", "6000"]
    if provider in MODELS:
        cmd += ["--model", MODELS[provider]]
    p = subprocess.run(cmd, capture_output=True, text=True, timeout=120)
    try:
        r = json.loads(p.stdout)
        return p.returncode == 0, r.get("output", "").strip(), r.get("usage", {}).get("total_tokens", "?")
    except ValueError:
        return False, (p.stderr.strip()[:200] or "no output"), "?"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--providers", default="anthropic,gemini")
    ap.add_argument("--task", default="Reply with one sentence: what is Go's goroutine?")
    ap.add_argument("--mock", action="store_true", help="use the mock provider for every entry (no network, no keys)")
    a = ap.parse_args()

    failed = 0
    with tempfile.TemporaryDirectory() as ws:
        for name in a.providers.split(","):
            used = "mock" if a.mock else name
            ok, text, tokens = run(used, a.task, ws)
            if not ok and not a.mock:  # e.g. missing key: fall back to the mock path
                print(f"[{name}] unavailable ({text}); falling back to mock")
                used = "mock"
                ok, text, tokens = run(used, a.task, ws)
            label = name if used == name else f"{name} (mock)"
            print(f"[{label}] ok={ok} tokens={tokens}\n    {text}")
            failed += not ok
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
