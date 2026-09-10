# Cloud backend

One node per instance, no network emulation: the cloud's own network is the
WAN. Each module creates N small instances plus a coordinator on a private
network and boots the hostagent on every instance through
`deploy/cloud-init.yaml.tftpl`.

```
# 1. build for the fleet's architecture (arm64 on all three defaults)
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o out/topdisc-node ./cmd/node
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o out/hostagent ./cmd/hostagent
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o out/testbed ./cmd/testbed
tar czf topdisc-linux-arm64.tgz -C out .        # upload anywhere the instances can fetch it

# 2. provision
cd deploy/terraform/aws && terraform init && terraform apply -var nodes=1000 -var binaries_url=https://…/topdisc-linux-arm64.tgz

# 3. on the coordinator (AWS: aws ssm start-session; GCP: gcloud compute ssh --tunnel-through-iap; Hetzner: ssh)
cd /opt/topdisc && ./inventory-aws.sh           # inventory-gcp.sh; Hetzner: terraform output -json inventory
./testbed cloud-1k.yaml                         # scenario copied next to inventory.json

# 4. tear down
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
