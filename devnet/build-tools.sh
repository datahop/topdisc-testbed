#!/usr/bin/env bash
# Builds the host-side tools into devnet/bin: stock devp2p (module cache, offline),
# fork devp2p from the fork worktree, and the testbed crawler.
#   devnet/build-tools.sh [fork worktree]
set -euo pipefail
. "$(dirname "$0")/lib.sh"
SRC=${1:-$GETH_SRC}
BIN=$DEVNET/bin
need go git
check_src "$SRC"
mkdir -p "$BIN"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
# GOPROXY=off fails on the toolchain and deprecation lookups; the module cache as a file
# proxy keeps the build offline.
GOTOOLCHAIN=go1.25.0 GOPROXY=file://$(go env GOMODCACHE)/cache/download GOBIN=$tmp \
	nice -n 19 go install "github.com/ethereum/go-ethereum/cmd/devp2p@$OLD_DEVP2P_VERSION"
mv "$tmp/devp2p" "$BIN/devp2p-$OLD_DEVP2P_VERSION"

(cd "$SRC" && nice -n 19 go build -o "$BIN/devp2p-topdisc" ./cmd/devp2p)
(cd "$TB" && nice -n 19 go build -o "$BIN/crawl" ./cmd/crawl)

log "fork devp2p from $NEW_COMMIT"
ls -l "$BIN"
