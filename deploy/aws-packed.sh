#!/bin/sh
# AWS packed-host driver: the verbs of deploy/gcp.sh on EC2 (an instance per
# testbed.aws_packed.vnodes_per_machine nodes, each node in its own netns), one zone, no peering.
#   deploy/aws-packed.sh up <scenario> [tf args]  terraform apply sized from the scenario + build + push to the bucket;
#                                                 refuses if the fleet over MAX_HOURS (6) costs more than MAX_SPEND ($150)
#                                                 or does not fit the region's on-demand vCPU and route quotas
#   deploy/aws-packed.sh push                  rebuild and re-push the binaries only
#   deploy/aws-packed.sh run scenarios/x.yaml  run it on the coordinator, wait, pull the results, then destroy everything
#                                              (up and run destroy on failure too; only a failed pull keeps the deployment; KEEP=1 keeps it always)
#   deploy/aws-packed.sh check                 list what is still running
#   deploy/aws-packed.sh pull                  fetch the latest run directory into runs/ (the coordinator saves every run to the bucket)
#   deploy/aws-packed.sh ssh                   shell on the coordinator (SSM)
#   deploy/aws-packed.sh down                  copy every saved run to runs/archives, then terraform destroy
# Needs the aws CLI with credentials (AWS_PROFILE) and the session-manager-plugin for ssh.
set -e
cd "$(dirname "$0")/.."
TF="deploy/terraform/aws-packed"
MAX_SPEND=${MAX_SPEND:-150}
MAX_HOURS=${MAX_HOURS:-6}
tfout() { terraform -chdir=$TF output -raw "$1"; }
region() { tfout region; }

build_push() {
  chmod -R u+w out 2>/dev/null; rm -rf out && mkdir -p out
  for c in node hostagent testbed; do
    GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o out/$( [ $c = node ] && echo topdisc-node || echo $c ) ./cmd/$c
  done
  (cd legacy && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o ../out/topdisc-node-legacy ./cmd/node-legacy)
  (cd legacy && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -modfile=go.fork.mod -tags fork -o ../out/topdisc-node-plain ./cmd/node-legacy)
  cp deploy/inventory-aws-packed.sh out/
  tar czf out/topdisc-linux-amd64.tgz -C out topdisc-node topdisc-node-legacy topdisc-node-plain hostagent testbed inventory-aws-packed.sh
  aws s3 cp --only-show-errors --region "$(region)" out/topdisc-linux-amd64.tgz "s3://$(tfout bucket)/topdisc-linux-amd64.tgz"
}

