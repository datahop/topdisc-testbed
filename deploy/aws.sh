#!/bin/sh
# AWS driver: build the binaries, provision, push, run, collect, tear down.
#   deploy/aws.sh up [nodes] [tf args]  terraform apply + build + push to S3 (e.g. up 1000 -var spot=true)
#   deploy/aws.sh push                  rebuild and re-push the binaries only
#   deploy/aws.sh run scenarios/x.yaml  copy scenario + inventory to the coordinator and run it there
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
    terraform -chdir=$TF init -input=false >/dev/null
    n=$2; [ $# -ge 2 ] && shift 2 || shift
    terraform -chdir=$TF apply -auto-approve -input=false ${n:+-var nodes=$n} "$@"
    build_push ;;
  push) build_push ;;
  run)
    aws s3 cp "$2" "s3://$(tfout binaries_bucket)/scenario.yaml"
    ssm_run "cd /opt/topdisc && aws s3 cp s3://$(tfout binaries_bucket)/scenario.yaml scenario.yaml && ./inventory-aws.sh && nohup ./testbed scenario.yaml > run.out 2>&1 &"
    echo "started; deploy/aws.sh ssh then tail -f /opt/topdisc/run.out" ;;
  pull)
    mkdir -p runs
    ssm_run "cd /opt/topdisc && d=\$(ls -td *-2* | head -1) && tar czf - \$d | aws s3 cp - s3://$(tfout binaries_bucket)/runs/\$d.tgz && echo \$d" | tail -1 > /tmp/topdisc-run
    aws s3 cp "s3://$(tfout binaries_bucket)/runs/$(cat /tmp/topdisc-run).tgz" - | tar xzf - -C runs
    echo "runs/$(cat /tmp/topdisc-run)" ;;
  ssh) aws ssm start-session --target "$(tfout coordinator_id)" ;;
  down) terraform -chdir=$TF destroy -auto-approve -input=false ;;
  *) sed -n 2,9p "$0"; exit 2 ;;
esac
