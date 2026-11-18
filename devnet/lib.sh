# Shared pins, paths and helpers for the devnet scripts; sourced, not run.
# shellcheck shell=bash disable=SC2034

DEVNET=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
TB=$(dirname "$DEVNET")

NEW_COMMIT=${NEW_COMMIT:-3b8bef9e31d8e94e4407fd4499a1bd098cbadb0f}
NEW_IMAGE=${NEW_IMAGE:-topdisc-geth:${NEW_COMMIT:0:12}}
OLD_IMAGE=${OLD_IMAGE:-ethereum/client-go:v1.17.10}
CL_IMAGE=${CL_IMAGE:-sigp/lighthouse:v8.3.3}
OLD_DEVP2P_VERSION=v1.17.10
GETH_VERSION=1.17.10-stable

PKG_DIR=${PKG_DIR:-$HOME/workspace/datahop/ethereum-package}
PKG_COMMIT=d3014d6d23d683362025b9e23a0d69de49e77cca
GETH_SRC=${GETH_SRC:-$HOME/workspace/datahop/geth-d79}
RUNS_DIR=${RUNS_DIR:-$HOME/workspace/datahop/runs}

export GOFLAGS=${GOFLAGS:--p=16}

# Host ports the devnet must never bind or publish.
FORBIDDEN_UDP=(9000 9001 30303)
FORBIDDEN_TCP=(8545 5052)

log() { printf '%s %s\n' "$(date -u +%FT%TZ)" "$*" >&2; }
die() { log "error: $*"; exit 1; }

need() {
	local c
	for c in "$@"; do
		command -v "$c" >/dev/null || die "missing command: $c"
	done
}

# check_src <fork worktree> dies unless it is at NEW_COMMIT with no changes or untracked files.
check_src() {
	local head
	head=$(git -C "$1" rev-parse HEAD)
	[ "$head" = "$NEW_COMMIT" ] || die "$1 is at $head, not NEW_COMMIT $NEW_COMMIT"
	[ -z "$(git -C "$1" status --porcelain --untracked-files=all)" ] || die "$1 has local changes or untracked files"
}

# rpc <url> <method> [params-json]
rpc() {
	curl -sf --max-time 5 -H 'Content-Type: application/json' \
		-d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"$2\",\"params\":${3:-[]}}" "$1"
}

# forbidden_listeners prints host sockets bound to a forbidden port.
forbidden_listeners() {
	local re_u re_t
	re_u=$(IFS='|'; echo "${FORBIDDEN_UDP[*]}")
	re_t=$(IFS='|'; echo "${FORBIDDEN_TCP[*]}")
	ss -Hlnup | awk -v re=":($re_u)\$" '$4 ~ re' || true
	ss -Hlntp | awk -v re=":($re_t)\$" '$4 ~ re' || true
}
