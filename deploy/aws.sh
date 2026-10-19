#!/bin/sh
# AWS driver: build the binaries, provision, push, run, collect, tear down.
#   deploy/aws.sh up <scenario> [tf args]  terraform apply sized from the scenario + build + push to S3 (e.g. -var spot=true)
#                                          refuses if the fleet's cost ceiling over MAX_HOURS (6) > MAX_SPEND ($200);
#                                          the ceiling prices every node on the dearest instance type the
#                                          autoscaling group may fall back to, plus volumes and a traffic allowance
#   deploy/aws.sh push                  rebuild and re-push the binaries only
#   deploy/aws.sh run scenarios/x.yaml  run it on the coordinator, wait, pull the results, then destroy everything
#                                       up and run destroy on any failure or interrupt too (a failed run is pulled first);
#                                       only a failed pull keeps the deployment; KEEP=1 keeps it always.
#                                       Once the fleet is up it is priced as launched: a fleet short or more than
#                                       FLEET_TOLERANCE (10 %) over the expected cost prints a warning and goes
#                                       on; FLEET_STRICT=1 destroys it instead
#   deploy/aws.sh check                 list what is still running in every region (instances, NAT gateways, elastic IPs)
#   deploy/aws.sh pull                  fetch the latest run directory into runs/ (the coordinator saves every run to the bucket)
#   deploy/aws.sh ssh                   shell on the coordinator (SSM)
#   deploy/aws.sh down                  copy every saved run to runs/archives, then terraform destroy
set -e
cd "$(dirname "$0")/.."
TF="deploy/terraform/aws"
tfout() { terraform -chdir=$TF output -raw "$1"; }

build_push() {
  rm -rf out && mkdir -p out
  for c in node hostagent testbed; do
    GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o out/$( [ $c = node ] && echo topdisc-node || echo $c ) ./cmd/$c
  done
  (cd legacy && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o ../out/topdisc-node-legacy ./cmd/node-legacy)
  (cd legacy && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -modfile=go.fork.mod -tags fork -o ../out/topdisc-node-plain ./cmd/node-legacy)
  cp deploy/inventory-aws.sh out/
  tar czf out/topdisc-linux-arm64.tgz -C out topdisc-node topdisc-node-legacy topdisc-node-plain hostagent testbed inventory-aws.sh
  # The home bucket and a mirror per active region: a private node's S3
  # endpoint only reaches its own region.
  for b in $(terraform -chdir=$TF output -json binaries_buckets | python3 -c 'import json,sys; print(" ".join(json.load(sys.stdin).values()))'); do
    aws s3 cp out/topdisc-linux-arm64.tgz "s3://$b/topdisc-linux-arm64.tgz"
  done
}

