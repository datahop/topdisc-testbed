# Cloud backend

One node per instance, no network emulation: the cloud's own network is the
WAN, so a scenario says where nodes live instead of what latency to emulate.
`scenario.network.regions` maps regions to weights and
`scenario.network.node_regions` pins individual node indices to a region; `testbed fleet
<scenario>` turns that into instance counts and `deploy/aws.sh up` provisions
one autoscaling group per region, VPC-peered in a full mesh, plus a
coordinator in `testbed.cloud.home_region`. Every instance boots the
hostagent through `deploy/cloud-init.yaml.tftpl`. Supported regions are
listed in `deploy/terraform/aws/gen.py`; `latency_ms` and `bandwidth_mibps`
in the same block are simnet's model of the same thing and are ignored here.

AWS has a driver that does every step; the other providers follow the
manual sequence below it.

```
deploy/aws.sh up scenarios/cloud-1k.yaml -var spot=true   # fleet sized from scenario.network.regions; build; push to S3
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

Spending cap: `deploy/aws.sh up` refuses a fleet whose on-demand cost over
`MAX_HOURS` (6) exceeds `MAX_SPEND` ($200); the account has a $200 monthly
budget `topdisc-testbed` with e-mail alerts (set once with `aws budgets
create-budget`, outside Terraform so a destroy does not remove it); and the
coordinator scales every region's autoscaling group to zero `max_hours` after
boot, so a forgotten fleet stops costing on its own. `deploy/aws.sh down` still has to remove the rest (NAT
gateways, coordinator, bucket).

Quotas: 10k instances need the account's vCPU (AWS, GCP) or server (Hetzner)
limit raised first. Spot/preemptible is fine for short runs; for 24 h churn
runs use on-demand, because a reclaimed instance is indistinguishable from
churn in the traces.

## Grid'5000

`deploy/g5k/g5k.py` (EnOSlib) has the same verbs and failure semantics as
`deploy/aws.sh`, validated end to end against `nancy`/`gros`:

```
pip install -r deploy/g5k/requirements.txt
deploy/g5k/g5k.py up  scenarios/g5k-smoke.yaml   # OAR job + kadeploy + hostagent on every machine + inventory.json
deploy/g5k/g5k.py run scenarios/g5k-smoke.yaml   # runs on the coordinator, waits, pulls into runs/, releases the job
deploy/g5k/g5k.py check
```

`testbed.g5k.cluster` is required (EnOSlib has no default cluster for a
site). `testbed fleet <scenario>` prints the machine count next to the
region counts. The walltime is the hard cap. `KEEP=1` keeps the job after
`run`; a failed pull keeps it too.

Oversubscription (`testbed.g5k.vnodes_per_machine` logical nodes per
physical machine) reuses `testbed.wan`'s own machinery -- the same
netns-per-node + netem bridge that `local` and `cloud`/AWS already use
(`pkg/host`, driven remotely through `cmd/hostagent`) -- instead of one
virtual machine or container per node. No per-node IP or subnet reservation
is needed: hosts route each other's private `/16`s over the ordinary prod
network hostagent already needs for SSH, so `up` reserves and kadeploys
`testbed fleet`'s machine count and nothing else.

This replaced an earlier Distem-based design (one LXC vnode per node, own
IP from a reserved `/22`, driven by `distem-bootstrap`/`distemctl`): Distem
turned out to have no working install path on any Debian release Grid'5000
currently deploys (its packaging targets buster/stretch, both EOL, and
conflicts with newer Ruby on every deployable reference env; the `-g`
git-build fallback finds no `distem` source package at all). `cmd/distemctl`
and `pkg/distem` are dead code now -- left in place rather than deleted in
case Distem's Grid'5000 packaging gets revived, but nothing in `deploy/g5k`
calls them any more.

## Provisioner contract

Every driver (`deploy/aws.sh`, `deploy/g5k/g5k.py`, a future one) provides
`up <scenario>`, `run <scenario>`, `pull`, `down`, `check` with the same
meaning, produces an `inventory.json` of `{hosts:[{index, ip, nodes, region,
port?}]}` with a hostagent reachable on every host, and leaves nothing running
after `run` unless `KEEP=1`. The coordinator (`./testbed`, backend `cloud`)
and the node binaries are the same everywhere; only provisioning differs.
