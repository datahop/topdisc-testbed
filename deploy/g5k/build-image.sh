#!/bin/sh
# Builds the Distem vnode image: a minimal Debian rootfs with the two node
# binaries and the hostagent as a systemd service on :9000. Needs root and
# debootstrap (Linux). Run on a Grid'5000 frontend or any Debian host.
#   deploy/g5k/build-image.sh [out.tar.gz] [debian release]
set -e
cd "$(dirname "$0")/../.."
OUT=${1:-$HOME/topdisc-vnode.tar.gz}
REL=${2:-bookworm}
ROOT=$(mktemp -d /tmp/topdisc-vnode.XXXXXX)
trap 'rm -rf "$ROOT"' EXIT

GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o out/topdisc-node ./cmd/node
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o out/hostagent ./cmd/hostagent
(cd legacy && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o ../out/topdisc-node-legacy ./cmd/node-legacy)

debootstrap --variant=minbase --include=systemd,systemd-sysv,iproute2,procps "$REL" "$ROOT" http://deb.debian.org/debian
install -d "$ROOT/opt/topdisc"
install -m 755 out/topdisc-node out/topdisc-node-legacy out/hostagent "$ROOT/opt/topdisc/"
cat > "$ROOT/etc/systemd/system/hostagent.service" <<'UNIT'
[Unit]
Description=topdisc hostagent
After=network.target
[Service]
ExecStart=/opt/topdisc/hostagent -serve :9000 -node-binary /opt/topdisc/topdisc-node -legacy-binary /opt/topdisc/topdisc-node-legacy -workdir /opt/topdisc/run
LimitNOFILE=65536
Restart=always
[Install]
WantedBy=multi-user.target
UNIT
ln -s /etc/systemd/system/hostagent.service "$ROOT/etc/systemd/system/multi-user.target.wants/hostagent.service"
echo 'net.core.rmem_max = 8388608
net.core.wmem_max = 8388608' > "$ROOT/etc/sysctl.d/90-topdisc.conf"
tar czf "$OUT" -C "$ROOT" .
echo "image: $OUT ($(du -h "$OUT" | cut -f1)); point testbed.g5k.image at file://$OUT"
