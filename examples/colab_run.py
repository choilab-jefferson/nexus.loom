#!/usr/bin/env -S colab run --timeout 900
"""Run one loom task on a fresh Colab VM and print the result.

    colab run --timeout 900 --env "ANTHROPIC_API_KEY=$ANTHROPIC_API_KEY" \
        examples/colab_run.py https://github.com/choilab-jefferson/nexus.loom/releases/download v0.1.0 "your task"

Stdlib only. Exit code is loom's exit code. Unverified in real Colab.
"""
import hashlib, os, subprocess, sys, urllib.request

base, tag, task = sys.argv[1], sys.argv[2], sys.argv[3]
asset = "loom-linux-amd64"


def get(name):
    with urllib.request.urlopen(f"{base}/{tag}/{name}") as r:
        return r.read()


binary = get(asset)
want = [l.split()[0] for l in get("checksums.txt").decode().splitlines() if l.endswith(asset)]
if not want or hashlib.sha256(binary).hexdigest() != want[0]:
    sys.exit("checksum mismatch")
open("/content/loom", "wb").write(binary)
os.chmod("/content/loom", 0o755)
os.makedirs("/content/work", exist_ok=True)

p = subprocess.run(["/content/loom", "-p", task, "--workspace", "/content/work",
                    "--trace", "/content/trace.jsonl", "--json"])
print("--- trace tail ---")
print("".join(open("/content/trace.jsonl").readlines()[-20:]))
sys.exit(p.returncode)
