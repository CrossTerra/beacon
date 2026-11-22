#!/usr/bin/env bash
# The check scripts/build-app.sh makes before it builds. The build reads only the commit, through `git archive`,
# so untracked and ignored files (a local .e2e.env, build outputs) cannot reach it and are allowed. What is refused:
# a HEAD that is not the commit being built, and changes to tracked files, staged or not, since the build would not
# contain them and the scripts running it are tracked files.

verify_checkout() {
	local current changes index
	current=$(git --no-replace-objects rev-parse --verify 'HEAD^{commit}') || return 1
	changes=$(git -c core.fsmonitor=false status --porcelain=v1 --untracked-files=no --ignore-submodules=none) || return 1
	index=$(git ls-files -v) || return 1
	# Skip-worktree and assume-unchanged can hide changes to tracked files from git status.
	if [[ "$current" != "$1" || -n "$changes" ]] || LC_ALL=C grep -Eq '^[Sa-z] ' <<< "$index"; then
		printf '%s\n' 'The checkout must be at the commit being built, with no changes to tracked files (commit or stash them).' >&2
		return 1
	fi
}
