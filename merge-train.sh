#!/bin/sh
# Rebuild branch merge-train: a base (upstream main) plus every fix branch merged in the planned
# order (MERGE-PLAN.md). The script lives on branch dev-gw, which merge-train does not contain:
# run it by path from a worktree that has merge-train checked out (do not copy it in there,
# an untracked copy makes the clean-tree check refuse):
#
#   git worktree add ../merge-train merge-train && cd ../merge-train
#   sh ../OpenUsenetServer/merge-train.sh            # base abe8543, upstream main of the plan
#   git fetch https://github.com/tgeek77/OpenUsenetServer main
#   sh ../OpenUsenetServer/merge-train.sh FETCH_HEAD # after an upstream merge
#
# It resets merge-train to the base, so it refuses to run with uncommitted or untracked files or on any
# other branch. Branches the base already contains (merged upstream) are skipped. Conflicts git
# rerere has a recorded resolution for (rr-cache of this clone) are replayed; any other conflict
# stops it.
set -e
BASE=${1:-abe8543}
ORDER="fix/authinfo-codes fix/go-vet-self-assign fix/server-test-watchdog fix/post-auth-reply
fix/takethis-reply-text fix/hdr-no-current-article fix/posting-account fix/listener-race
fix/import-job-race fix/inbound-peer-by-hostname fix/example-pathhost fix/mode-stream-fallback
fix/feed-single-drain fix/feed-persistent-conn fix/peer-password-random fix/keep-article-bytes fix/over-msgid-bytes-on-kab fix/xref-served
fix/store-articles-bytea"

if [ "$(git branch --show-current)" != merge-train ]; then
	echo "check out branch merge-train in this worktree first" >&2
	exit 1
fi
if [ -n "$(git status --porcelain --untracked-files=all)" ]; then
	echo "uncommitted or untracked files; not resetting merge-train" >&2
	git status --short >&2
	exit 1
fi
base=$(git rev-parse --verify "$BASE^{commit}")
git reset -q --hard "$base"
for b in $ORDER; do
	if git merge-base --is-ancestor "$b" "$base"; then
		echo "merged   $b (in the base)"
		continue
	fi
	if git -c rerere.enabled=true merge -q --no-ff --no-edit --rerere-autoupdate "$b" >/dev/null 2>&1; then
		echo "ok       $b $(git rev-parse --short "$b")"
	elif [ -z "$(git diff --name-only --diff-filter=U)" ]; then
		# git rerere replayed a recorded resolution (see MERGE-PLAN.md) for every file
		git commit -q --no-edit
		echo "resolved $b $(git rev-parse --short "$b") (recorded resolution)"
	else
		echo "CONFLICT $b: $(git diff --name-only --diff-filter=U | tr '\n' ' ')"
		git merge --abort
		exit 1
	fi
done
go build ./...
go vet ./... && echo "vet clean"
