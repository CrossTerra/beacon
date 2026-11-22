#!/usr/bin/env bash
# Exercise the checkout check against a fixture repository. No signing identity, no keychain.
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=scripts/checkout.sh
source "$script_dir/checkout.sh"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
git init --quiet --template='' "$tmp/repo"
cd "$tmp/repo"
mkdir -p cmd/beacon
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
commit=$(git rev-parse HEAD)
verify_checkout "$commit"

expect_checkout_refusal() {
	local reason=$1 expected=$2
	if verify_checkout "$expected" 2>/dev/null; then
		printf 'Accepted a checkout with tracked changes or at another commit (%s).\n' "$reason" >&2
		exit 1
	fi
}

expect_checkout_refusal 'abbreviated commit' "${commit:0:12}"
expect_checkout_refusal 'wrong commit' "$(printf '%040d' 0)"
printf '// changed\n' >> cmd/beacon/main.go
expect_checkout_refusal 'unstaged changes' "$commit"
git add cmd/beacon/main.go
expect_checkout_refusal 'staged changes' "$commit"
git restore --staged --worktree cmd/beacon/main.go
rm cmd/beacon/main.go
expect_checkout_refusal 'deleted tracked file' "$commit"
git restore cmd/beacon/main.go
# Untracked and ignored files cannot reach a build from git archive: they are allowed.
touch untracked-output ignored-output
verify_checkout "$commit"
rm untracked-output ignored-output
git update-index --assume-unchanged cmd/beacon/main.go
printf '// hidden change\n' >> cmd/beacon/main.go
expect_checkout_refusal 'hidden tracked change' "$commit"
git update-index --no-assume-unchanged cmd/beacon/main.go
git restore --worktree cmd/beacon/main.go
verify_checkout "$commit"

echo "Checkout checks passed."
