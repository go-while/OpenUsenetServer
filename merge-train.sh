#!/bin/sh
# Rebuild branch merge-train: upstream main abe8543 plus every fix branch merged in the planned
# order (MERGE-PLAN.md). Run in a worktree of branch merge-train. Stops at the first conflict.
set -e
ORDER="fix/authinfo-codes fix/go-vet-self-assign fix/server-test-watchdog fix/post-auth-reply
fix/takethis-reply-text fix/hdr-no-current-article fix/posting-account fix/listener-race
fix/import-job-race fix/inbound-peer-by-hostname fix/mode-stream-fallback fix/feed-single-drain
fix/peer-password-random fix/keep-article-bytes fix/over-msgid-bytes-on-kab fix/store-articles-bytea"
git checkout -q merge-train
git reset -q --hard abe8543
for b in $ORDER; do
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
