# Cloud backend

One scenario, three provisioners. Each module creates N node hosts plus a
coordinator host on a private network and boots the hostagent on every host
through `deploy/cloud-init.yaml.tftpl`.

```
# 1. build for the fleet's architecture (arm64 on AWS/GCP, amd64 on Hetzner)
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o out/topdisc-node ./cmd/node
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o out/hostagent ./cmd/hostagent
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o out/testbed ./cmd/testbed
tar czf topdisc-linux-arm64.tgz -C out .        # upload anywhere the hosts can fetch it

# 2. provision
cd deploy/terraform/aws && terraform init && terraform apply -var hosts=2 -var nodes_per_host=500 -var binaries_url=https://…/topdisc-linux-arm64.tgz
terraform output -json inventory > inventory.json

# 3. run from the coordinator host (or anywhere that reaches the hosts' agent port)
scp inventory.json scenarios/cloud-1k.yaml coordinator:   # AWS/GCP: aws ssm / gcloud compute ssh --tunnel-through-iap
./testbed cloud-1k.yaml

# 4. tear down
terraform destroy
```

The inventory is `{coordinator, hosts:[{index, ip, nodes}]}`; the coordinator
packs node indices onto hosts in order (node 0, the bootnode, on host 0). With
`testbed.wan.enabled` each node runs in its own netns on host `h` with an
address in `10.(100+h).0.0/16`; the modules route those subnets to the host
(`source_dest_check`/`can_ip_forward`/`hcloud_network_route`).

Spot/preemptible hosts are fine for short runs. For 24 h churn runs use
on-demand: a reclaimed host is indistinguishable from churn in the traces.
