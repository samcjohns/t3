#!/usr/bin/env bash
# Pulls the latest code, then builds and (re)starts the whole t3 stack:
# Postgres, the server and both market makers.
#
# Usage: ./deploy.sh [--skip-pull]
#
# Secrets come from the environment or a .env file next to this script
# (git-ignored), e.g.:
#   T3_ADMIN_PASSWORD=...
#   T3_MARKET_MAKER_PASSWORD=...
#   T3_PORT=8080          # host port, when not using an edge network
#   T3_EDGE_NETWORK=edge  # optional: serve via a proxy/tunnel on this network
#   T3_PUBLIC_URL=https://t3.example.com
set -euo pipefail

# Everything runs from main, at the bottom, so bash has read the whole script
# before `git pull` can rewrite this file mid-run.
main() {
	cd "$(dirname "$0")"

	local pull=true
	for arg in "$@"; do
		case "$arg" in
		--skip-pull) pull=false ;;
		-h | --help)
			sed -n '2,14p' "$0"
			exit 0
			;;
		*) die "unknown argument: $arg (try --help)" ;;
		esac
	done

	command -v git >/dev/null || die "git is not installed"
	docker compose version >/dev/null 2>&1 || die "docker compose is not available"

	if [[ -f .env ]]; then
		set -a
		# shellcheck disable=SC1091
		source .env
		set +a
	fi
	for var in T3_ADMIN_PASSWORD T3_MARKET_MAKER_PASSWORD; do
		if [[ -z "${!var:-}" ]]; then
			warn "$var is not set; using the development default from docker-compose.yml"
		fi
	done

	compose=(docker compose -f docker-compose.yml)
	local url="http://localhost:${T3_PORT:-8080}"
	if [[ -n "${T3_EDGE_NETWORK:-}" ]]; then
		if ! docker network inspect "$T3_EDGE_NETWORK" >/dev/null 2>&1; then
			step "Creating shared network $T3_EDGE_NETWORK"
			docker network create "$T3_EDGE_NETWORK" >/dev/null
		fi
		# Only connections from the edge network may name the real client.
		T3_TRUSTED_PROXIES=$(docker network inspect "$T3_EDGE_NETWORK" \
			-f '{{range .IPAM.Config}}{{.Subnet}},{{end}}')
		export T3_TRUSTED_PROXIES=${T3_TRUSTED_PROXIES%,}
		compose+=(-f docker-compose.edge.yml)
		url="${T3_PUBLIC_URL:-http://t3-server:8080 on the $T3_EDGE_NETWORK network}"
	fi

	if $pull; then
		step "Pulling latest code ($(git rev-parse --abbrev-ref HEAD))"
		git pull --ff-only
	fi
	step "Deploying $(git rev-parse --short HEAD): $(git log -1 --format=%s)"

	step "Building images"
	"${compose[@]}" build --pull

	step "Starting containers"
	"${compose[@]}" up -d --remove-orphans

	wait_healthy t3-server

	"${compose[@]}" ps --format 'table {{.Name}}\t{{.Status}}'
	step "t3 is up at $url"
}

# wait_healthy waits for a container's health check (the server's /readyz).
wait_healthy() {
	step "Waiting for $1 to report healthy"
	local status
	for _ in $(seq 60); do
		status=$(docker inspect -f '{{.State.Health.Status}}' "$1" 2>/dev/null || echo missing)
		[[ "$status" == healthy ]] && return 0
		sleep 1
	done
	warn "$1 is $status after 60s; recent logs:"
	"${compose[@]}" logs --tail 40 server
	exit 1
}

step() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
warn() { printf '\033[33mwarning:\033[0m %s\n' "$*" >&2; }
die() {
	printf '\033[31merror:\033[0m %s\n' "$*" >&2
	exit 1
}

main "$@"
