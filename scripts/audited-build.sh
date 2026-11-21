#!/usr/bin/env bash
# Build and verification functions for build-local.sh. The signing workflow is in docs/signing.md.

verify_audited_checkout() {
	local current changes index
	current=$(git --no-replace-objects rev-parse --verify 'HEAD^{commit}') || return 1
	changes=$(git -c core.fsmonitor=false -c core.untrackedCache=false status \
		--porcelain=v1 --untracked-files=all --ignored --ignore-submodules=none) || return 1
	index=$(git ls-files -v) || return 1
	# Skip-worktree and assume-unchanged can hide modified files from git status.
	if [[ "$current" != "$1" || -n "$changes" ]] || LC_ALL=C grep -Eq '^[Sa-z] ' <<< "$index"; then
		printf '%s\n' 'The checkout must be clean and match the full audited commit. See docs/signing.md.' >&2
		return 1
	fi
}

stage_audited_build() {
	local audited_commit=$1 stage=$2
	mkdir "$stage/src" || return 1
	git --no-replace-objects archive "$audited_commit" | tar -x -C "$stage/src" || return 1
	(
		cd "$stage/src" || exit 1
		export GOENV=off GOWORK=off GOTOOLCHAIN=local GOFLAGS='' CGO_ENABLED=1
		go mod verify || exit 1
		go build -trimpath -buildvcs=false \
			-ldflags "-X main.version=dev -X main.commit=$audited_commit" \
			-o "$stage/beacon" ./cmd/beacon
	)
}

artifact_sha256() {
	shasum -a 256 < "$1" | cut -d ' ' -f 1
}

verify_artifact() {
	local actual
	actual=$(artifact_sha256 "$1") || return 1
	if [[ "$actual" != "$2" ]]; then
		printf '%s\n' 'Artifact checksum mismatch. See docs/signing.md.' >&2
		return 1
	fi
}
