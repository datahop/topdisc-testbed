#!/bin/sh
# Hetzner Cloud driver: the verbs of deploy/gcp.sh on packed hosts (a server per
# testbed.hetzner.vnodes_per_machine nodes, each node in its own netns).
#   deploy/hetzner.sh up <spec> [tf args]      terraform apply sized from the scenario + build + push the binaries over SSH
#                                              + start the agents + write inventory.json; refuses if the fleet over
#                                              MAX_HOURS (6) costs more than MAX_SPEND (EUR 100, gross prices from the API)
#   deploy/hetzner.sh push                  rebuild, re-push and restart the agents only
#   deploy/hetzner.sh run <spec>            run it on the coordinator, wait, pull the results, then destroy everything
#                                           (up and run destroy on failure too; only a failed pull keeps the deployment; KEEP=1 keeps it always)
#   deploy/hetzner.sh check                 list the project's servers
#   deploy/hetzner.sh pull                  fetch the latest run directory from the coordinator into runs/
#   deploy/hetzner.sh ssh                   shell on the coordinator
#   deploy/hetzner.sh down                  terraform destroy
# <spec> is a scenario file or --workload W --profile P [--set key=value]... [--name N] (see README.md).
# DRY_RUN=1 makes up stop after printing the fleet, before any API or terraform call.
# Needs the project token in ~/.config/hcloud/token (or HCLOUD_TOKEN) and an SSH key (SSH_KEY, default ~/.ssh/id_ed25519).
# There is no scale-down timer here: a laptop that dies mid-run leaves the servers up until `down`.
set -e
cd "$(dirname "$0")/.."
TF="deploy/terraform/hetzner"
export TF_VAR_hcloud_token=${HCLOUD_TOKEN:-$(cat ~/.config/hcloud/token)}
SSH_KEY=${SSH_KEY:-$HOME/.ssh/id_ed25519}
MAX_SPEND=${MAX_SPEND:-100}
MAX_HOURS=${MAX_HOURS:-6}
SSH="ssh -i $SSH_KEY -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=15"
SCP="scp -i $SSH_KEY -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"
tfout() { terraform -chdir=$TF output -raw "$1"; }
api() { curl -s -H "Authorization: Bearer $TF_VAR_hcloud_token" "https://api.hetzner.cloud/v1/$1"; }
cssh() { $SSH root@"$(tfout coordinator_public_ip)" "$1"; }
# every server's public address, the coordinator last
public_ips() { terraform -chdir=$TF output -json hosts | python3 -c 'import json,sys; print("\n".join(h["public_ip"] for h in json.load(sys.stdin)))'; tfout coordinator_public_ip; }

