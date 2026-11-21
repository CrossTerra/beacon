#!/usr/bin/env bash
# Exercise the signing preflight against a fixture repository. This script uses no signing identity.
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=scripts/audited-build.sh
source "$script_dir/audited-build.sh"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
git init --quiet --template='' "$tmp/repo"
cd "$tmp/repo"
mkdir -p cmd/beacon "$tmp/stage"
cat > go.mod <<'EOF'
module example.invalid/signing-fixture

go 1.24.3
EOF
cat > cmd/beacon/main.go <<'EOF'
package main

import "fmt"

var version, commit string

func main() {
	fmt.Println(version, commit)
}
EOF
printf '%s\n' 'ignored-output' > .gitignore
git add .
git -c user.name=Fixture -c user.email=fixture@example.invalid -c commit.gpgSign=false \
	-c core.hooksPath=/dev/null commit --quiet -m Fixture
audited_commit=$(git rev-parse HEAD)
verify_audited_checkout "$audited_commit"

expect_checkout_refusal() {
	local reason=$1 expected=$2
	if verify_audited_checkout "$expected" 2>/dev/null; then
		printf 'Accepted an unaudited checkout (%s).\n' "$reason" >&2
		exit 1
	fi
}

expect_checkout_refusal 'abbreviated commit' "${audited_commit:0:12}"
expect_checkout_refusal 'wrong commit' "$(printf '%040d' 0)"
printf '// changed\n' >> cmd/beacon/main.go
expect_checkout_refusal 'unstaged changes' "$audited_commit"
git add cmd/beacon/main.go
expect_checkout_refusal 'staged changes' "$audited_commit"
git restore --staged --worktree cmd/beacon/main.go
touch untracked-output
expect_checkout_refusal 'untracked output' "$audited_commit"
rm untracked-output
touch ignored-output
expect_checkout_refusal 'ignored output' "$audited_commit"
rm ignored-output
git update-index --assume-unchanged cmd/beacon/main.go
printf '// hidden change\n' >> cmd/beacon/main.go
expect_checkout_refusal 'hidden tracked change' "$audited_commit"
git update-index --no-assume-unchanged cmd/beacon/main.go
git restore --worktree cmd/beacon/main.go
verify_audited_checkout "$audited_commit"

stage_audited_build "$audited_commit" "$tmp/stage"
artifact="$tmp/stage/beacon"
if [[ $("$artifact") != "dev $audited_commit" ]]; then
	echo "The staged fixture is missing the audited commit." >&2
	exit 1
fi
digest=$(artifact_sha256 "$artifact")
verify_artifact "$artifact" "$digest"
printf 'changed after staging' >> "$artifact"
if verify_artifact "$artifact" "$digest" 2>/dev/null; then
	echo "Accepted changed staged bytes." >&2
	exit 1
fi
echo "Audited build checks passed."
