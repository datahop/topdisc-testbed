#!/bin/sh
# Google Cloud driver: the verbs of deploy/aws.sh on packed hosts (a VM per
# testbed.gcp.vnodes_per_machine nodes, each node in its own netns, as on Grid'5000).
#   deploy/gcp.sh up <spec> [tf args]      terraform apply sized from the scenario + build + push to the bucket;
#                                          refuses if the fleet over MAX_HOURS (6) costs more than MAX_SPEND ($100)
#                                          or does not fit the project's CPU quotas (e.g. -var zone=us-central1-f
#                                          -var machine_type=n1-standard-8 when the scenario's type is stocked out)
#   deploy/gcp.sh push                  rebuild and re-push the binaries only
#   deploy/gcp.sh run <spec>            run it on the coordinator, wait, pull the results, then destroy everything
#                                       (up and run destroy on failure too; only a failed pull keeps the deployment; KEEP=1 keeps it always)
#   deploy/gcp.sh check                 list what is still running
#   deploy/gcp.sh pull                  fetch the latest run directory into runs/ (the coordinator saves every run to the bucket)
#   deploy/gcp.sh ssh                   shell on the coordinator (IAP)
#   deploy/gcp.sh down                  copy every saved run to runs/archives, then terraform destroy
# <spec> is a scenario file or --workload W --profile P [--set key=value]... [--name N] (see README.md).
# DRY_RUN=1 makes up stop after printing the fleet, before any gcloud or terraform call (with PROJECT set).
# Needs gcloud logged in with a default project (or PROJECT=...) and application-default credentials for terraform.
set -e
cd "$(dirname "$0")/.."
TF="deploy/terraform/gcp"
PROJECT=${PROJECT:-$(gcloud config get-value project 2>/dev/null)}
MAX_SPEND=${MAX_SPEND:-100}
MAX_HOURS=${MAX_HOURS:-6}
tfout() { terraform -chdir=$TF output -raw "$1"; }
gssh() { gcloud compute ssh "$(tfout coordinator)" --project "$PROJECT" --zone "$(tfout zone)" --tunnel-through-iap --quiet --command "$1"; }
gscp() { gcloud compute scp --project "$PROJECT" --zone "$(tfout zone)" --tunnel-through-iap --quiet "$@"; }

build_push() {
  rm -rf out && mkdir -p out
  for c in node hostagent testbed; do
    GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o out/$( [ $c = node ] && echo topdisc-node || echo $c ) ./cmd/$c
  done
  (cd legacy && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o ../out/topdisc-node-legacy ./cmd/node-legacy)
  (cd legacy && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -modfile=go.fork.mod -tags fork -o ../out/topdisc-node-plain ./cmd/node-legacy)
  cp deploy/inventory-gcp.sh out/
  tar czf out/topdisc-linux-amd64.tgz -C out topdisc-node topdisc-node-legacy topdisc-node-plain hostagent testbed inventory-gcp.sh
  gcloud storage cp -q out/topdisc-linux-amd64.tgz "gs://$(tfout bucket)/topdisc-linux-amd64.tgz"
}