build() {
  mkdir -p out
  for c in node hostagent testbed; do
    GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o out/$( [ $c = node ] && echo topdisc-node || echo $c ) ./cmd/$c
  done
  (cd legacy && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o ../out/topdisc-node-legacy ./cmd/node-legacy)
  (cd legacy && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -modfile=go.fork.mod -tags fork -o ../out/topdisc-node-plain ./cmd/node-legacy)
}
# Pushes the binaries to every server, 16 at a time, waits for cloud-init's
# marker first, then (re)starts the agent so it runs the binaries just pushed.
push() {
  for ip in $(public_ips); do
    (
      for i in $(seq 1 60); do $SSH root@$ip 'test -e /opt/topdisc/ready' 2>/dev/null && break; sleep 10; done
      $SCP -q out/topdisc-node out/topdisc-node-legacy out/topdisc-node-plain out/hostagent out/testbed root@$ip:/opt/topdisc/ &&
      $SSH root@$ip 'chmod +x /opt/topdisc/*; systemctl enable -q hostagent; systemctl restart hostagent' && echo "pushed $ip"
    ) &
    [ $(jobs -r | wc -l) -ge 16 ] && wait
  done
  wait
  # the coordinator runs no agent
  cssh 'systemctl disable -q --now hostagent' || true
}
# Gross hourly prices straight from the API for the two types in use.
cost_guard() {
  python3 - "$1" "$MAX_HOURS" "$MAX_SPEND" "$(api 'server_types?per_page=50')" <<'PY' || exit 1
import json, sys
f = json.load(open(sys.argv[1])); h = f["hetzner"]; hosts = f["hetzner_machines"]
hours, cap = float(sys.argv[2]), float(sys.argv[3])
types = {t["name"]: t for t in json.loads(sys.argv[4])["server_types"]}
def price(name):
    t = types.get(name) or sys.exit("unknown server type %s" % name)
    p = [x for x in t["prices"] if x["location"] == h["location"]] or sys.exit("%s is not offered in %s" % (name, h["location"]))
    return float(p[0]["price_hourly"]["gross"])
hourly = hosts * price(h["server_type"]) + price(h["coordinator_type"])
print("cost: %d x %s + %s = EUR %.2f/h gross, EUR %.0f over %g h (cap EUR %.0f)" % (hosts, h["server_type"], h["coordinator_type"], hourly, hourly * hours, hours, cap))
if hourly * hours > cap:
    sys.exit("refusing: over MAX_SPEND; lower MAX_HOURS or the fleet, or raise MAX_SPEND knowingly")
PY
}
inventory() {
  terraform -chdir=$TF output -json hosts | python3 -c '
import json,sys
hosts=json.load(sys.stdin); per=int(sys.argv[1])
json.dump({"coordinator": sys.argv[2], "routes": "network", "hosts": [{"index": h["index"], "ip": h["ip"], "nodes": per} for h in hosts]}, open("out/inventory.json", "w"))
print("%d hosts x %d nodes" % (len(hosts), per))' "$1" "$(tfout coordinator_ip)"
}
fleet_file() { f=$(mktemp); ./testbed fleet "$1" > "$f"; echo "$f"; }

case "$1" in
  up)
    shift
    go build -o testbed ./cmd/testbed
    sc=$(mktemp scenarios/.composed.XXXXXX)
    n=$(./testbed compose -o "$sc" "$@") || { rm -f "$sc"; exit 1; }
    shift "$n"
    f=$(fleet_file "$sc")
    rm -f "$sc"
    hosts=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["hetzner_machines"])' "$f")
    wan=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["wan"])' "$f")
    per=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["hetzner"]["vnodes_per_machine"])' "$f")
    g=$(python3 -c 'import json,sys; h=json.load(open(sys.argv[1]))["hetzner"]; print(" ".join("-var %s=%s" % (k, h[k]) for k in ("location","server_type","coordinator_type")))' "$f")
    [ "$hosts" -gt 0 ] || { echo "not a Hetzner scenario: testbed.hetzner.vnodes_per_machine is unset"; exit 1; }
    [ "$wan" = True ] || { echo "testbed.wan must be enabled: the nodes live in network namespaces"; exit 1; }
    echo "fleet: $hosts hosts; $g"
    [ -z "$DRY_RUN" ] || { rm -f "$f"; exit 0; }
    cost_guard "$f"
    rm -f "$f"
    [ -n "$KEEP" ] || trap 'st=$?; [ $st -eq 0 ] || { echo "up failed; destroying"; "$0" down; "$0" check; }; exit $st' EXIT
    terraform -chdir=$TF init -input=false >/dev/null
    terraform -chdir=$TF apply -auto-approve -input=false -var "hosts=$hosts" -var "ssh_public_key=$(cat $SSH_KEY.pub)" $g "$@"
    build
    push
    inventory "$per" ;;
  push) build; push ;;
  run)
    shift
    sc=$(mktemp scenarios/.composed.XXXXXX)
    ./testbed compose "$@" > "$sc" || { rm -f "$sc"; exit 1; }
    f=$(fleet_file "$sc")
    [ -n "$KEEP" ] || trap 'st=$?; if [ $st -eq 0 ] || [ -z "$PULL_FAILED" ]; then "$0" down; "$0" check; else echo "pull failed: deployment kept; fix, then deploy/hetzner.sh pull && deploy/hetzner.sh down"; fi' EXIT
    per=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["hetzner"]["vnodes_per_machine"])' "$f")
    rm -f "$f"
    inventory "$per"
    c=$(tfout coordinator_public_ip)
    $SCP -q "$sc" root@$c:/opt/topdisc/scenario.yaml
    $SCP -q out/inventory.json root@$c:/opt/topdisc/inventory.json
    $SCP -q -r "$(dirname "$sc")/models" root@$c:/opt/topdisc/
    rm -f "$sc"
    # setsid -f: the forked run is detached from this SSH session, which
    # would otherwise stay open (and this driver stuck) until the run ends.
    cssh "cd /opt/topdisc && rm -f run.out && setsid -f sh -c './testbed scenario.yaml > run.out 2>&1; echo RUN-EXIT \$? >> run.out' > /dev/null 2>&1 < /dev/null"
    echo "started on the coordinator; waiting"
    while :; do
      out=$(cssh "tail -c 4000 /opt/topdisc/run.out" 2>/dev/null || true)
      echo "$out" | grep -qE "^RUN-EXIT" && break
      echo "$out" | grep -E "backend:|waiting for|nodes started|\[churn\]|traces missing" | tail -1
      sleep 30
    done
    echo "$out" | grep -vE "^PARAMS"
    rc=$(echo "$out" | sed -n 's/^RUN-EXIT //p' | tail -1)
    if [ "$rc" != 0 ]; then
      echo "run failed (exit $rc); pulling what it wrote, then destroying (KEEP=1 to keep the deployment)"
      "$0" pull || true
      exit 1
    fi
    "$0" pull || { PULL_FAILED=1; exit 1; } ;;
  check)
    # Hetzner has no usage endpoint: what the running servers have cost so
    # far is their uptime at the gross hourly price.
    api 'servers?per_page=50' > /tmp/hz-servers.json; api 'server_types?per_page=50' > /tmp/hz-types.json
    python3 - <<'PY'