MAX_SPEND=${MAX_SPEND:-200}
MAX_HOURS=${MAX_HOURS:-6}
# The node instance types the autoscaling group may launch, cheapest first:
# an instance_types override in the terraform args, else the variable's default.
instance_types() {
  for a in "$@"; do case "$a" in *instance_types=*) echo "$a" | sed 's/.*instance_types=//; s/ .*//' | tr -d '[]"'; return;; esac; done
  python3 -c 'import re,sys; print(",".join(re.findall(r"\"([^\"]+)\"", re.search(r"variable \"instance_types\".*?default\s*=\s*\[(.*?)\]", open(sys.argv[1]).read(), re.S).group(1))))' $TF/variables.tf
}
# On-demand prices per region (deploy/aws-cost.py): the expected cost puts
# every node on the first type, the ceiling on the dearest one the group can
# fall back to when a pool runs dry, both with volumes, the coordinator and a
# cross-region traffic allowance. The ceiling is what MAX_SPEND caps. Spot is
# cheaper, so on-demand is conservative.
cost_guard() {
  out=$(python3 deploy/aws-cost.py estimate "$1" "$2" "$3" "$(instance_types "$4")" "$MAX_HOURS" $4) || exit 1
  echo "$out" | grep -v '^CEILING'
  echo "cap: MAX_SPEND=\$$MAX_SPEND over MAX_HOURS=$MAX_HOURS h; the coordinator scales the fleet to zero after $MAX_HOURS h"
  ceiling=$(echo "$out" | sed -n 's/^CEILING //p')
  python3 -c "import sys; sys.exit(0 if $ceiling <= $MAX_SPEND else 1)" || { echo "refusing: the ceiling is over MAX_SPEND; lower MAX_HOURS or the fleet, or raise MAX_SPEND knowingly"; exit 1; }
}
# Prices the fleet as launched (types per region from EC2) against the
# expected cost. Returns 2 when short or over FLEET_TOLERANCE; the caller
# decides whether that stops the run.
fleet_guard() {
  mix=$(for r in $(echo "$1" | python3 -c 'import json,sys; print(" ".join(r for r,n in json.load(sys.stdin).items() if n>0))'); do
    aws ec2 describe-instances --region "$r" --filters Name=tag:aws:autoscaling:groupName,Values=topdisc-nodes Name=instance-state-name,Values=running \
      --query 'Reservations[].Instances[].InstanceType' --output text | tr '\t' '\n' | sed "s/^/$r /"
  done | python3 -c '
import json,sys
m={}
for ln in sys.stdin:
    p=ln.split()
    if len(p)==2: m.setdefault(p[0],{}); m[p[0]][p[1]]=m[p[0]].get(p[1],0)+1
print(json.dumps(m))')
  python3 deploy/aws-cost.py fleet "$1" "$2" "$(instance_types)" "$mix"
}

# Runs a command on the coordinator through SSM and prints its output.
# The coordinator registers with SSM a few minutes after boot; wait for it
# before the first command instead of failing with InvalidInstanceId.
ssm_wait() {
  for i in $(seq 1 60); do
    st=$(aws ssm describe-instance-information --filters "Key=InstanceIds,Values=$(tfout coordinator_id)" \
         --query 'InstanceInformationList[0].PingStatus' --output text 2>/dev/null)
    [ "$st" = Online ] && return 0
    [ $i -eq 1 ] && echo "waiting for the coordinator to register with SSM"
    sleep 10
  done
  echo "coordinator never registered with SSM"; return 1
}
# One t4g node is 2 vCPUs; a fleet above the region's vCPU quota comes up
# short and the coordinator then refuses the inventory. Check first.
quota_guard() {
  # the coordinator (on demand, 2 vCPUs) counts against the on-demand quota only
  code=L-1216C47A; coord=2; echo "$3 $4 $5 $6" | grep -q "spot=true" && { code=L-34B43A08; coord=0; }
  echo "$1" | python3 -c '
import json,sys; f=json.load(sys.stdin); home=sys.argv[1]; coord=int(sys.argv[2])
for r,n in f.items():
    if n: print(r, 2*n + (coord if r==home else 0))' "$2" "$coord" | while read r need; do
    q=$(aws service-quotas get-service-quota --region $r --service-code ec2 --quota-code $code --query Quota.Value --output text 2>/dev/null | cut -d. -f1)
    [ -n "$q" ] || { echo "$r: could not read the vCPU quota"; continue; }
    [ "$q" -ge "$need" ] || { echo "refusing: $r needs $need vCPUs, quota $code is $q; request an increase or shrink the fleet"; exit 1; }
    echo "$r: vCPU quota $q, need $need"
  done
}
# ssm_send starts a command and returns at once. The run itself is started this
# way: the SSM shell only completes when the process tree exits, so a run that
# lasts an hour would otherwise hit the command's execution timeout.
ssm_send() {
  params=$(python3 -c 'import json,sys; print(json.dumps({"commands":["export AWS_DEFAULT_REGION=$(cat /opt/topdisc/region); "+sys.argv[1]]}))' "$1")
  aws ssm send-command --instance-ids "$(tfout coordinator_id)" --document-name AWS-RunShellScript \
       --parameters "$params" --timeout-seconds 600 --query Command.CommandId --output text > /dev/null
}

ssm_run() {
  params=$(python3 -c 'import json,sys; print(json.dumps({"commands":["export AWS_DEFAULT_REGION=$(cat /opt/topdisc/region); "+sys.argv[1]]}))' "$1")
  id=$(aws ssm send-command --instance-ids "$(tfout coordinator_id)" --document-name AWS-RunShellScript \
       --parameters "$params" --timeout-seconds 3600 --query Command.CommandId --output text)
  while :; do
    st=$(aws ssm get-command-invocation --command-id "$id" --instance-id "$(tfout coordinator_id)" --query Status --output text)
    case $st in Pending|InProgress|Delayed) sleep 5;; *) break;; esac
  done
  aws ssm get-command-invocation --command-id "$id" --instance-id "$(tfout coordinator_id)" --query StandardOutputContent --output text
  [ "$st" = Success ]
}

