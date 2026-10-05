#!/bin/sh
# AWS driver: build the binaries, provision, push, run, collect, tear down.
#   deploy/aws.sh up <scenario> [tf args]  terraform apply sized from the scenario + build + push to S3 (e.g. -var spot=true)
#                                          refuses if nodes x on-demand price x MAX_HOURS (6) > MAX_SPEND ($200)
#   deploy/aws.sh push                  rebuild and re-push the binaries only
#   deploy/aws.sh run scenarios/x.yaml  run it on the coordinator, wait, pull the results, then destroy everything
#                                       up and run destroy on any failure or interrupt too (a failed run is pulled first);
#                                       only a failed pull keeps the deployment; KEEP=1 keeps it always
#   deploy/aws.sh check                 list what is still running in every region (instances, NAT gateways, elastic IPs)
#   deploy/aws.sh pull                  fetch the latest run directory from the coordinator into runs/
#   deploy/aws.sh ssh                   shell on the coordinator (SSM)
#   deploy/aws.sh down                  terraform destroy
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
  cp deploy/inventory-aws.sh out/
  tar czf out/topdisc-linux-arm64.tgz -C out topdisc-node topdisc-node-legacy hostagent testbed inventory-aws.sh
  aws s3 cp out/topdisc-linux-arm64.tgz "s3://$(tfout binaries_bucket)/topdisc-linux-arm64.tgz"
}

MAX_SPEND=${MAX_SPEND:-200}
MAX_HOURS=${MAX_HOURS:-6}
# On-demand us-east-1 prices, $/h: nodes on t4g.nano, coordinator m7g.large,
# a NAT gateway per active region with private addresses, a public IPv4 per
# instance with public_ips. Spot is cheaper, so this is conservative.
cost_guard() {
  est=$(echo "$1" | python3 -c '
import json,sys
f=json.load(sys.stdin); n=sum(f.values()); active=sum(1 for v in f.values() if v>0)
pub=sys.argv[1]=="true"
print(round(n*0.0042 + 0.0816 + ((n+1)*0.005 if pub else active*0.045), 2))' "$2")
  total=$(python3 -c "print(round($est*$MAX_HOURS,2))")
  echo "estimated: \$$est/h on-demand, \$$total over MAX_HOURS=$MAX_HOURS (cap \$$MAX_SPEND); the coordinator scales the fleet to zero after $MAX_HOURS h"
  python3 -c "import sys; sys.exit(0 if $total <= $MAX_SPEND else 1)" || { echo "refusing: over MAX_SPEND; lower MAX_HOURS or the fleet"; exit 1; }
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
    cost_guard "$regions" "$pub"
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
    ssm_run "cd /opt/topdisc && aws s3 cp s3://$(tfout binaries_bucket)/scenario.yaml scenario.yaml && aws s3 cp --recursive --only-show-errors s3://$(tfout binaries_bucket)/models models && ./inventory-aws.sh && rm -f run.out"
    ssm_send "cd /opt/topdisc && setsid sh -c './testbed scenario.yaml > run.out 2>&1; echo RUN-EXIT \$? >> run.out' > /dev/null 2>&1 < /dev/null &"
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
    d=$(ssm_run "cd /opt/topdisc && d=\$(ls -td *-2* | head -1) && tar czf /tmp/\$d.tgz \$d && aws s3 cp /tmp/\$d.tgz s3://$b/runs/\$d.tgz >/dev/null && echo PULLED \$d" | sed -n 's/^PULLED //p')
    [ -n "$d" ] || { echo "pull: no run archive produced on the coordinator"; exit 1; }
    aws s3 cp "s3://$b/runs/$d.tgz" - | tar xzf - -C runs
    [ -f "runs/$d/nodes.json" ] || { echo "pull: runs/$d incomplete"; exit 1; }
    echo "runs/$d ($(ls runs/$d/traces | wc -l | tr -d ' ') traces)" ;;
  ssh) aws ssm start-session --target "$(tfout coordinator_id)" ;;
  down) terraform -chdir=$TF destroy -auto-approve -input=false -var "regions={}" ;;
  *) sed -n 2,9p "$0"; exit 2 ;;
esac