# On-demand prices in us-east-1 (2026-10-21, USD/h); eu-central-1 is about
# 15 % dearer. Zone-local traffic is free; gp3 volumes $0.08/GB-month.
cost_guard() {
  f=$1; shift
  python3 - "$f" "$MAX_HOURS" "$MAX_SPEND" "$@" <<'PY' || exit 1
import json, re, sys
f = json.load(open(sys.argv[1])); a = f["aws_packed"]; hosts = f["aws_packed_machines"]
hours, cap = float(sys.argv[2]), float(sys.argv[3])
args = " ".join(sys.argv[4:])
PRICES = {"m6a.large": 0.0864, "m6a.xlarge": 0.1728, "m6a.2xlarge": 0.3456, "m6a.4xlarge": 0.6912,
          "c6a.xlarge": 0.153, "c6a.2xlarge": 0.306, "c6a.4xlarge": 0.612,
          "m6i.xlarge": 0.192, "m6i.2xlarge": 0.384, "m6i.4xlarge": 0.768,
          "c6i.xlarge": 0.17, "c6i.2xlarge": 0.34, "c6i.4xlarge": 0.68}
def price(t):
    if t not in PRICES: sys.exit("no price for %s; add it to deploy/aws-packed.sh" % t)
    return PRICES[t]
def var(name, default):
    m = re.search(r"%s=(\d+)" % name, args); return int(m.group(1)) if m else default
disks = (hosts * var("host_disk_gb", 20) + var("coordinator_disk_gb", 100)) * 0.08 / 730
ips = (hosts + 1) * 0.005
hourly = hosts * price(a["instance_type"]) + price(a["coordinator_type"]) + disks + ips
print("cost: %d x %s + %s + disks + public IPs = $%.2f/h, $%.0f over %g h (cap $%.0f)" % (hosts, a["instance_type"], a["coordinator_type"], hourly, hourly * hours, hours, cap))
if hourly * hours > cap:
    sys.exit("refusing: over MAX_SPEND; lower MAX_HOURS or the fleet, or raise MAX_SPEND knowingly")
PY
}
# The region's on-demand vCPU quota (standard families) and the route table's
# route quota must hold the fleet, on top of what already runs there; a fleet
# over quota comes up short and the inventory then refuses it.
quota_guard() {
  python3 - "$1" <<'PY' || exit 1
import json, subprocess, sys
f = json.load(open(sys.argv[1])); a = f["aws_packed"]; hosts = f["aws_packed_machines"]; r = a["region"]
def aws(*args): return json.loads(subprocess.check_output(["aws", "--region", r, "--output", "json"] + list(args)))
def quota(code, svc="ec2"): return int(aws("service-quotas", "get-service-quota", "--service-code", svc, "--quota-code", code)["Quota"]["Value"])
types = {t["InstanceType"]: t["VCpuInfo"]["DefaultVCpus"] for t in aws("ec2", "describe-instance-types", "--instance-types", a["instance_type"], a["coordinator_type"])["InstanceTypes"]}
need = hosts * types[a["instance_type"]] + types[a["coordinator_type"]]
running = sum(i["CpuOptions"]["CoreCount"] * i["CpuOptions"]["ThreadsPerCore"] for res in aws("ec2", "describe-instances", "--filters", "Name=instance-state-name,Values=running,pending")["Reservations"] for i in res["Instances"])
have = quota("L-1216C47A") - running
if have < need: sys.exit("refusing: need %d on-demand vCPUs, %d free in %s (quota L-1216C47A); request an increase or shrink the fleet" % (need, have, r))
print("quota vCPUs: need %d, %d free in %s" % (need, have, r))
routes = quota("L-93826ACB", "vpc")
if routes < hosts + 2: sys.exit("refusing: %d hosts need %d routes per route table, quota is %d (L-93826ACB)" % (hosts, hosts + 2, routes))
print("quota routes: need %d, %d allowed" % (hosts + 2, routes))
PY
}
# Runs a command on the coordinator through SSM and prints its output. The
# coordinator registers with SSM a few minutes after boot; wait for it before
# the first command instead of failing with InvalidInstanceId, then for the
# init script's marker.
ssm_wait() {
  for i in $(seq 1 60); do
    st=$(aws ssm describe-instance-information --region "$(region)" --filters "Key=InstanceIds,Values=$(tfout coordinator_id)" \
         --query 'InstanceInformationList[0].PingStatus' --output text 2>/dev/null)
    [ "$st" = Online ] && break
    [ $i -eq 1 ] && echo "waiting for the coordinator to register with SSM"
    sleep 10
  done
  [ "$st" = Online ] || { echo "coordinator never registered with SSM"; return 1; }
  for i in $(seq 1 60); do
    ssm_run "test -e /opt/topdisc/ready" >/dev/null 2>&1 && return 0
    [ $i -eq 1 ] && echo "waiting for the coordinator's init script"
    sleep 10
  done
  echo "coordinator never finished its init script"; return 1
}
# ssm_send starts a command and returns at once: the SSM shell only completes
# when the process tree exits, so the run itself is started this way.
ssm_send() {
  params=$(python3 -c 'import json,sys; print(json.dumps({"commands":["export AWS_DEFAULT_REGION=$(cat /opt/topdisc/region); "+sys.argv[1]]}))' "$1")
  aws ssm send-command --region "$(region)" --instance-ids "$(tfout coordinator_id)" --document-name AWS-RunShellScript \
       --parameters "$params" --timeout-seconds 600 --query Command.CommandId --output text > /dev/null
}
ssm_run() {
  params=$(python3 -c 'import json,sys; print(json.dumps({"commands":["export AWS_DEFAULT_REGION=$(cat /opt/topdisc/region); "+sys.argv[1]]}))' "$1")
  id=$(aws ssm send-command --region "$(region)" --instance-ids "$(tfout coordinator_id)" --document-name AWS-RunShellScript \
       --parameters "$params" --timeout-seconds 3600 --query Command.CommandId --output text)
  while :; do
    st=$(aws ssm get-command-invocation --region "$(region)" --command-id "$id" --instance-id "$(tfout coordinator_id)" --query Status --output text)
    case $st in Pending|InProgress|Delayed) sleep 5;; *) break;; esac
  done
  aws ssm get-command-invocation --region "$(region)" --command-id "$id" --instance-id "$(tfout coordinator_id)" --query StandardOutputContent --output text
  [ "$st" = Success ]
}
fleet_file() { f=$(mktemp); ./testbed fleet "$1" > "$f"; echo "$f"; }

