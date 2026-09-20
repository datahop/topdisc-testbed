# Evaluation parameters and scenarios

The parameters of the Phase 3 experiment plan, what each one means, how it is
expressed in a scenario file, and what the repo can do with it today. A
scenario file (`scenarios/*.yaml`) has a backend-agnostic `scenario:` block
and a `testbed:` block; the same `scenario:` block is meant to run unchanged
on every backend. `./testbed reference` prints every key with its default.

Backends: **simnet** (in-process discv5 nodes, simulated links, harness sees
everything); **real** = local processes, AWS, Grid'5000 (one `p2p.Server`
process per node, per-node traces). Status: ✓ done, ◐ partial, ✗ missing,
— not the testbed's job.

## Population

| Parameter | Meaning | Scenario key | simnet | real | Notes |
|---|---|---|---|---|---|
| Network size | Number of nodes in the experiment. The plan wants at least 10,000. | `population.nodes` | ✓ (80 GB RAM, 25 min) | ✓ AWS once vCPU quotas land; Grid'5000 pending sizing | |
| Services | Distinct services (topics). The plan uses 300. | `population.topics` | ✓ | ✓ | `topics: 1` with `all_register` is the single-topic stress case |
| Service popularity | Zipf distribution of nodes over services, α = 1 in the plan. Each node gets exactly one service and registers it. | `population.topics: 300`, `zipf_s: 1.0` (`all_register` is the single-topic mode and must be off) | ✓ | ✓ | Binning services into small / medium / popular for figures is not implemented (#113) |
| Seed | Every random draw (keys, service assignment, churn schedule, placement, lookup times) derives from it, so a run is reproducible from the YAML alone. | `population.seed` | ✓ | ✓ | |
| Deployment fraction | Share of nodes that speak TopDisc; the rest are legacy discv5. §4 sweeps 1, 5, 10, 25, 50 % and wants the same fraction within every service. | `population.legacy_frac`, `legacy_bootnode` | ◐ global fraction, ENR flag removed | ✓ stock upstream geth v1.17.5 (`legacy/`), same fraction per service | The bootnode stays TopDisc unless `legacy_bootnode`: a fresh stock bootnode returns no nodes until it has revalidated its table (~20 nodes/min), which isolates TopDisc nodes for minutes |
| Stock-binary nodes | Nodes running unmodified upstream geth, for interop rather than a TopDisc-off mode. | `population.vanilla_frac` | ✓ (`-tags vanilla`) | ✗ | simnet only |

## Protocol parameters

| Parameter | Meaning | Scenario key | simnet | real | Notes |
|---|---|---|---|---|---|
| Ad lifetime E | How long an advertisement stays in a registrar's cache; 15 min in the plan. Advertisers renew before expiry. | `topic.ad_lifetime` | ◐ | ◐ | Renewal on expiry (#77) is not on `topdisc`; without it the registration count sawtooths every E |
| Cache capacity C | Ads a registrar holds in total; plan value 1000, code default 5000. | `topic.ad_cache_size` | ✓ | ✓ | Decision on #12 |
| K_lookup | Entries per distance bucket of the search table; plan 5, code default 16. | `topic.search_bucket_size` | ✓ | ✓ | #12 |
| K_register | Registrations kept per bucket by an advertiser; plan 3, code default 5. | `topic.reg_bucket_size` | ✓ | ✓ | Also `reg_bucket_standby` (default 20), `reg_table_depth` and `search_table_depth` (default 10 distance buckets each); #12 |
| F_return | Registrants returned per TOPICQUERY reply; plan 10, code default 16. | `topic.topic_nodes_limit` | ✓ | ✓ | |
| Aux nodes | Closest-to-topic nodes attached to replies (protocol constant, not in the plan). | `topic.aux_nodes_limit` | ✓ | ✓ | |
| Registration attempt timeout | Give up on a registrar after this; defaults to 1.5 × E. | `topic.reg_attempt_timeout` | ✓ | ✓ | |

Real backends pass the protocol parameters to every node through its assignment (`p2p.Config.DiscoveryV5Topic` in the fork since `v1.17.2-testbed.3`); before that tag they silently ran the fork defaults.

## Lookup workload

| Parameter | Meaning | Scenario key | simnet | real | Notes |
|---|---|---|---|---|---|
| F_lookup | A lookup ends once this many distinct providers of the service are found; 30 in the plan. | `search.target_count: 30` | ✓ | ✓ | |
| Lookup schedule | Plan: the run is divided into L equal intervals and every node does one lookup at a uniformly random time in each. | `search.model: scheduled`, `search.intervals` | ✓ | ✓ | Times drawn from the seed in the assignment. Lookup-only; the other lookup-only variant is `continuous` (one search for the whole search phase consumed at `search.initial_results`, then one new registrant per `search.result_interval`). The connection-driven mode is `conn`: nodes fill peer slots from their topic search (simnet `conn_model`, real backends geth's dialer). Lookup-only runs never dial and reject `conn_model.enabled` |
| Lookup timeout | Give up on a lookup after this even if F_lookup is not reached. | `search.request_timeout` | ✓ | ✓ | |
| Connection model | Peer slots a node fills from search results: geth's 50 peers, one third outbound. Only with `search.model: conn`; lookup-only models never dial. On real backends this is the actual `p2p.Server`; on simnet a model. | `conn_model.max_peers`, `dial_ratio`, `redial_wait` | ✓ | ✓ inherent | |

## Network conditions

| Parameter | Meaning | Scenario key | simnet | real | Notes |
|---|---|---|---|---|---|
| WAN latency | Plan: pair RTTs between 8 and 91 ms, mean 34 ms (from the paper's testbed). | simnet: `network.latency_ms` (one value for every pair); local: `testbed.wan` star model (one-way 4–45 ms per node, netns + netem); Grid'5000: `network.model: star` (per-pair matrix from the same draws) or `regions` (RTT table) | ◐ single latency, no distribution (#115) | local ◐ untested (needs a root Linux host); AWS = real network; Grid'5000 ✓ validated on nancy/gros (see deploy/README.md) | |
| Bandwidth | Plan: 20 KB/s per node. | simnet: `network.bandwidth_mibps` (per link); local/Grid'5000: `testbed.wan.rate_kbps: 160` | ✓ | ◐ | AWS: not capped |
| Node locations | Where nodes live. On AWS: real regions, weights per region and pins per node. On Grid'5000: emulated with an inter-region RTT table. | `network.regions`, `network.node_regions`, `network.rtt_table` | — | ✓ AWS; Grid'5000 step 6 | `scenarios/models/region-rtt.json`; to be replaced by RTTs measured on AWS |

## Churn

| Parameter | Meaning | Scenario key | simnet | real | Notes |
|---|---|---|---|---|---|
| Session churn | Nodes leave and come back with the session-length distribution measured on the discv5 network (42 % never leave, geometric tail of short sessions), fitted from a month of Nebula crawls. | `session_churn.enabled`, `session_churn.model: models/nebula-discv5-2026-08.json`, `window_real_hours`, `gap` | ✓ | ✓ (kill and restart the same identity) | Rejoins keep the node key, so ads for it stay valid |
| Churn rate axis | The §2 churn plots vary the rate: a multiplier on session lengths. | `session_churn.scale` (0.5 doubles the rate) | ✓ | ✓ | |
| Steady-state churn | Older model: every interval a fraction leaves and the same number of fresh nodes joins. | `churn.interval`, `churn.frac`, `churn.mode` | ✓ | ✗ | Superseded by session churn |
| Random disconnections | Drop a fraction of live connections periodically without the nodes leaving (network trouble). | `disconnect.interval`, `disconnect.frac` | ✓ | ✗ | |

## Run pacing

| Parameter | Meaning | Scenario key |
|---|---|---|
| Bootstrap wait | Time for routing tables to fill before anyone registers. | `phases.bootstrap_wait` |
| Start window | Each node starts at a random time in this window, from the seed, bootnode first, and registers `bootstrap_wait` after its own start, so registrations and ad expiries spread over the same window. Set it to the ad lifetime so 10k nodes do not register, and their ads do not expire, together. On simnet nodes are in the DHT from the beginning and registrations follow the same per-node schedule. When unset, `register_stagger` applies. | `phases.start_window` |
| Register stagger and wait | Spacing between nodes starting to register, and how long registration runs before lookups begin (1.5 × E by default so the network reaches a steady state). | `phases.register_stagger`, `register_wait` |
| Search stagger and timeout | Spacing between nodes starting to search, and the length of the search phase. | `phases.search_stagger`, `search_timeout` |

## Testbed block

Not part of the experiment, but part of reproducing it: `testbed.backend`
(`simnet`, `local`, `cloud`), simulator buffer sizes, local process settings,
WAN emulation, cloud inventory and home region, trace outputs, safety
(`abort_on_drop`). See `./testbed reference`.

## Scenarios the plan calls for

| Scenario | Plan section | Content | Exists |
|---|---|---|---|
| `phase3-10k` | §2 | 10k nodes, 300 services Zipf 1, E = 15 min, F_lookup = 30, scheduled lookups, WAN model, no churn | ✓ `scenarios/phase3-10k.yaml` |
| `phase3-10k-churn-<scale>` | §2 churn resilience | `phase3-10k` plus session churn at several `scale` values | ✓ `scenarios/phase3-10k-churn.yaml` (edit `scale`) |
| `phase3-10k-legacy` | §3 | Same population, service assignment, addresses and lookup schedule; every node in legacy mode | ◐ `legacy_frac: 1.0` on `phase3-10k` (bootnode included via `legacy_bootnode`) |
| `phase3-10k-deploy-<pct>` | §4 | `phase3-10k` with `legacy_frac` at 99, 95, 90, 75, 50 %, stratified per service | ✓ set `legacy_frac` on `phase3-10k` |
| `<name>` per backend | all | The same `scenario:` block with a different `testbed:` block | ✓ pattern: `smoke-*` (simnet), `local-*`, `cloud-*`, `g5k-*` (step 5) |

Existing scenarios by purpose: `default.yaml` (100 Mibps baseline),
`smoke-20`/`smoke-500*` (simnet checks, churn model, continuous search),
`10k-*` (simnet at scale), `local-100*` (real processes on one host, churn,
continuous, WAN), `cloud-smoke`/`cloud-300`/`cloud-1k`/`cloud-10k` (AWS),
`reference.yaml` (every key documented).