# On-demand prices in us-central1 (2026-10-20, USD/h); other regions are
# within 15 % and priced the same. Zone-local traffic is free. Disk sizes
# follow the terraform args when given.
cost_guard() {
  f=$1; shift
  python3 - "$f" "$MAX_HOURS" "$MAX_SPEND" "$@" <<'PY' || exit 1
import json, re, sys
f = json.load(open(sys.argv[1])); g = f["gcp"]; hosts = f["gcp_machines"]
hours, cap = float(sys.argv[2]), float(sys.argv[3])
args = " ".join(sys.argv[4:])
PRICES = {"e2-standard-2": 0.067, "e2-standard-4": 0.134, "e2-standard-8": 0.268, "e2-standard-16": 0.536,
          "n1-standard-4": 0.190, "n1-standard-8": 0.380, "n1-standard-16": 0.760,
          "n2-standard-4": 0.194, "n2-standard-8": 0.389, "n2-standard-16": 0.777, "n2-highmem-8": 0.524,
          "n2d-standard-8": 0.338, "t2d-standard-8": 0.338, "c3-standard-8": 0.419}
def price(t):
    if t not in PRICES: sys.exit("no price for %s; add it to deploy/gcp.sh" % t)
    return PRICES[t]
def var(name, default):
    m = re.search(r"%s=(\d+)" % name, args); return int(m.group(1)) if m else default
disks = (hosts * var("host_disk_gb", 20) + var("coordinator_disk_gb", 100)) * 0.1 / 730
hourly = hosts * price(g["machine_type"]) + price(g["coordinator_type"]) + disks
print("cost: %d x %s + %s + disks = $%.2f/h, $%.0f over %g h (cap $%.0f)" % (hosts, g["machine_type"], g["coordinator_type"], hourly, hourly * hours, hours, cap))
if hourly * hours > cap:
    sys.exit("refusing: over MAX_SPEND; lower MAX_HOURS or the fleet, or raise MAX_SPEND knowingly")
PY
}
# The project's all-regions CPUS and, in the region, the family's own pool
# when the API lists one (N2_CPUS, E2_CPUS, ...) else the shared CPUS, must
# each hold the fleet. A fleet over quota comes up short and the inventory
# then refuses it; check first. Skipped on a live deployment, whose own
# usage would be counted against it.
quota_guard() {
  python3 - "$1" "$PROJECT" <<'PY' || exit 1
import json, subprocess, sys
f = json.load(open(sys.argv[1])); g = f["gcp"]; hosts = f["gcp_machines"]
region = json.loads(subprocess.check_output(["gcloud", "compute", "regions", "describe", g["region"], "--project", sys.argv[2], "--format=json"]))["quotas"]
project = json.loads(subprocess.check_output(["gcloud", "compute", "project-info", "describe", "--project", sys.argv[2], "--format=json"]))["quotas"]
have = {q["metric"]: q["limit"] - q["usage"] for q in region + project}
def cpus(t): return int(t.split("-")[-1])
need = {}
for t, n in ((g["machine_type"], hosts), (g["coordinator_type"], 1)):
    fam = t.split("-")[0].upper() + "_CPUS"
    for q in ("CPUS_ALL_REGIONS", fam if fam in have else "CPUS"):
        need[q] = need.get(q, 0) + cpus(t) * n
for q, n in need.items():
    if q not in have: sys.exit("quota %s not found in %s" % (q, g["region"]))
    if have[q] < n: sys.exit("refusing: need %d of %s, %d free in %s; request an increase or shrink the fleet" % (n, q, have[q], g["region"]))
    print("quota %s: need %d, %d free" % (q, n, have[q]))
PY
}
# IAP ssh answers once the guest is up; the startup script marks ready when the binaries are in.
coordinator_wait() {
  for i in $(seq 1 90); do
    gssh "test -e /opt/topdisc/ready" >/dev/null 2>&1 && return 0
    [ $i -eq 1 ] && echo "waiting for the coordinator's startup script"
    sleep 10
  done
  echo "coordinator never finished its startup script"; return 1
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
    hosts=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["gcp_machines"])' "$f")
    wan=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["wan"])' "$f")
    g=$(python3 -c 'import json,sys; g=json.load(open(sys.argv[1]))["gcp"]; print(" ".join("-var %s=%s" % (k, g[k]) for k in ("region","zone","machine_type","coordinator_type")))' "$f")
    [ "$hosts" -gt 0 ] || { echo "not a GCP scenario: testbed.gcp.vnodes_per_machine is unset"; exit 1; }
    [ "$wan" = True ] || { echo "testbed.wan must be enabled: the nodes live in network namespaces"; exit 1; }
    echo "fleet: $hosts hosts; $g"
    [ -z "$DRY_RUN" ] || { rm -f "$f"; exit 0; }
    cost_guard "$f" "$@"
    terraform -chdir=$TF init -input=false >/dev/null
    if terraform -chdir=$TF state list 2>/dev/null | grep -q 'google_compute_instance\.'; then echo "live deployment: quota guard skipped"; else quota_guard "$f"; fi
    rm -f "$f"
    [ -n "$KEEP" ] || trap 'st=$?; [ $st -eq 0 ] || { echo "up failed; destroying"; "$0" down; "$0" check; }; exit $st' EXIT
    terraform -chdir=$TF apply -auto-approve -input=false -var "project=$PROJECT" -var "hosts=$hosts" -var "max_hours=$MAX_HOURS" $g "$@"
    build_push ;;
  push) build_push ;;
  run)
    shift
    sc=$(mktemp scenarios/.composed.XXXXXX)
    ./testbed compose "$@" > "$sc" || { rm -f "$sc"; exit 1; }
    f=$(fleet_file "$sc")
    [ -n "$KEEP" ] || trap 'st=$?; if [ $st -eq 0 ] || [ -z "$PULL_FAILED" ]; then "$0" down; "$0" check; else echo "pull failed: deployment kept; fix, then deploy/gcp.sh pull && deploy/gcp.sh down"; fi' EXIT
    hosts=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["gcp_machines"])' "$f")
    per=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["gcp"]["vnodes_per_machine"])' "$f")
    rm -f "$f"
    coordinator_wait
    gscp "$sc" "$(tfout coordinator):/opt/topdisc/scenario.yaml"
    gscp --recurse "$(dirname "$sc")/models" "$(tfout coordinator):/opt/topdisc/"
    rm -f "$sc"
    # The hosts boot at their own pace; list them until every one is there.
    for i in $(seq 1 30); do
      out=$(gssh "cd /opt/topdisc && ./inventory-gcp.sh $per && rm -f run.out" 2>&1) && echo "$out" | grep -q "^$hosts hosts" && break
      echo "fleet: $(echo "$out" | tail -1); waiting"; sleep 20
    done
    echo "$out" | grep -q "^$hosts hosts" || { echo "fleet never complete"; exit 1; }
    # The coordinator saves the run to the bucket itself, so a laptop that is
    # away at the end loses nothing; pull then only downloads.
    # setsid -f: the forked run is detached from this SSH session, which
    # would otherwise stay open (and this driver stuck) until the run ends.
    gssh "cd /opt/topdisc && setsid -f sh -c './testbed scenario.yaml > run.out 2>&1; rc=\$?; ./save-runs.sh >> run.out 2>&1; echo RUN-EXIT \$rc >> run.out' > /dev/null 2>&1 < /dev/null"
    echo "started on the coordinator; waiting"
    while :; do
      out=$(gssh "tail -c 4000 /opt/topdisc/run.out" 2>/dev/null || true)
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
    gcloud compute instances list --project "$PROJECT" --filter='name~^topdisc-' --format='table(name,zone.basename(),machineType.basename(),status)'
    gcloud storage ls --project "$PROJECT" 2>/dev/null | grep topdisc || true ;;
  pull)
    mkdir -p runs
    b=$(tfout bucket)
    d=$(gssh "cd /opt/topdisc && ./save-runs.sh >/dev/null 2>&1; d=\$(ls -td *-2*/ | head -1) && d=\$(basename \$d) && gsutil -q ls gs://$b/runs/\$d.tgz && echo PULLED \$d" | sed -n 's/^PULLED //p')
    [ -n "$d" ] || { echo "pull: no run archive in gs://$b/runs/"; exit 1; }
    for i in 1 2 3 4 5; do
      gcloud storage cp -q "gs://$b/runs/$d.tgz" "runs/$d.tgz" && tar xzf "runs/$d.tgz" -C runs && rm -f "runs/$d.tgz" && break
      echo "pull: download failed (try $i of 5); retrying"; sleep 20
    done
    [ -f "runs/$d/nodes.json" ] || { echo "pull: runs/$d incomplete"; exit 1; }
    echo "runs/$d ($(ls runs/$d/traces | wc -l | tr -d ' ') traces)" ;;
  ssh) gcloud compute ssh "$(tfout coordinator)" --project "$PROJECT" --zone "$(tfout zone)" --tunnel-through-iap ;;
  down)
    if b=$(tfout bucket 2>/dev/null) && [ -n "$b" ] && [ -n "$(gcloud storage ls "gs://$b/runs/" 2>/dev/null)" ]; then
      mkdir -p runs/archives
      gcloud storage rsync -q "gs://$b/runs/" runs/archives/ || [ -n "$FORCE" ] || { echo "down: could not copy gs://$b/runs/ to runs/archives; not destroying (FORCE=1 to destroy anyway)"; exit 1; }
      ls runs/archives/*.tgz 2>/dev/null | sed 's/^/kept: /'
    fi
    for i in 1 2 3; do
      terraform -chdir=$TF destroy -auto-approve -input=false -var "project=$PROJECT" -var "hosts=0" && break
      echo "down: destroy incomplete (try $i of 3); retrying"
    done ;;
  *) sed -n 2,16p "$0"; exit 2 ;;
esac
