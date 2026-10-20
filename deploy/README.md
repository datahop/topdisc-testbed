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
deploy/aws.sh up scenarios/aws-7k5-topdisc-cont.yaml -var spot=true   # fleet sized from scenario.network.regions; build; push to S3
deploy/aws.sh run scenarios/aws-7k5-topdisc-cont.yaml
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
./testbed aws-7k5-topdisc-cont.yaml                          # scenario copied next to inventory.json

terraform destroy
```

The inventory is `{coordinator, hosts:[{index, ip, nodes}]}`, one host per
instance. The coordinator assigns node indices in inventory order (node 0,
the bootnode, on the first instance), posts each instance its assignment,
starts everything, applies churn by killing and restarting over HTTP, and
fetches the traces at the end. Before fixing the phases it waits for every
hostagent to answer (up to 15 min; instances finish cloud-init at their own
pace), and at the end it retries each trace fetch a few times and reports the
missing ones by region. `run` starts the coordinator without waiting on the
SSM command (a long run would outlive the command's timeout) and polls the
run log; a run that fails is pulled for its logs and destroyed.

Spending cap: `deploy/aws.sh up` prices the fleet with `deploy/aws-cost.py`
and refuses one whose ceiling over `MAX_HOURS` (6) exceeds `MAX_SPEND` ($200).
The expected cost puts every node on the first instance type; the ceiling puts
them on the dearest type in `instance_types`, which the autoscaling group falls
back to when a pool runs dry (two thirds of a 7,500-node fleet on 2026-10-18
came up on t4g.micro and t4g.small). Both include the 8 GB root volume per
node and a cross-region traffic allowance of `TRANSFER_GB_NODE_H` (0.1) GB per
node-hour at $0.02/GB. Once the fleet is up, `deploy/aws.sh run` prices it as
launched: a fleet that is short or more than `FLEET_TOLERANCE` (10 %) over the
expected cost prints a warning and goes on, or is destroyed with
`FLEET_STRICT=1`. A nano-only `instance_types` keeps the ceiling equal to the
expectation at the price of a short fleet when nano runs out. The coordinator
holds every trace while it aggregates, and 7,500 nodes overflowed the 8 GB of
an m7g.large, so `up` picks m7g.xlarge from 4,000 nodes and m7g.2xlarge from
8,000 unless the args name a `coordinator_type`; traces that fail to fetch at
the end get a slower second pass before the run gives them up. Hosts set
`net.core.rmem_default` to 4 MB so a busy registrar's UDP socket does not
drop under the default 208 kB buffer, which 15 hosts hit on 2026-10-18. The account has a $200 monthly
budget `topdisc-testbed` with e-mail alerts (set once with `aws budgets
create-budget`, outside Terraform so a destroy does not remove it); and the
coordinator scales every region's autoscaling group to zero `max_hours` after
boot, so a forgotten fleet stops costing on its own. `deploy/aws.sh down` still has to remove the rest
(coordinator, VPCs, buckets).

Addressing: by default the instances have private addresses only and the
nodes talk over the peered VPCs. geth treats those as LAN, so its per-/24
table limits and the admission IP score do not run. With
`testbed.cloud.public_ips: true` every node instance gets a public IPv4 (about
$0.005 an hour each) and the nodes advertise and use those, so the limits run
as on mainnet; the node ports (30300-30999, UDP and TCP) are then open to the
internet. The coordinator reaches the hostagents over the private addresses
in both cases. Private nodes need nothing from the internet: they fetch the
binaries from S3 through their region's gateway endpoint, which only reaches
buckets in that region, so every active region away from home gets a mirror
bucket of the binaries (`binaries_buckets` output; `aws.sh push` fills them
all). Only the coordinator has a public address, for SSM and the AWS APIs.

Quotas: 10k instances need the account's vCPU (AWS, GCP) or server (Hetzner)
limit raised first. Spot is for smokes only: AWS reclaimed 13 % of a 540-node fleet
within 35 min (2026-09-28), and each reclaimed node loses its trace; evaluation
runs use on-demand, also because a reclaimed instance is indistinguishable from
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

Sizing, measured on one `gros` machine at Nancy with the crawl workload
(2026-09-28): a node costs 21.6 MB RSS and about a third of a percent
of one thread in steady state, so memory would allow some 3400 nodes per
machine; what binds first is the host's networking. At 750 nodes per machine
the kernel neighbour table overflowed its defaults and took the host off the
network, and the agent ran out of file descriptors. `prepare` now raises the
neighbour-table thresholds and the driver starts the agent with 65536
descriptors; the scenarios keep `vnodes_per_machine` at 250, and 500 is the
next step to validate. Prepare itself costs about 0.23 s per node (one `ip`
invocation each for namespace, veth and qdisc), which is why it has its own
deadline.

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
