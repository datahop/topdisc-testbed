#!/bin/sh
# AWS driver: build the binaries, provision, push, run, collect, tear down.
#   deploy/aws.sh up <scenario> [tf args]  terraform apply sized from the scenario + build + push to S3 (e.g. -var spot=true)
#                                          refuses if nodes x on-demand price x MAX_HOURS (6) > MAX_SPEND ($200)
#   deploy/aws.sh push                  rebuild and re-push the binaries only
#   deploy/aws.sh run scenarios/x.yaml  run it on the coordinator, wait, pull the results, then destroy everything
#                                       up and run destroy on any failure or interrupt too; KEEP=1 keeps the deployment
#   deploy/aws.sh check                 list what is still running in every region (instances, NAT gateways)
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
  cp deploy/inventory-aws.sh out/
  tar czf out/topdisc-linux-arm64.tgz -C out topdisc-node hostagent testbed inventory-aws.sh
  aws s3 cp out/topdisc-linux-arm64.tgz "s3://$(tfout binaries_bucket)/topdisc-linux-arm64.tgz"
}

MAX_SPEND=${MAX_SPEND:-200}
MAX_HOURS=${MAX_HOURS:-6}
# On-demand us-east-1 prices, $/h: nodes on t4g.nano, coordinator m7g.large,
# one NAT gateway per active region. Spot is cheaper, so this is conservative.
cost_guard() {
  est=$(echo "$1" | python3 -c '
import json,sys
f=json.load(sys.stdin); n=sum(f.values()); active=sum(1 for v in f.values() if v>0)
print(round(n*0.0042 + 0.0816 + active*0.045, 2))')
  total=$(python3 -c "print(round($est*$MAX_HOURS,2))")
  echo "estimated: \$$est/h on-demand, \$$total over MAX_HOURS=$MAX_HOURS (cap \$$MAX_SPEND); the coordinator scales the fleet to zero after $MAX_HOURS h"
  python3 -c "import sys; sys.exit(0 if $total <= $MAX_SPEND else 1)" || { echo "refusing: over MAX_SPEND; lower MAX_HOURS or the fleet"; exit 1; }
}

# Runs a command on the coordinator through SSM and prints its output.
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
    echo "fleet: $regions, home $home"
    cost_guard "$regions"
    terraform -chdir=$TF init -input=false >/dev/null
    terraform -chdir=$TF apply -auto-approve -input=false -var "regions=$regions" -var "home_region=$home" \
      -var "max_hours=$MAX_HOURS" "$@"
    build_push ;;
  push) build_push ;;
  run)
    # Destroy at the end unless KEEP=1; a failed pull keeps the deployment so
    # the results can still be fetched by hand (deploy/aws.sh pull / ssh).
    [ -n "$KEEP" ] || trap 'st=$?; if [ $st -eq 0 ] || [ -z "$PULL_FAILED" ]; then "$0" down; "$0" check; else echo "pull failed: deployment kept; fix, then deploy/aws.sh pull && deploy/aws.sh down"; fi' EXIT
    aws s3 cp "$2" "s3://$(tfout binaries_bucket)/scenario.yaml"
    ssm_run "cd /opt/topdisc && aws s3 cp s3://$(tfout binaries_bucket)/scenario.yaml scenario.yaml && ./inventory-aws.sh && (nohup ./testbed scenario.yaml > run.out 2>&1; echo RUN-EXIT \$? >> run.out) > /dev/null 2>&1 &"
    echo "started on the coordinator; waiting"
    while :; do
      out=$(ssm_run "tail -c 4000 /opt/topdisc/run.out" 2>/dev/null || true)
      echo "$out" | grep -qE "^RUN-EXIT" && break
      echo "$out" | grep -E "backend:|nodes started|\[churn\]" | tail -1
      sleep 30
    done
    echo "$out" | grep -vE "^PARAMS"
    "$0" pull || { PULL_FAILED=1; exit 1; } ;;
  check)
    for r in $(sed -n 's/^REGIONS = \[\(.*\)\]/\1/p' $TF/gen.py | tr -d '",'); do
      n=$(aws ec2 describe-instances --region $r --filters Name=instance-state-name,Values=pending,running --query 'length(Reservations[].Instances[])' --output text)
      g=$(aws ec2 describe-nat-gateways --region $r --filter Name=state,Values=pending,available --query 'length(NatGateways)' --output text)
      echo "$r: instances=$n nat-gateways=$g"
    done ;;
  ssh) aws ssm start-session --target "$(tfout coordinator_id)" ;;
  down) terraform -chdir=$TF destroy -auto-approve -input=false -var "regions={}" ;;
  *) sed -n 2,9p "$0"; exit 2 ;;
esac
