#!/usr/bin/env bash
# Writes params/<run>.yaml for the ethereum-package: participant 0 is the only bootnode
# (BOOT_KIND), then the remaining NEW and OLD participants as count groups.
#   [VAL_NEW=k] [VAL_OLD=k] devnet/gen-params.sh <run> <n_new> <n_old> <new|old> [old image]
# VAL_NEW / VAL_OLD: participants of each kind with 32 validators (default all of them),
# participant 0 included; the others run an EL and a beacon node only.
set -euo pipefail
. "$(dirname "$0")/lib.sh"
[ $# -ge 4 ] || die "usage: $0 <run> <n_new> <n_old> <new|old> [old image]"
RUN=$1 N_NEW=$2 N_OLD=$3 BOOT_KIND=$4
OLD_IMAGE=${5:-$OLD_IMAGE}
# Bash arithmetic reads a leading 0 as octal, and its errors do not stop the script.
isnum() { [[ $1 =~ ^(0|[1-9][0-9]*)$ ]]; }
isnum "$N_NEW" && isnum "$N_OLD" || die "n_new and n_old must be numbers without leading zeros"
case $BOOT_KIND in
new) [ "$N_NEW" -ge 1 ] || die "BOOT_KIND new needs n_new >= 1" ;;
old) [ "$N_OLD" -ge 1 ] || die "BOOT_KIND old needs n_old >= 1" ;;
*) die "BOOT_KIND must be new or old" ;;
esac
VAL_NEW_SET=${VAL_NEW+set} VAL_OLD_SET=${VAL_OLD+set}
VAL_NEW=${VAL_NEW:-$N_NEW} VAL_OLD=${VAL_OLD:-$N_OLD}
for k in NEW OLD; do
	n=N_$k v=VAL_$k
	isnum "${!v}" || die "$v must be a number without leading zeros, got '${!v}'"
	if [ "${!n}" -eq 0 ]; then
		[ "${!v}" -eq 0 ] || die "$v=${!v} but n_${k,,} is 0"
	elif [ "${!v}" -lt 1 ] || [ "${!v}" -gt "${!n}" ]; then
		die "$v=${!v} must be between 1 and n_${k,,}=${!n}"
	fi
done
# At least two validators per slot of the 32-slot epoch.
if [ -n "$VAL_NEW_SET$VAL_OLD_SET" ] && [ $((VAL_NEW + VAL_OLD)) -lt 2 ]; then
	die "$(((VAL_NEW + VAL_OLD) * 32)) validators, need >= 64 (VAL_NEW + VAL_OLD >= 2)"
fi

# participant <kind> <count> <validators per node> [boot]
participant() {
	local kind=$1 count=$2 validators=$3 boot=${4:-} image params=() list="" p
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
	    validator_count: $validators
	    count: $count
	YAML
	# Fulu is at genesis and 32 validators per node is below the package's 128 threshold,
	# so the package requires one supernode; it is always participant 0's beacon node.
	[ -n "$boot" ] && echo "    supernode: true"
	return 0
}

# group <kind> <count> <validating>: the validating participants, then the others.
group() {
	participant "$1" "$3" 32
	participant "$1" $(($2 - $3)) 0
}

out=$DEVNET/params/$RUN.yaml
tmp=$out.tmp
mkdir -p "$(dirname "$out")"
{
	echo "# ${VAL_NEW_SET:+VAL_NEW=$VAL_NEW }${VAL_OLD_SET:+VAL_OLD=$VAL_OLD }devnet/gen-params.sh $RUN $N_NEW $N_OLD $BOOT_KIND $OLD_IMAGE"
	echo "participants:"
	if [ "$BOOT_KIND" = new ]; then
		participant new 1 32 boot
		group new $((N_NEW - 1)) $((VAL_NEW - 1))
		group old "$N_OLD" "$VAL_OLD"
	else
		participant old 1 32 boot
		group new "$N_NEW" "$VAL_NEW"
		group old $((N_OLD - 1)) $((VAL_OLD - 1))
	fi
	echo "additional_services: []"
	echo "parallel_keystore_generation: true"
} >"$tmp"
mv "$tmp" "$out"
log "wrote $out"