case "$1" in
  up)
    [ -n "$KEEP" ] || trap 'st=$?; [ $st -eq 0 ] || { echo "up failed; destroying"; "$0" down; "$0" check; }; exit $st' EXIT
    sc=$2; shift 2
    go build -o testbed ./cmd/testbed
    fleet=$(./testbed fleet "$sc")
    regions=$(echo "$fleet" | python3 -c 'import json,sys; print(json.dumps(json.load(sys.stdin)["regions"]))')
    home=$(echo "$fleet" | python3 -c 'import json,sys; print(json.load(sys.stdin)["home_region"])')
    pub=$(echo "$fleet" | python3 -c 'import json,sys; print(str(json.load(sys.stdin).get("public_ips", False)).lower())')
    echo "fleet: $regions, home $home, public IPs $pub"
    # The coordinator holds every trace while it aggregates: 7,500 nodes
    # overflowed 8 GB. Size it by the fleet unless the args say otherwise.
    case "$*" in *coordinator_type=*) ;; *)
      n=$(echo "$regions" | python3 -c 'import json,sys; print(sum(json.load(sys.stdin).values()))')
      ct=m7g.large; [ "$n" -ge 4000 ] && ct=m7g.xlarge; [ "$n" -ge 8000 ] && ct=m7g.2xlarge
      set -- "$@" -var "coordinator_type=$ct"; echo "coordinator: $ct for $n nodes" ;;
    esac
    cost_guard "$regions" "$home" "$pub" "$*"
    quota_guard "$regions" "$home" "$@"
    terraform -chdir=$TF init -input=false >/dev/null
    terraform -chdir=$TF apply -auto-approve -input=false -var "regions=$regions" -var "home_region=$home" \
      -var "max_hours=$MAX_HOURS" -var "public_ips=$pub" "$@"
    build_push ;;
  push) build_push ;;
  run)
    # Destroy at the end unless KEEP=1. A run that failed is pulled for its
    # logs and destroyed; only a failed pull keeps the deployment so the
    # results can still be fetched by hand (deploy/aws.sh pull / ssh).
    [ -n "$KEEP" ] || trap 'st=$?; if [ $st -eq 0 ] || [ -z "$PULL_FAILED" ]; then "$0" down; "$0" check; else echo "pull failed: deployment kept; fix, then deploy/aws.sh pull && deploy/aws.sh down"; fi' EXIT
    aws s3 cp "$2" "s3://$(tfout binaries_bucket)/scenario.yaml"
    # the scenario's models (topic and churn fits) live next to it
    aws s3 cp --recursive --only-show-errors "$(dirname "$2")/models" "s3://$(tfout binaries_bucket)/models"
    ssm_wait
    # Autoscaling groups fill over several minutes: list the fleet until it holds
    # every node of the scenario (25 min at most), so the run does not start short.
    fleet=$(go run ./cmd/testbed fleet "$2")
    regions=$(echo "$fleet" | python3 -c 'import json,sys; print(json.dumps(json.load(sys.stdin)["regions"]))')
    home=$(echo "$fleet" | python3 -c 'import json,sys; print(json.load(sys.stdin)["home_region"])')
    want=$(echo "$regions" | python3 -c 'import json,sys; print(sum(json.load(sys.stdin).values()))')
    for i in $(seq 1 50); do
      ssm_run "cd /opt/topdisc && aws s3 cp s3://$(tfout binaries_bucket)/scenario.yaml scenario.yaml && aws s3 cp --recursive --only-show-errors s3://$(tfout binaries_bucket)/models models && ./inventory-aws.sh && rm -f run.out"
      have=$(ssm_run "cd /opt/topdisc && python3 -c 'import json; print(len(json.load(open(\"inventory.json\"))[\"hosts\"]))'" 2>/dev/null)
      [ "${have:-0}" -ge "$want" ] && break
      echo "fleet: ${have:-0} of $want hosts running; waiting"; sleep 30
    done
    # The group fills from whatever pools have capacity, so price what came up
    # before the nodes start; the operator chooses whether a dear fleet runs.
    fleet_guard "$regions" "$home" || {
      if [ -n "$FLEET_STRICT" ]; then echo "stopping: FLEET_STRICT is set; destroying"; exit 1; fi
      echo "continuing with the fleet as launched (FLEET_STRICT=1 stops here instead)"
    }
    # The coordinator saves the run to the bucket itself, so a laptop that is
    # away at the end loses nothing; pull then only downloads.
    ssm_send "cd /opt/topdisc && setsid sh -c './testbed scenario.yaml > run.out 2>&1; rc=\$?; ./save-runs.sh >> run.out 2>&1; echo RUN-EXIT \$rc >> run.out' > /dev/null 2>&1 < /dev/null &"
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
    for r in $(sed -n 's/^REGIONS = \[\(.*\)\]/\1/p' $TF/gen.py | tr -d '",'); do
      n=$(aws ec2 describe-instances --region $r --filters Name=instance-state-name,Values=pending,running --query 'length(Reservations[].Instances[])' --output text)
      g=$(aws ec2 describe-nat-gateways --region $r --filter Name=state,Values=pending,available --query 'length(NatGateways)' --output text)
      e=$(aws ec2 describe-addresses --region $r --query 'length(Addresses)' --output text)
      echo "$r: instances=$n nat-gateways=$g elastic-ips=$e"
    done ;;
  pull)
    mkdir -p runs
    b=$(tfout binaries_bucket)
    d=$(ssm_run "cd /opt/topdisc && ./save-runs.sh >/dev/null 2>&1; d=\$(ls -td *-2*/ | head -1) && d=\$(basename \$d) && aws s3 ls s3://$b/runs/\$d.tgz >/dev/null && echo PULLED \$d" | sed -n 's/^PULLED //p')
    [ -n "$d" ] || { echo "pull: no run archive in s3://$b/runs/"; exit 1; }
    for i in 1 2 3 4 5; do
      aws s3 cp --only-show-errors "s3://$b/runs/$d.tgz" "runs/$d.tgz" && tar xzf "runs/$d.tgz" -C runs && rm -f "runs/$d.tgz" && break
      echo "pull: download failed (try $i of 5); retrying"; sleep 20
    done
    [ -f "runs/$d/nodes.json" ] || { echo "pull: runs/$d incomplete"; exit 1; }
    echo "runs/$d ($(ls runs/$d/traces | wc -l | tr -d ' ') traces)" ;;
  ssh) aws ssm start-session --target "$(tfout coordinator_id)" ;;
  down)
    # The bucket goes with the deployment: copy every saved run first, and
    # refuse to destroy while that fails (FORCE=1 destroys anyway).
    if b=$(tfout binaries_bucket 2>/dev/null) && [ -n "$b" ]; then
      mkdir -p runs/archives
      aws s3 sync --only-show-errors "s3://$b/runs/" runs/archives/ || [ -n "$FORCE" ] || { echo "down: could not copy s3://$b/runs/ to runs/archives; not destroying (FORCE=1 to destroy anyway)"; exit 1; }
      ls runs/archives/*.tgz 2>/dev/null | sed 's/^/kept: /'
    fi
    # Draining thousands of instances can outlast an autoscaling group's
    # 10-minute delete timeout; destroy again until nothing is left.
    for i in 1 2 3; do
      terraform -chdir=$TF destroy -auto-approve -input=false -var "regions={}" && break
      echo "down: destroy incomplete (try $i of 3); retrying"
    done ;;
  *) sed -n 2,9p "$0"; exit 2 ;;
esac
