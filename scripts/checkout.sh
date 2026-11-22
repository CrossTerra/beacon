#!/usr/bin/env bash
# The check scripts/build-app.sh makes before it builds: the checkout is clean and at the commit named.

verify_clean_checkout() {
	local current changes index
	current=$(git --no-replace-objects rev-parse --verify 'HEAD^{commit}') || return 1
	changes=$(git -c core.fsmonitor=false -c core.untrackedCache=false status \
		--porcelain=v1 --untracked-files=all --ignored --ignore-submodules=none) || return 1
	index=$(git ls-files -v) || return 1
	# Skip-worktree and assume-unchanged can hide modified files from git status.
	if [[ "$current" != "$1" || -n "$changes" ]] || LC_ALL=C grep -Eq '^[Sa-z] ' <<< "$index"; then
		printf '%s\n' 'The checkout must be clean (no changes, untracked or ignored files) and at the commit being built.' >&2
		return 1
	fi
}
