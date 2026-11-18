#!/usr/bin/env bash
# Runs one devnet from params/<run>.yaml in enclave d79-<run>: inventory, snapshot hooks,
# dump and teardown. Output goes to $RUNS_DIR/<date>-d79-<run>/run.log. Long runs:
#   setsid nohup devnet/run.sh <run> </dev/null >/dev/null 2>&1 & disown
# Env: DURATION_MIN (75, minutes after genesis), SNAP_MIN ("10 30 60 75"), KEEP_ENCLAVE (0).
set -euo pipefail
. "$(dirname "$0")/lib.sh"
[ $# -eq 1 ] || die "usage: $0 <run>"
RUN=$1
PARAMS=$DEVNET/params/$RUN.yaml
ENCLAVE=d79-$RUN
OUT=$RUNS_DIR/$(date +%F)-d79-$RUN
DURATION_MIN=${DURATION_MIN:-75}
SNAP_MIN=${SNAP_MIN:-10 30 60 75}
KEEP_ENCLAVE=${KEEP_ENCLAVE:-0}
ENRDUMP=$DEVNET/bin/devp2p-topdisc

need kurtosis docker curl jq ss git
[ -f "$PARAMS" ] || die "no $PARAMS (devnet/gen-params.sh)"
[ -x "$ENRDUMP" ] || die "no $ENRDUMP (devnet/build-tools.sh)"
[ ! -e "$OUT" ] || die "$OUT exists"
[ "$(git -C "$PKG_DIR" rev-parse HEAD)" = "$PKG_COMMIT" ] || die "$PKG_DIR is not at $PKG_COMMIT"
[ -z "$(git -C "$PKG_DIR" status --porcelain)" ] || die "$PKG_DIR has local changes"
! kurtosis enclave inspect "$ENCLAVE" >/dev/null 2>&1 || die "enclave $ENCLAVE exists"
while read -r img; do
	docker image inspect "$img" >/dev/null 2>&1 || die "image $img missing (docker pull, or devnet/build-image.sh)"
done < <(awk '/_image:/ {print $2}' "$PARAMS" | sort -u)

mkdir -p "$OUT/snapshots" "$OUT/logs"
echo "$OUT"
exec >>"$OUT/run.log" 2>&1

cp "$PARAMS" "$OUT/params.yaml"
{
	echo "package $PKG_COMMIT"
	kurtosis version 2>&1 | sed 's/^/kurtosis /'
	awk '/_image:/ {print $2}' "$PARAMS" | sort -u | while read -r img; do
		echo "image $img $(docker image inspect -f '{{.Id}}' "$img")"
	done
	echo "host $(uname -r) nproc=$(nproc) $(free -g | awk '/^Mem:/ {print "mem_total_g=" $2 " mem_avail_g=" $7}')"
} >"$OUT/meta.txt"

forbidden_listeners >"$OUT/ports-baseline.txt"
[ -s "$OUT/ports-baseline.txt" ] && log "warning: forbidden ports already bound before the run: $(cat "$OUT/ports-baseline.txt")"

uuid=""
enclave_uuid() {
	kurtosis enclave inspect --full-uuids "$ENCLAVE" 2>/dev/null | awk '$1 == "UUID:" {print $2}'
}
containers() {
	docker ps -a --filter "label=com.kurtosistech.enclave-id=$uuid" --format '{{.ID}} {{.Names}}'
}

finish() {
	local rc=$?
	set +e
	[ -n "$uuid" ] || uuid=$(enclave_uuid)
	if [ -n "$uuid" ]; then
		log "dumping enclave"
		kurtosis enclave dump "$ENCLAVE" "$OUT/dump" >"$OUT/dump.log" 2>&1 || log "kurtosis enclave dump failed"
		containers | while read -r id name; do
			docker logs --timestamps "$id" >"$OUT/logs/${name%%--*}.log" 2>&1
		done
		containers | awk '{print $1}' | xargs -r docker inspect >"$OUT/docker-inspect.json"
	fi
	if [ "$KEEP_ENCLAVE" = 1 ]; then
		log "keeping enclave $ENCLAVE"
	else
		kurtosis enclave rm -f "$ENCLAVE" >/dev/null 2>&1 && log "removed enclave $ENCLAVE"
	fi
	log "done rc=$rc"
}
trap finish EXIT
trap 'exit 130' INT TERM

# published_forbidden prints forbidden host ports published by the enclave's containers,
# expanding Docker's range syntax (0.0.0.0:a-b->c-d/tcp).
published_forbidden() {
	docker ps --filter "label=com.kurtosistech.enclave-id=$uuid" --format '{{.Ports}}' | tr ',' '\n' |
		grep -oE ':[0-9]+(-[0-9]+)?->[0-9]+(-[0-9]+)?/(tcp|udp)' |
		awk -v udp=" ${FORBIDDEN_UDP[*]} " -v tcp=" ${FORBIDDEN_TCP[*]} " '{
			split($0, a, "->"); proto = a[2]; sub(/.*\//, "", proto)
			n = split(substr(a[1], 2), r, "-"); hi = n == 2 ? r[2] : r[1]
			for (p = r[1] + 0; p <= hi + 0; p++)
				if (index(proto == "udp" ? udp : tcp, " " p " ")) print p "/" proto
		}' | sort -u
}

# port_check aborts the run when the enclave publishes a forbidden host port. Host
# listeners on those ports that appear during the run are not ours: warn when they change.
host_warned=""
port_check() {
	local now pub
	now=$(forbidden_listeners | grep -vxFf "$OUT/ports-baseline.txt" || true)
	if [ -n "$now" ] && [ "$now" != "$host_warned" ]; then
		log "WARNING: new host listeners on forbidden ports (not the enclave's): $now"
	fi
	host_warned=$now
	pub=$(published_forbidden)
	if [ -n "$pub" ]; then
		log "enclave publishes forbidden host ports: $(tr '\n' ' ' <<<"$pub")"
		exit 2
	fi
}

ip_of() {
	docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}' "$1" | awk '{print $1}'
}

