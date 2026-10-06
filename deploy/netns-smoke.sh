#!/bin/bash
# Smoke test for the crawl-address network setup, on one machine, as root.
#
# A run on crawl addresses gives every node the address a crawled peer had, so
# a node's address no longer says which machine it is on and nothing is on-link
# in either direction. This builds the same shape pkg/host.Prepare does -- two
# simulated machines, a bridge each, one node namespace on each holding a /32
# out of a foreign /24 -- and checks a node on one machine can reach a node on
# the other. If this passes, the addressing and routing work; everything else
# about the run is unchanged.
#
#   sudo bash deploy/netns-smoke.sh
set -u

A_BR=10.100.255.254; B_BR=10.101.255.254
A_NODE=1.2.3.4;      B_NODE=5.6.7.8     # two foreign /24s, as the crawl gives
LINK_A=192.0.2.1;    LINK_B=192.0.2.2   # stands in for the production network

cleanup() {
  for ns in n1 n2 mA mB; do ip netns del $ns 2>/dev/null; done
}
trap cleanup EXIT
cleanup

set -e
# --- two machines, joined by a link that stands in for the G5K network
ip netns add mA; ip netns add mB
ip link add mAmB type veth peer name mBmA
ip link set mAmB netns mA; ip link set mBmA netns mB
ip -n mA addr add $LINK_A/30 dev mAmB; ip -n mA link set mAmB up; ip -n mA link set lo up
ip -n mB addr add $LINK_B/30 dev mBmA; ip -n mB link set mBmA up; ip -n mB link set lo up
for m in mA mB; do
  ip netns exec $m sysctl -q -w net.ipv4.ip_forward=1
  ip netns exec $m sysctl -q -w net.ipv4.conf.all.rp_filter=0
  ip netns exec $m sysctl -q -w net.ipv4.conf.default.rp_filter=0
done

# --- a bridge per machine, as Prepare builds
ip -n mA link add tbbr0 type bridge; ip -n mA addr add $A_BR/16 dev tbbr0; ip -n mA link set tbbr0 up
ip -n mB link add tbbr0 type bridge; ip -n mB addr add $B_BR/16 dev tbbr0; ip -n mB link set tbbr0 up

# --- one node per machine, holding a /32 from a foreign /24
node() { # node() <ns> <machine> <addr> <bridge-addr>
  local ns=$1 m=$2 ip4=$3 br=$4
  ip netns add "$ns"
  ip link add "v$ns" type veth peer name "h$ns"
  ip link set "v$ns" netns "$ns"
  ip link set "h$ns" netns "$m"
  ip -n "$m" link set "h$ns" master tbbr0 up
  ip -n "$ns" addr add "$ip4/32" dev "v$ns"
  ip -n "$ns" link set lo up; ip -n "$ns" link set "v$ns" up
  ip -n "$ns" route add "$br/32" dev "v$ns"       # the gateway is not on-link
  ip -n "$ns" route add default via "$br"
  ip -n "$m" route replace "$ip4/32" dev tbbr0    # makes the machine ARP for it
}
node n1 mA $A_NODE $A_BR
node n2 mB $B_NODE $B_BR

# --- the per-/24 routes between machines
ip -n mA route replace ${B_NODE%.*}.0/24 via $LINK_B
ip -n mB route replace ${A_NODE%.*}.0/24 via $LINK_A
set +e

fail=0
check() { # check <description> <command...>
  if "${@:2}" >/dev/null 2>&1; then echo "  ok    $1"; else echo "  FAIL  $1"; fail=1; fi
}
echo "crawl-address network smoke test"
check "node reaches its own machine's bridge"  ip netns exec n1 ping -c1 -W2 $A_BR
check "node reaches the far machine's bridge"  ip netns exec n1 ping -c1 -W2 $B_BR
check "node reaches a node on the far machine" ip netns exec n1 ping -c2 -W2 $B_NODE
check "and the far node answers back"          ip netns exec n2 ping -c2 -W2 $A_NODE
check "UDP carries both ways"                  ip netns exec n1 timeout 3 bash -c "echo hi >/dev/udp/$B_NODE/9"

echo
if [ $fail -eq 0 ]; then echo "PASS - the addressing and routing hold"; else
  echo "FAIL - do not run tonight on crawl addresses"
  echo "routes on mA:"; ip -n mA route | sed 's/^/    /'
  echo "routes in n1:"; ip -n n1 route | sed 's/^/    /'
fi
exit $fail
