# Releasing loom (GitHub public snapshot)

GitLab (`origin`, `nexus/nexus.loom`) keeps full history. GitHub
(`choilab-jefferson/nexus.loom`) is a public mirror holding **one squashed root
commit per release**. Only the owner (`wxc151`, Wookjin Choi) may publish.

## Procedure

1. Merge to GitLab `main`, then tag it `vX.Y.Z` on GitLab (`git tag vX.Y.Z && git push origin vX.Y.Z`).
2. Snapshot source = the **tree of that tag**. Everything tracked is included,
   notably `.github/` (`ci.yml`, `release.yml`) and `LICENSE` (MIT). Untracked files are not.
3. Remote (once): `git remote add github https://github.com/choilab-jefferson/nexus.loom.git`
4. Build the commit with no parents (first release) via `git commit-tree`:
   - author and committer: `Wookjin Choi <taznux@gmail.com>` (same as root commit `4421e7fc`)
   - message: first release `Initial public release of loom vX.Y.Z`; later `Release loom vX.Y.Z`
5. Push it to GitHub `main`, then push the tag `vX.Y.Z` pointing at that commit.
   The tag push triggers `.github/workflows/release.yml`, which builds
   `loom-{linux,darwin}-{amd64,arm64}` + `checksums.txt` and runs `gh release create`.

`scripts/publish-snapshot.sh vX.Y.Z [--first] [--push]` does steps 3-5. It is a
**dry run** (prints commands) unless `--push` is given.

```bash
scripts/publish-snapshot.sh v0.1.2            # review output
scripts/publish-snapshot.sh v0.1.2 --push     # owner only
```

## Notes

- Existing history: GitHub `main` is the single commit `4421e7fc` (v0.1.0, 0 parents).
- Squash policy for later releases is **unverified**: the script chains each new snapshot
  onto `github/main` (one commit per release, no GitLab history). Use `--first` for a
  parentless commit (a force-push is then needed; avoid unless intended).
- The script's `--push` path is untested. Whether GitHub already has `v0.1.1` is unverified.
- The same procedure applies to nexus.lite (nexus/nexus.lite#15).
