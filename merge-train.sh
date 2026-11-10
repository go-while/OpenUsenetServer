#!/bin/sh
# Rebuild branch merge-train: a base (upstream main) plus every fix branch merged in the planned
# order (MERGE-PLAN.md). Run it in a worktree that has branch merge-train checked out:
#
#   git worktree add ../merge-train merge-train && cd ../merge-train
#   ./merge-train.sh                    # base: abe8543, upstream main when the plan was made
#   git fetch https://github.com/tgeek77/OpenUsenetServer main && ./merge-train.sh FETCH_HEAD
#
# It resets merge-train to the base, so it refuses to run with uncommitted changes or on any
# other branch. Branches the base already contains (merged upstream) are skipped. Stops at the
# first conflict.
set -e
BASE=${1:-abe8543}
ORDER="fix/authinfo-codes fix/go-vet-self-assign fix/server-test-watchdog fix/post-auth-reply
fix/takethis-reply-text fix/hdr-no-current-article fix/posting-account fix/listener-race
fix/import-job-race fix/inbound-peer-by-hostname fix/mode-stream-fallback fix/feed-single-drain
fix/peer-password-random fix/keep-article-bytes fix/over-msgid-bytes-on-kab fix/store-articles-bytea"

if [ "$(git branch --show-current)" != merge-train ]; then
	echo "check out branch merge-train in this worktree first" >&2
	exit 1
fi
if ! git diff --quiet || ! git diff --cached --quiet; then
	echo "uncommitted changes; not resetting merge-train" >&2
	exit 1
fi
base=$(git rev-parse --verify "$BASE^{commit}")
git reset -q --hard "$base"
for b in $ORDER; do
	if git merge-base --is-ancestor "$b" "$base"; then
		echo "merged   $b (in the base)"
		continue
	fi
	if git merge -q --no-ff --no-edit "$b" >/dev/null 2>&1; then
		echo "ok       $b $(git rev-parse --short "$b")"
	else
		echo "CONFLICT $b: $(git diff --name-only --diff-filter=U | tr '\n' ' ')"
		git merge --abort
		exit 1
	fi
done
go build ./...
go vet ./... && echo "vet clean"
