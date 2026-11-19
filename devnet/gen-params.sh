#!/usr/bin/env bash
# Writes params/<run>.yaml for the ethereum-package: participant 0 is the only bootnode
# (BOOT_KIND), then the remaining NEW and OLD participants as two count groups.
#   devnet/gen-params.sh <run> <n_new> <n_old> <new|old> [old image]
set -euo pipefail
. "$(dirname "$0")/lib.sh"
[ $# -ge 4 ] || die "usage: $0 <run> <n_new> <n_old> <new|old> [old image]"
RUN=$1 N_NEW=$2 N_OLD=$3 BOOT_KIND=$4
OLD_IMAGE=${5:-$OLD_IMAGE}
case $BOOT_KIND in
new) [ "$N_NEW" -ge 1 ] || die "BOOT_KIND new needs n_new >= 1" ;;
old) [ "$N_OLD" -ge 1 ] || die "BOOT_KIND old needs n_old >= 1" ;;
*) die "BOOT_KIND must be new or old" ;;
esac

# participant <kind> <count> [boot]
participant() {
	local kind=$1 count=$2 boot=${3:-} image params=() list="" p
	[ "$count" -gt 0 ] || return 0
	if [ "$kind" = new ]; then
		image=$NEW_IMAGE
		params=("--discovery.topic" "'--log.vmodule=p2p/discover/topicindex/*=5'" "--identity=topdisc")
	else
		image=$OLD_IMAGE
	fi
	# Without an explicit empty list geth falls back to the mainnet discv5 bootnodes.
	[ -n "$boot" ] && params+=("--bootnodes=")
	for p in "${params[@]}"; do
		list+="${list:+, }\"$p\""
	done
	cat <<-YAML
	  - el_type: geth
	    el_image: $image
	    el_log_level: debug
	    el_extra_params: [$list]
	    cl_type: lighthouse
	    cl_image: $CL_IMAGE
	    vc_type: lighthouse
	    vc_image: $CL_IMAGE
	    validator_count: 32
	    count: $count
	YAML
	# Fulu is at genesis and 32 validators per node is below the package's 128 threshold,
	# so the package requires one supernode; it is always participant 0's beacon node.
	[ -n "$boot" ] && echo "    supernode: true"
	return 0
}

out=$DEVNET/params/$RUN.yaml
mkdir -p "$(dirname "$out")"
{
	echo "# devnet/gen-params.sh $RUN $N_NEW $N_OLD $BOOT_KIND $OLD_IMAGE"
	echo "participants:"
	if [ "$BOOT_KIND" = new ]; then
		participant new 1 boot
		participant new $((N_NEW - 1))
		participant old "$N_OLD"
	else
		participant old 1 boot
		participant new "$N_NEW"
		participant old $((N_OLD - 1))
	fi
	echo "additional_services: []"
	echo "parallel_keystore_generation: true"
} >"$out"
log "wrote $out"