inventory() {
	local id name svc ip image info nid enr td kind
	echo '[]' >"$OUT/inventory.json"
	while read -r id name; do
		svc=${name%%--*}
		[[ $svc =~ ^el-[0-9]+- ]] || continue
		ip=$(ip_of "$id")
		image=$(docker inspect -f '{{.Config.Image}}' "$id")
		info=$(rpc "http://$ip:8545" admin_nodeInfo) || die "admin_nodeInfo failed on $svc ($ip)"
		nid=$(jq -r .result.id <<<"$info")
		enr=$(jq -r .result.enr <<<"$info")
		td=$("$ENRDUMP" enrdump "$enr" | awk '$1 == "\"td\"" {print $2}')
		kind=old
		[ "$image" = "$NEW_IMAGE" ] && kind=new
		if { [ "$kind" = new ] && [ -z "$td" ]; } || { [ "$kind" = old ] && [ -n "$td" ]; }; then
			die "$svc: image $image ($kind) but ENR td='$td'"
		fi
		jq --arg name "$svc" --arg kind "$kind" --arg ip "$ip" --arg id "$nid" --arg enr "$enr" \
			--arg td "$td" --arg image "$image" \
			'. + [{name: $name, kind: $kind, ip: $ip, id: $id, enr: $enr, td: (if $td == "" then null else $td end), image: $image}]' \
			"$OUT/inventory.json" >"$OUT/inventory.json.tmp"
		mv "$OUT/inventory.json.tmp" "$OUT/inventory.json"
	done < <(containers | sort -k2 -V)
	log "inventory: $(jq -r 'group_by(.kind) | map("\(.[0].kind)=\(length)") | join(" ")' "$OUT/inventory.json")"
}

# wait_until <unix time>, checking ports every minute.
wait_until() {
	local now
	while now=$(date +%s) && [ "$now" -lt "$1" ]; do
		port_check
		sleep $(($1 - now < 60 ? $1 - now : 60))
	done
	port_check
}

wait_genesis() {
	local cl ip gt n=0 head
	cl=$(containers | awk '$2 ~ /^cl-0*1-/ {print $1}')
	[ -n "$cl" ] || die "no cl-1 container"
	ip=$(ip_of "$cl")
	until gt=$(curl -sf --max-time 5 "http://$ip:4000/eth/v1/beacon/genesis" | jq -er .data.genesis_time); do
		[ $((n += 1)) -le 60 ] || die "cl-1 beacon API not up"
		sleep 5
	done
	GENESIS=$gt
	echo "$GENESIS" >"$OUT/genesis.txt"
	log "genesis $(date -u -d "@$GENESIS" +%FT%TZ)"
	wait_until "$GENESIS"
	ip=$(jq -r '.[0].ip' "$OUT/inventory.json")
	n=0
	until head=$(rpc "http://$ip:8545" eth_blockNumber | jq -r .result) && [ $((head)) -gt 0 ]; do
		[ $((n += 1)) -le 60 ] || die "el-1 produced no block 5 min after genesis"
		sleep 5
	done
	log "first block on el-1: $((head))"
}

# snapshot_hook <label>: placeholder for the M3 checks (tables, peers, heads, metrics).
snapshot_hook() {
	mkdir -p "$OUT/snapshots/$1"
	log "snapshot $1"
}

log "kurtosis run $ENCLAVE ($PARAMS)"
kurtosis run --enclave "$ENCLAVE" "$PKG_DIR" --args-file "$PARAMS" >"$OUT/kurtosis-run.log" 2>&1 &&
	rc=0 || rc=$?
uuid=$(enclave_uuid) || true
# kurtosis run can exit 0 after a Starlark error.
if [ "$rc" -ne 0 ] || grep -qE 'Error encountered running Starlark|There was an error' "$OUT/kurtosis-run.log"; then
	die "kurtosis run failed (rc=$rc), see kurtosis-run.log"
fi
[ -n "$uuid" ] || die "no UUID for enclave $ENCLAVE"
port_check
inventory
wait_genesis

END=$((GENESIS + DURATION_MIN * 60))
for m in $(tr ' ' '\n' <<<"$SNAP_MIN" | sort -n); do
	if [ "$m" -gt "$DURATION_MIN" ]; then
		log "skipping snapshot t${m}m, beyond DURATION_MIN=$DURATION_MIN"
		continue
	fi
	wait_until $((GENESIS + m * 60))
	snapshot_hook "t${m}m"
done
wait_until "$END"
log "reached ${DURATION_MIN} min after genesis"
