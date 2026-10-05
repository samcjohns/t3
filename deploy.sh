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
#   T3_PORT=8080
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
			sed -n '2,12p' "$0"
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

	if $pull; then
		step "Pulling latest code ($(git rev-parse --abbrev-ref HEAD))"
		git pull --ff-only
	fi
	step "Deploying $(git rev-parse --short HEAD): $(git log -1 --format=%s)"

	step "Building images"
	docker compose build --pull

	step "Starting containers"
	docker compose up -d --remove-orphans

	wait_ready "http://localhost:${T3_PORT:-8080}/readyz"

	docker compose ps --format 'table {{.Name}}\t{{.Status}}'
	step "t3 is up at http://localhost:${T3_PORT:-8080}"
}

wait_ready() {
	local url=$1
	step "Waiting for $url"
	for _ in $(seq 60); do
		if curl -fsS -o /dev/null "$url" 2>/dev/null; then
			return 0
		fi
		sleep 1
	done
	warn "server did not become ready within 60s; recent logs:"
	docker compose logs --tail 40 server
	exit 1
}

step() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
warn() { printf '\033[33mwarning:\033[0m %s\n' "$*" >&2; }
die() {
	printf '\033[31merror:\033[0m %s\n' "$*" >&2
	exit 1
}

main "$@"