import datetime, json
st = {t["name"]: t for t in json.load(open("/tmp/hz-types.json"))["server_types"]}
now = datetime.datetime.now(datetime.timezone.utc); total = 0
servers = json.load(open("/tmp/hz-servers.json"))["servers"]
print("%d servers" % len(servers))
for s in servers:
    h = (now - datetime.datetime.fromisoformat(s["created"].replace("Z", "+00:00"))).total_seconds() / 3600
    loc = (s.get("location") or s.get("datacenter", {}).get("location") or {}).get("name", "fsn1")
    p = [float(x["price_hourly"]["gross"]) for x in st[s["server_type"]["name"]]["prices"] if x["location"] == loc]
    cost = h * (p[0] if p else 0); total += cost
    print("  %s %s %s %s up %.1f h EUR %.2f" % (s["name"], s["server_type"]["name"], s["status"], s["public_net"]["ipv4"]["ip"], h, cost))
print("running servers so far: EUR %.2f gross" % total)
PY
    ;;
  pull)
    # Aggregates first, then the traces compressed: at 25k nodes the traces
    # are ~50 GB and a rebuild path only (cmd/recollect), not a figure input.
    mkdir -p runs
    c=$(tfout coordinator_public_ip)
    d=$(cssh 'cd /opt/topdisc && ls -td *-2*/ | head -1' | tr -d '/')
    [ -n "$d" ] || { echo "pull: no run directory on the coordinator"; exit 1; }
    rsync -a -e "$SSH" --exclude=traces/ root@$c:/opt/topdisc/$d/ runs/$d/
    [ -f "runs/$d/nodes.json" ] || { echo "pull: runs/$d has no nodes.json"; exit 1; }
    cssh "tar czf - -C /opt/topdisc/$d traces" > runs/$d/traces.tgz || { rm -f runs/$d/traces.tgz; echo "pull: traces failed; aggregates are in"; exit 1; }
    echo "runs/$d ($(du -sh runs/$d/traces.tgz | cut -f1) of traces)" ;;
  ssh) $SSH root@"$(tfout coordinator_public_ip)" ;;
  down)
    for i in 1 2 3; do
      terraform -chdir=$TF destroy -auto-approve -input=false -var "hosts=0" -var "ssh_public_key=$(cat $SSH_KEY.pub)" && break
      echo "down: destroy incomplete (try $i of 3); retrying"
    done ;;
  *) sed -n 2,17p "$0"; exit 2 ;;
esac
