# Cloud backend

One node per instance, no network emulation: the cloud's own network is the
WAN. Each module creates N small instances plus a coordinator on a private
network and boots the hostagent on every instance through
`deploy/cloud-init.yaml.tftpl`.

AWS has a driver that does every step; the other providers follow the
manual sequence below it.

```
deploy/aws.sh up 1000 -var spot=true # terraform apply, build linux/arm64, push to the run's S3 bucket
deploy/aws.sh run scenarios/cloud-1k.yaml
deploy/aws.sh ssh                    # tail -f /opt/topdisc/run.out
deploy/aws.sh pull                   # run directory into runs/
deploy/aws.sh down
```

Instances fetch the binaries from S3 in a retry loop, so `up` may push after
they boot. `aws login` (or credentials in the environment) is the only
prerequisite.

Manual sequence (GCP, Hetzner):

```
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o out/topdisc-node ./cmd/node
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o out/hostagent ./cmd/hostagent
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o out/testbed ./cmd/testbed
tar czf topdisc-linux-arm64.tgz -C out .        # upload anywhere the instances can fetch it

cd deploy/terraform/gcp && terraform init && terraform apply -var nodes=1000 -var binaries_url=https://…/topdisc-linux-arm64.tgz

# on the coordinator (GCP: gcloud compute ssh --tunnel-through-iap; Hetzner: ssh)
cd /opt/topdisc && ./inventory-gcp.sh           # Hetzner: terraform output -json inventory
./testbed cloud-1k.yaml                         # scenario copied next to inventory.json

terraform destroy
```

The inventory is `{coordinator, hosts:[{index, ip, nodes}]}`, one host per
instance. The coordinator assigns node indices in inventory order (node 0,
the bootnode, on the first instance), posts each instance its assignment,
starts everything, applies churn by killing and restarting over HTTP, and
fetches the traces at the end.

Quotas: 10k instances need the account's vCPU (AWS, GCP) or server (Hetzner)
limit raised first. Spot/preemptible is fine for short runs; for 24 h churn
runs use on-demand, because a reclaimed instance is indistinguishable from
churn in the traces.