case "$1" in
  up)
    sc=$2; shift 2
    go build -o testbed ./cmd/testbed
    f=$(fleet_file "$sc")
    hosts=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["aws_packed_machines"])' "$f")
    wan=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["wan"])' "$f")
    g=$(python3 -c 'import json,sys; a=json.load(open(sys.argv[1]))["aws_packed"]; print(" ".join("-var %s=%s" % (k, a[k]) for k in ("region","zone","instance_type","coordinator_type")))' "$f")
    [ "$hosts" -gt 0 ] || { echo "not an AWS packed scenario: testbed.aws_packed.vnodes_per_machine is unset"; exit 1; }
    [ "$wan" = True ] || { echo "testbed.wan must be enabled: the nodes live in network namespaces"; exit 1; }
    echo "fleet: $hosts hosts; $g"
    cost_guard "$f" "$@"
    terraform -chdir=$TF init -input=false >/dev/null
    if terraform -chdir=$TF state list 2>/dev/null | grep -q 'aws_instance\.'; then echo "live deployment: quota guard skipped"; else quota_guard "$f"; fi
    rm -f "$f"
    [ -n "$KEEP" ] || trap 'st=$?; [ $st -eq 0 ] || { echo "up failed; destroying"; "$0" down; "$0" check; }; exit $st' EXIT
    terraform -chdir=$TF apply -auto-approve -input=false -var "hosts=$hosts" -var "max_hours=$MAX_HOURS" $g "$@"
    build_push ;;
  push) build_push ;;
  run)
    [ -n "$KEEP" ] || trap 'st=$?; if [ $st -eq 0 ] || [ -z "$PULL_FAILED" ]; then "$0" down; "$0" check; else echo "pull failed: deployment kept; fix, then deploy/aws-packed.sh pull && deploy/aws-packed.sh down"; fi' EXIT
    f=$(fleet_file "$2")
    hosts=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["aws_packed_machines"])' "$f")
    per=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["aws_packed"]["vnodes_per_machine"])' "$f")
    rm -f "$f"
    b=$(tfout bucket)
    aws s3 cp --only-show-errors --region "$(region)" "$2" "s3://$b/scenario.yaml"
    aws s3 cp --recursive --only-show-errors --region "$(region)" "$(dirname "$2")/models" "s3://$b/models"
    ssm_wait
    # The hosts boot at their own pace; list them until every one is there.
    for i in $(seq 1 30); do
      out=$(ssm_run "cd /opt/topdisc && aws s3 cp --only-show-errors s3://$b/scenario.yaml scenario.yaml && aws s3 cp --recursive --only-show-errors s3://$b/models models && ./inventory-aws-packed.sh $per && rm -f run.out" 2>&1) && echo "$out" | grep -q "^$hosts hosts" && break
      echo "fleet: $(echo "$out" | tail -1); waiting"; sleep 20
    done
    echo "$out" | grep -q "^$hosts hosts" || { echo "fleet never complete"; exit 1; }
    # The coordinator saves the run to the bucket itself, so a laptop that is
    # away at the end loses nothing; pull then only downloads.
    ssm_send "cd /opt/topdisc && setsid -f sh -c './testbed scenario.yaml > run.out 2>&1; rc=\$?; ./save-runs.sh >> run.out 2>&1; echo RUN-EXIT \$rc >> run.out' > /dev/null 2>&1 < /dev/null"
    echo "started on the coordinator; waiting"
    while :; do
      out=$(ssm_run "tail -c 4000 /opt/topdisc/run.out" 2>/dev/null || true)
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
    r=$(region 2>/dev/null); [ -n "$r" ] || r=${AWS_REGION:-us-east-1}
    aws ec2 describe-instances --region "$r" --filters Name=tag-key,Values=topdisc Name=instance-state-name,Values=pending,running,stopping,stopped \
      --query 'Reservations[].Instances[].[Tags[?Key==`Name`].Value|[0],InstanceType,State.Name,PrivateIpAddress]' --output table
    aws s3 ls --region "$r" 2>/dev/null | grep topdisc-packed || true ;;
  pull)
    mkdir -p runs
    b=$(tfout bucket)
    d=$(ssm_run "cd /opt/topdisc && ./save-runs.sh >/dev/null 2>&1; d=\$(ls -td *-2*/ | head -1) && d=\$(basename \$d) && aws s3 ls s3://$b/runs/\$d.tgz >/dev/null && echo PULLED \$d" | sed -n 's/^PULLED //p')
    [ -n "$d" ] || { echo "pull: no run archive in s3://$b/runs/"; exit 1; }
    for i in 1 2 3 4 5; do
      aws s3 cp --only-show-errors --region "$(region)" "s3://$b/runs/$d.tgz" "runs/$d.tgz" && tar xzf "runs/$d.tgz" -C runs && rm -f "runs/$d.tgz" && break
      echo "pull: download failed (try $i of 5); retrying"; sleep 20
    done
    [ -f "runs/$d/nodes.json" ] || { echo "pull: runs/$d incomplete"; exit 1; }
    echo "runs/$d ($(ls runs/$d/traces | wc -l | tr -d ' ') traces)" ;;
  ssh) aws ssm start-session --region "$(region)" --target "$(tfout coordinator_id)" ;;
  down)
    if b=$(tfout bucket 2>/dev/null) && [ -n "$b" ] && [ -n "$(aws s3 ls --region "$(region)" "s3://$b/runs/" 2>/dev/null)" ]; then
      mkdir -p runs/archives
      aws s3 sync --only-show-errors --region "$(region)" "s3://$b/runs/" runs/archives/ || [ -n "$FORCE" ] || { echo "down: could not copy s3://$b/runs/ to runs/archives; not destroying (FORCE=1 to destroy anyway)"; exit 1; }
      ls runs/archives/*.tgz 2>/dev/null | sed 's/^/kept: /'
    fi
    for i in 1 2 3; do
      terraform -chdir=$TF destroy -auto-approve -input=false -var "hosts=0" && break
      echo "down: destroy incomplete (try $i of 3); retrying"
    done ;;
  *) sed -n 2,14p "$0"; exit 2 ;;
esac
