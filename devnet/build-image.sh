#!/usr/bin/env bash
# Builds the NEW geth image topdisc-geth:<sha12> from the fork worktree at NEW_COMMIT (lib.sh).
#   devnet/build-image.sh [fork worktree]
set -euo pipefail
. "$(dirname "$0")/lib.sh"
SRC=${1:-$GETH_SRC}
need git go docker

check_src "$SRC"

# ci.go reads .git/HEAD directly, which is a file in a worktree, so the commit is passed explicitly.
log "building geth at $NEW_COMMIT"
(cd "$SRC" && nice -n 19 go run build/ci.go install -git-commit="$NEW_COMMIT" ./cmd/geth)

ctx=$(mktemp -d)
trap 'rm -rf "$ctx"' EXIT
cp "$DEVNET/image/Dockerfile" "$SRC/build/bin/geth" "$ctx/"
docker build -q -t "$NEW_IMAGE" --label org.opencontainers.image.revision="$NEW_COMMIT" "$ctx" >/dev/null

ver=$(docker run --rm "$NEW_IMAGE" version)
printf '%s\n' "$ver"
grep -qx "Version: $GETH_VERSION" <<<"$ver" || die "image does not report version $GETH_VERSION"
grep -qx "Git Commit: $NEW_COMMIT" <<<"$ver" || die "image does not report commit $NEW_COMMIT"
help=$(docker run --rm "$NEW_IMAGE" --help)
grep -q -- '--discovery.topic' <<<"$help" || die "image lacks --discovery.topic"
log "built $NEW_IMAGE"
