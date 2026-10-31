#!/usr/bin/env bash
# Publish a squashed single-commit snapshot of a GitLab tag to the GitHub mirror.
# DRY RUN by default: prints the commands. Pushes only with --push. See docs/release.md.
set -eu
tag=${1:?usage: $0 vX.Y.Z [--first] [--push]}; shift
first=0; push=0
for a in "$@"; do case $a in --first) first=1;; --push) push=1;; *) echo "unknown arg $a" >&2; exit 2;; esac; done
case $tag in v[0-9]*.[0-9]*.[0-9]*) ;; *) echo "tag must be vX.Y.Z" >&2; exit 2;; esac
git rev-parse -q --verify "refs/tags/$tag^{tree}" >/dev/null || { echo "tag $tag not found locally" >&2; exit 1; }
if [ $first = 1 ]; then msg="Initial public release of loom $tag"; parent=""; else msg="Release loom $tag"; parent="-p github/main"; fi
run() { echo "+ $*"; [ $push = 1 ] && "$@" || true; }
echo "# mode: $([ $push = 1 ] && echo PUSH || echo dry-run)"
git remote get-url github >/dev/null 2>&1 || run git remote add github https://github.com/choilab-jefferson/nexus.loom.git
[ $first = 1 ] || run git fetch github main
if [ $push = 1 ]; then
  sha=$(GIT_AUTHOR_NAME="Wookjin Choi" GIT_AUTHOR_EMAIL=taznux@gmail.com \
        GIT_COMMITTER_NAME="Wookjin Choi" GIT_COMMITTER_EMAIL=taznux@gmail.com \
        git commit-tree "$tag^{tree}" $parent -m "$msg")
else
  sha='<new-commit-sha>'
  echo "+ GIT_AUTHOR_NAME='Wookjin Choi' GIT_AUTHOR_EMAIL=taznux@gmail.com GIT_COMMITTER_NAME='Wookjin Choi' GIT_COMMITTER_EMAIL=taznux@gmail.com git commit-tree $tag^{tree} $parent -m '$msg'"
fi
run git push github "$sha:refs/heads/main"
run git tag -f "gh-$tag" "$sha"
run git push github "refs/tags/gh-$tag:refs/tags/$tag"
