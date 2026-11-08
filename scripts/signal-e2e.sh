#!/usr/bin/env bash
# Runs a local Signal in Docker for Beacon's end-to-end test.
#
#   scripts/signal-e2e.sh up     build Signal's image from $SIGNAL_REF (default main) of $SIGNAL_REPO, start it with
#                                its own Postgres on a private network, create the actors and keys, and write the
#                                env file. The image is built from `git archive` of the ref, so the repository's
#                                working tree and checked-out branch do not matter and are not touched.
#   scripts/signal-e2e.sh down   remove the containers and the network
#
# Then: set -a; . ./.e2e.env; set +a; go test -race -run E2E -v ./cmd/beacon/
set -euo pipefail

repo=${SIGNAL_REPO:-$HOME/Repos/signal}
ref=${SIGNAL_REF:-main}
port=${BEACON_E2E_PORT:-18080}
envfile=${BEACON_E2E_ENV:-.e2e.env}
net=beacon-e2e
pg=beacon-e2e-postgres
app=beacon-e2e-signal
image=beacon-e2e-signal
db=postgres://signal:signal@$pg:5432/signal

down() {
	docker rm -f "$app" "$pg" >/dev/null 2>&1 || true
	docker network rm "$net" >/dev/null 2>&1 || true
}

cli() {
	docker run --rm --network "$net" -e DATABASE_URL="$db" -e NODE_ENV=development "$image" node dist/cli.mjs "$@"
}

up() {
	down
	sha=$(git -C "$repo" rev-parse --verify "$ref^{commit}")
	git -C "$repo" archive --format=tar "$sha" | docker build -q -t "$image" --build-arg GIT_SHA="$sha" - >/dev/null
	docker network create "$net" >/dev/null
	docker run -d --name "$pg" --network "$net" \
		-e POSTGRES_USER=signal -e POSTGRES_PASSWORD=signal -e POSTGRES_DB=signal postgres:17 >/dev/null
	for _ in $(seq 60); do
		docker exec "$pg" pg_isready -U signal -d signal >/dev/null 2>&1 && break
		sleep 1
	done
	cli migrate >/dev/null
	cli admin create-actor human winton "Winton" >/dev/null
	# The person's own identity in the fixture source: `by` on own acts resolves to it (DEC-27).
	cli admin attach-identity winton fixture:user:winton >/dev/null
	cli admin create-actor integration beacon-winton "Beacon for Winton" >/dev/null
	beacon_key=$(cli admin create-key beacon-winton --scope read --scope write --scope relay --identity-prefix 'fixture:*' | tail -n 1)
	reader_key=$(cli admin create-key winton --scope read --scope write | tail -n 1)
	docker run -d --name "$app" --network "$net" -p "127.0.0.1:$port:8080" -e DATABASE_URL="$db" "$image" >/dev/null
	for _ in $(seq 60); do
		curl -fsS "http://127.0.0.1:$port/readyz" >/dev/null 2>&1 && break
		sleep 1
	done
	curl -fsS "http://127.0.0.1:$port/readyz" >/dev/null
	umask 077
	cat >"$envfile" <<EOF
BEACON_E2E_SIGNAL_URL=http://127.0.0.1:$port
BEACON_E2E_KEY=$beacon_key
BEACON_E2E_READER_KEY=$reader_key
BEACON_E2E_HANDLE=@winton
EOF
	echo "Signal is at http://127.0.0.1:$port (Signal $ref at ${sha:0:7}); keys are in $envfile"
}

case ${1:-} in
up) up ;;
down) down ;;
*)
	echo "usage: $0 up|down" >&2
	exit 2
	;;
esac
