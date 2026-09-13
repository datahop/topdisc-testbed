# Phase 3 coverage

What the repo must provide to run the Phase 3 experiment plan, and where it
stands. Backends: **S** = simnet (in-process, harness-global measurements),
**R** = real backends (local / AWS / Grid'5000: one `p2p.Server` process per
node, per-node traces). Status: ✓ done, ◐ partial, ✗ missing, — not testbed
work.

## 1. Evaluation parameters

| Plan (§1) | Scenario key | S | R | Notes |
|---|---|---|---|---|
| 10,000 nodes | `population.nodes` | ✓ | ✓ AWS (quota pending), ✗ Grid'5000 | g5k: vnodes-per-machine unknown until step 3 |
| WAN: pair RTT 8–91 ms (mean 34), 20 KB/s per node | `network.latency_ms`, `bandwidth_mibps` (S); `testbed.wan` star (local); `network.model: star` (g5k) | ◐ single latency, not a distribution (#115) | local ◐ untested (needs root); AWS = real network; g5k ✗ step 6 | AWS uses real geography via `network.regions` |
| 300 services, Zipf α=1, one per node | `population.topics: 300`, `zipf_s: 1.0`, `all_register` | ✓ | ✓ | popularity binning for figures: #113 |
| F_lookup = 30, lookup ends at F results | `search.target_count: 30`, `search.model: continuous` | ✓ | ✓ | |
| L intervals, one lookup per node at a random time in each | `search.model: scheduled` | ✗ | ✗ | step 6 (#114) |
| K_lookup = 5, K_register = 3, F_return = 10 | `topic.search_bucket_size`, `topic.nodes_per_source_bucket`, `topic.topic_nodes_limit` | ◐ | ◐ | plan values ≠ code defaults (5/16/16); decision on #12 |
| E = 15 min, ads renewed | `topic.ad_lifetime` | ◐ | ◐ | renewal on expiry (#77) not on topdisc |
| C = 1000 | `topic.ad_cache_size` | ✓ | ✓ | code default 5000 (#12) |
| Churn rate axis (§2 churn plots) | `session_churn.scale`, `session_churn.model`, `churn.*` | ✓ | ✓ kill/restart | Nebula-fitted model in `scenarios/models` |
| Deployment % 1/5/10/25/50 (§4), same fraction per service | `population.legacy_frac` | ◐ not stratified per service | ✗ | step 7; `vanilla_frac` (stock geth binary) is simnet-only |
| Legacy discv5 population (§3) | node `-legacy` mode | ✗ | ✗ | step 7: no topic packets, service in ENR, random-walk lookups |
| Seeds | `population.seed` | ✓ | ✓ | keys, topics, churn and placement derive from it |
| Node locations | `network.regions`, `node_regions` | — | ✓ AWS, g5k step 6 | |
| Advertiser IP distribution from the DNS list (§5, §6) | — | — | — | #117; §5 tool and §6 simulator |

### Scenarios the plan needs

| Scenario | Purpose | Exists |
|---|---|---|
| `phase3-10k` | §2 baseline: plan values above, no churn | ✗ step 6 |
| `phase3-10k-churn-<scale>` | §2 churn resilience sweep | ◐ `10k-session-churn-*` (simnet) |
| `phase3-10k-legacy` | §3: same population, IPs, schedule; legacy nodes | ✗ step 7 |
| `phase3-10k-deploy-<pct>` | §4 sweep | ◐ `smoke-500-*`-style penetration runs (simnet) |
| per-backend variants | same `scenario:` block, different `testbed:` | ✓ pattern (`cloud-*`, `local-*`, `smoke-*`) |

## 2. Figures and metrics

Stems refer to `figures/figures.py` (needs m.json), `figures/figures_overhead.py`
(series.json / oh.json) and `figures/report_plots.py` (cross-run). R produces
oh.json only; m.json and series.json for R are the trace-parity task (step 9).

### §2 registration and cache

| Plot | Stem | S | R |
|---|---|---|---|
| Cache utilisation over time (vs Python simulator) | `oh_08_cache_utilisation` | ✓ | ✗ parity (periodic ads-held sample) |
| Registration waiting time vs popularity | `oh_05_wait_time_cdf` | ✓ per topic | ✗ parity (quoted/admitted waits) |
| Time to first / to complete registrations across buckets, by popularity | `07_registration_latency_bar`, `07_placement_time_idspace` | ◐ first admission; per-bucket completion #116 | ✗ parity |
| Registrars per advertiser, ads per registrar | `06_fanout_both_views`, `04b_id_space_registrars` | ✓ | ✗ parity (cache snapshots) |

### §2 search and discovery

| Plot | Stem | S | R |
|---|---|---|---|
| Lookup latency vs popularity | `02b_time_to_first_cdf` | ✓ | ◐ latency per lookup in trace; popularity join needs parity |
| Fraction of advertisers discovered vs popularity | `02_recall_reached` | ✓ | ✗ parity (result ids) |
| Registrars/nodes contacted per lookup vs popularity | — | ✗ #116 | ✗ |
| Times each advertiser is discovered | `05_id_space_found_vs_missed` | ✓ | ✗ parity |
| Placement → first discovery | `oh_06_idspace_found_time` | ✓ | ✗ parity |

### §2 load distribution

| Plot | Stem | S | R |
|---|---|---|---|
| Registration and lookup contacts across the service-centred ID space | — | ◐ traffic, not contacts (`oh_01`) | ◐ same |
| Total messages/bytes per node | `oh_01_idspace_traffic` | ✓ | ✓ |
| Per-node traffic by registration / renewal / lookup | `oh_03_idspace_msgtype`, `oh_10_reg_vs_lookup` | ◐ no renewal split (#116) | ◐ same |
| Load vs node distance from service IDs | `oh_07_load_vs_topic_distance` | ✓ | ✗ needs topic ids (parity) |
| Summary: median, p95, p99, max, CV | — | ✗ trivial from oh.json | ✗ |

### §2 churn resilience

| Plot | Stem | S | R |
|---|---|---|---|
| Fraction of returned ads that are dead vs churn rate | `churn_deadresult` | ✓ | ✗ parity (result ids × churn schedule) |
| Age of dead ads vs churn rate | `churn_deadage` | ✓ | ✗ parity |
| Lookup success and latency vs churn rate | `search_ttf_by_rate`, `search_discovery_by_rate` | ✓ | ◐ latency yes, success needs result ids |
| Registrations maintained per advertiser vs churn rate | `reg_fanout_by_rate` | ◐ needs renewal (#77) | ✗ |

### §3 TopDisc vs legacy discv5 (all need step 7)

| Plot | S | R |
|---|---|---|
| Lookup latency, by popularity | ✗ | ✗ |
| Fraction of peers discovered, by popularity | ✗ | ✗ |
| Nodes contacted per lookup, by popularity | ✗ | ✗ |
| Messages/bytes per lookup | ✗ | ✗ |
| Per-node lookup traffic distribution | ✗ | ✗ |
| Background registration/renewal overhead | ✓ (TopDisc side) | ✓ `oh_10` |

### §4 partial deployment

| Plot | S | R |
|---|---|---|
| Startup → first TopDisc-capable peer via legacy discv5 | ✗ | ✗ step 7 event |
| Start of registration → first successful registration vs deployment | ◐ visibility runs exist | ✗ parity |
| Fraction of advertisers discovered vs deployment | ✓ (incremental-deployment runs) | ✗ |
| Lookup latency vs deployment | ✓ | ◐ |
| Registrars contacted per registration and per lookup vs deployment | ✗ #116 | ✗ |
| Messages/bytes per lookup vs deployment | ◐ | ◐ |
| Per-node load vs deployment | ✓ | ✓ |
| Small / medium / popular service split | ✗ #113 binning | ✗ |

### §5 standalone registrar overhead — not covered

Memory vs cache capacity/occupancy, registration and lookup processing latency
vs occupancy, CPU vs request rate, throughput vs offered rate, lower-bound-state
overhead, validation cost. Needs a load generator driving one registrar
(`cmd/regbench`, #121); the host monitoring of step 8 supplies the CPU/RSS axes.

### §6 admission-control security — not testbed

Python simulator (#123, #124).

## Totals

| | done | partial | missing |
|---|---|---|---|
| Parameters (13 rows) | 6 | 5 | 2 |
| Figures §2–§4 on simnet (32) | 17 | 7 | 8 |
| Figures §2–§4 on real backends (32) | 3 | 5 | 24 |

The real-backend column collapses to the simnet column once trace parity
(step 9) lands, except the three items that are missing on both (contacts per
lookup, renewal split, popularity binning: #116, #113).
