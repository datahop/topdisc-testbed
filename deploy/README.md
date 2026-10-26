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
DRY_RUN=1 deploy/aws.sh up --workload aws-smoke --profile aws   # compose and print the fleet only, no AWS call
deploy/aws.sh ssh                    # tail -f /opt/topdisc/run.out
deploy/aws.sh pull                   # run directory into runs/
deploy/aws.sh down
```

Instances fetch the binaries from S3 in a retry loop, so `up` may push after
they boot. `aws login` (or credentials in the environment) is the only
prerequisite.

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

## Google Cloud

`deploy/gcp.sh` has the verbs of `deploy/aws.sh` on packed hosts, the
Grid'5000 layout rather than the AWS one: a VM per
`testbed.gcp.vnodes_per_machine` nodes (200 on an `n2-standard-8`), each
node in its own netns on the host's bridge (`pkg/host`, so `testbed.wan`
must be enabled; its star model gives each node its delay, there is no
per-region RTT on this backend), plus a coordinator VM, all in one zone with
private addresses.

```
gcloud auth login && gcloud auth application-default login && gcloud config set project <project>
gcloud services enable compute.googleapis.com iam.googleapis.com cloudresourcemanager.googleapis.com iap.googleapis.com
deploy/gcp.sh up  scenarios/gcp-smoke.yaml    # terraform apply sized by `testbed fleet` (gcp_machines); build amd64; push to the bucket
deploy/gcp.sh run scenarios/gcp-smoke.yaml    # runs on the coordinator, waits, pulls into runs/, destroys
deploy/gcp.sh check
DRY_RUN=1 PROJECT=x deploy/gcp.sh up --workload smoke --profile gcp-smoke --set population.nodes=400   # fleet only
```

Host i is the VM `topdisc-host-i` and owns the node subnet
`10.(100+i).0.0/16`: a VPC route per host sends that /16 to it and the hosts
carry IP forwarding, so packets to a node reach its VM and the kernel
forwards them to the namespace. `inventory-gcp.sh` therefore lists the hosts
by VM index and refuses a gap, and writes `routes: network` so the hosts
install no route via each other: a VM holds a /32 with every peer behind
the gateway (the virtual network answers ARP for the gateway only), and the
VPC routes carry the node traffic. Only the synthetic addressing works this
way; `population.addresses: crawl` needs hosts on one L2 that route each
other's /24s (Grid'5000) and is refused on this inventory. On any inventory
without `routes: network`, `prepare` refuses a peer that is not on-link
before creating anything.
Everything lives in `deploy/terraform/gcp` (network, bucket, two service
accounts, hosts, routes, coordinator) and `deploy/gcp-startup.sh.tftpl`
(sysctls, binaries from the bucket, hostagent unit on the hosts, save-runs
and the scale-down timer on the coordinator).

Spending: `up` prices the fleet (on-demand, us-central1) and refuses one
over `MAX_SPEND` ($100) for `MAX_HOURS` (6), checks the region's `CPUS`, the
family's CPUs (`N2_CPUS`, …) and the project's `CPUS_ALL_REGIONS` against
the fleet, and the coordinator deletes the hosts and halts `max_hours`
after boot. `down` copies the bucket's run archives to `runs/archives`
before destroying. A fresh project has `CPUS_ALL_REGIONS` 24 and E2 24 per
region: 3 `n2-standard-8` hosts once raised to 96 (2026-10-20). The
`gcp-15k` scenarios need 608 vCPUs and cost about $30/h, so they pass the
guards only with the quota raised and `MAX_HOURS=3` (or a higher
`MAX_SPEND`). Zones run out of a machine type at times; `-var
zone=… -var machine_type=…` after the scenario overrides it for one
deployment. Validated densities on an 8-vCPU host: 200 (smoke) and 500
(`gcp-density.yaml`).

## AWS packed hosts

`deploy/aws-packed.sh` is the Google Cloud layout on EC2, for runs the
one-node-per-instance AWS backend above cannot afford: an instance per
`testbed.aws_packed.vnodes_per_machine` nodes (500 on an `m6a.2xlarge`, 8
vCPUs and 32 GB), each node in its own netns with `testbed.wan`'s star-model
delays, plus a coordinator, all in one subnet of one zone. The subnet's
route table sends host i's `10.(100+i).0.0/16` to its network interface; the
hosts forward and run without the source/destination check. Every instance
has a public IPv4 for S3 and SSM only (nothing gets in from outside the
VPC); the coordinator is reached through SSM, as in `deploy/aws.sh`, saves
every run to the bucket, and `inventory-aws-packed.sh` lists the hosts by
instance index with `routes: network`.

```
export AWS_PROFILE=datahop-testbed
deploy/aws-packed.sh up  scenarios/aws-packed-smoke.yaml    # terraform apply sized by `testbed fleet` (aws_packed_machines); build amd64; push to the bucket
deploy/aws-packed.sh run scenarios/aws-packed-smoke.yaml    # runs on the coordinator, waits, pulls into runs/, destroys
deploy/aws-packed.sh check
DRY_RUN=1 deploy/aws-packed.sh up --workload smoke --profile aws-packed-smoke -var host_disk_gb=20   # fleet only
```

Spending: `up` prices the fleet (on-demand, us-east-1) and refuses one over
`MAX_SPEND` ($150) for `MAX_HOURS` (6), checks the region's on-demand vCPU
quota (L-1216C47A, 5,100 in us-east-1 on 2026-10-21) and the route-table
route quota (L-93826ACB, 500) against the fleet, and the coordinator
terminates the hosts and halts `max_hours` after boot. `down` copies the
bucket's run archives to `runs/archives` before destroying. The `aws-25k`
scenarios run on 50 hosts and an `r6a.8xlarge` coordinator (64 GB ran out
aggregating 25k traces) for about $19.5 an hour, $60 a run.
`terraform -chdir=deploy/terraform/aws-packed test` checks the resource
names and tags offline (mock provider, no credentials).

Parallel deployments: `DEPLOY=<id>` (1 to 20 characters of a-z, 0-9 and
`-`, starting with a letter or digit, not `default`) makes every verb act
on deployment `<id>` alone. Its state is terraform workspace `d-<id>`
(`deploy/terraform/aws-packed/terraform.tfstate.d/d-<id>`, left empty after
`down` and reused by the next `up`), its resources are named
`topdisc-packed-<id>-*` (instances `topdisc-<id>-host-N`) and tagged
`topdisc-deploy=<id>`, its binaries build in `out-<id>`, and its coordinator
lists and, at `max_hours`, terminates only its own hosts. `check` with an id
lists that deployment and its bucket; without one it lists every topdisc
instance, with a `topdisc-deploy` column. An un-ID'd deployment assumes it
is alone in the region, so `up` with an id refuses while an un-ID'd fleet
exists (pending to stopped) and `up` without one refuses while an ID'd
fleet exists, also when re-running `up` on a live deployment: parallel
deployments need an id on every one. `MAX_SPEND` and `max_hours` apply to
each deployment separately; N chains spend up to N caps. The quota guard of
two `up`s started together sees neither fleet, so stagger them by a minute,
and run
`terraform -chdir=deploy/terraform/aws-packed init` once before the first
parallel pair in a fresh checkout (two first-ever `init`s racing corrupt
`.terraform`). Terraform run by hand needs both `TF_WORKSPACE=d-<id>` and
`TF_VAR_deploy=<id>` exported; with only one it plans to rename everything.
Runs of the same scenario are told apart by their timestamp (`--name N` to
name them). A chain guards on its own id:

```
export AWS_PROFILE=datahop-testbed DEPLOY=a5k-21
live() { aws ec2 describe-instances --region us-east-1 --filters Name=tag:topdisc-deploy,Values=$DEPLOY Name=instance-state-name,Values=pending,running,stopping,stopped --query 'length(Reservations[].Instances[])' --output text; }
[ "$(live)" = 0 ] || { echo "ABORT: $DEPLOY live"; exit 1; }
deploy/aws-packed.sh up scenarios/$s.yaml && deploy/aws-packed.sh run scenarios/$s.yaml
```

A chain without an id keeps its `tag-key=topdisc` guard, which also aborts
while any ID'd deployment is live, as it should.

## Hetzner Cloud

`deploy/hetzner.sh` is the Google Cloud layout on Hetzner servers: a server
per `testbed.hetzner.vnodes_per_machine` nodes (500 on a `cx53`, 16 shared
vCPUs and 32 GB), each node in its own netns with `testbed.wan`'s star-model
delays, plus a coordinator, on a private network that routes one /16 per
host to it (`hcloud_network_route`); the hosts forward, and cloud-init adds
the route to the node subnets via the private gateway, without which that
traffic leaves by the public interface. Every server keeps a public IPv4
(Hetzner has none without) for SSH only; the driver pushes the binaries
over it and starts the agents, writes `inventory.json` (`routes: network`)
from the terraform outputs, and pulls the run directory with rsync.

```
printf '%s' '<token>' > ~/.config/hcloud/token && chmod 600 ~/.config/hcloud/token   # project API token, read & write
deploy/hetzner.sh up  scenarios/hetzner-smoke.yaml    # terraform apply sized by `testbed fleet` (hetzner_machines); build; push; agents; inventory
deploy/hetzner.sh run scenarios/hetzner-smoke.yaml    # runs on the coordinator, waits, pulls into runs/, destroys
deploy/hetzner.sh check                               # servers up and what they have cost so far
DRY_RUN=1 deploy/hetzner.sh up --workload hetzner-smoke --profile hetzner-smoke   # fleet only
```

Spending: `up` prices the fleet from the API (gross, hourly) and refuses one
over `MAX_SPEND` (EUR 100) for `MAX_HOURS` (6); Hetzner has no budget or
spending cap of its own, no usage endpoint (`check` prices the running
servers by uptime) and no scale-down timer here, since that would put the
token on a server: a laptop that dies mid-run leaves the servers up until
`down`. A new project is limited to 16 shared cores and cannot request more
until its first invoice is paid; the 25k matrix needs 900 shared cores and
60 servers. Older types (`cpx41`, `cpx51`) can no longer be ordered.
Validated 2026-10-20: `hetzner-smoke.yaml`, 400 nodes on 2 `cx43`, recall
1.000, no UDP drops, EUR 0.15 an hour.

## Grid'5000

`deploy/g5k/g5k.py` (EnOSlib) has the same verbs and failure semantics as
`deploy/aws.sh`, validated end to end against `nancy`/`gros`:

```
pip install -r deploy/g5k/requirements.txt
deploy/g5k/g5k.py up  scenarios/g5k-smoke.yaml   # OAR job + kadeploy + hostagent on every machine + inventory.json
deploy/g5k/g5k.py run scenarios/g5k-smoke.yaml   # runs on the coordinator, waits, pulls into runs/, releases the job
deploy/g5k/g5k.py check
DRY_RUN=1 deploy/g5k/g5k.py up --workload g5k-smoke --profile g5k-gros-smoke   # build and print the fleet only, no EnOSlib
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
`up <spec>`, `run <spec>`, `pull`, `down`, `check` with the same
meaning, where `<spec>` is a scenario file or `--workload W --profile P
[--set key=value]... [--name N]` composed by `./testbed compose` into one file
the coordinator runs as `scenario.yaml` (`run` composes with the local
`./testbed`, which `up` rebuilds; after pulling new code, run `up` or `go build
-o testbed ./cmd/testbed` first), produces an `inventory.json` of `{hosts:[{index, ip, nodes, region,
port?}]}` with a hostagent reachable on every host, and leaves nothing running
after `run` unless `KEEP=1`. The coordinator (`./testbed`, backend `cloud`)
and the node binaries are the same everywhere; only provisioning differs.
