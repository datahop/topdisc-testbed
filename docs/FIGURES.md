# Figures and metrics

Every plot the Phase 3 plan asks for, what it measures, which trace it is
computed from, and where it stands per backend. Stems are the file names
written by `figures/figures.py` (reads `metrics.json`), `figures/figures_overhead.py`
(reads `series.json`, `oh.json`, `metrics.json`) and `figures/report_plots.py`
(cross-run). `docs/TRACES.md` describes the trace files.

Backends: **simnet** writes all three traces from its global view. **real**
(local, AWS, Grid'5000) writes per-node traces that the coordinator turns into
the same three files (`pkg/coordinator/metrics.go`): lookups carry result ids
and found-at times, registrars snapshot the ads they hold every second, nodes
report quoted/admitted waits and periodic counters; the coordinator joins them
with the assignments. All 21 per-run figures render from a real-backend run. Status: ✓ done,
◐ partial, ✗ missing.

## §2 Registration and cache behaviour

| Plot | Measures | Computed from | Stem | simnet | real |
|---|---|---|---|---|---|
| Cache utilisation over time | Ads held network-wide and per service against capacity C, sampled periodically; the plan compares it with the Python simulator under the same workload. | `series.json` `samples[].cacheHeld / cacheCap / cacheByTopic` | `oh_08_cache_utilisation` | ✓ | ✓ |
| Registration waiting time vs popularity | The wait a registrar quotes an advertiser (every quote) and the cumulative wait until admission (one per successful registration), per service. | `series.json` `waitTime[].quotedMs / admittedMs` | `oh_05_wait_time_cdf` | ✓ | ✓ |
| Time to first registration and to complete the intended registrations across buckets, by popularity | From an advertiser's start until its ad is first admitted by a remote registrar, and until every bucket of its registration table holds an ad. | `metrics.json` `registrationTimingNs`, `registrationStartNs`; per-bucket completion not recorded | `07_registration_latency_bar`, `07_placement_time_idspace`, `07b_placement_mean_idspace` | ◐ first admission only (#116) | ◐ first admission plotted; per-bucket completion recorded (`registrationBucketFullNs`, `registrationCompleteNs`), no figure yet |
| Registrars per advertiser, ads per registrar | Fan-out: how many registrars hold each advertiser's ad; load: how many ads each registrar holds. | `metrics.json` `registrationCoverage.byRegistrant / byHost` | `06_fanout_both_views`, `04b_id_space_registrars`, `04_id_space_registrants` | ✓ | ✓ |

## §2 Search performance and discovery

| Plot | Measures | Computed from | Stem | simnet | real |
|---|---|---|---|---|---|
| Lookup latency vs popularity | Time from a lookup's start to its first result and to completion (F_lookup reached or timeout). | `metrics.json` `results[].timeToFirstNs / timeToCompletionNs`; real traces: `lookups[].latency_ms` | `02b_time_to_first_cdf` | ✓ | ✓ |
| Fraction of advertisers discovered vs popularity | Distinct registrants found over time and at the end, against the number that exist for the service. | `metrics.json` `results[].uniqueFoundAtMs`, `perTopic[].meanRecall / fullRecall` | `02_recall_reached`, `03_unique_found_over_time` | ✓ | ✓ |
| Registrars / nodes contacted per lookup vs popularity | How many nodes a lookup queried before finishing. | not recorded | — | ✗ (#116) | ◐ recorded (`lookupQueries`, `lookupContacted`), no figure yet |
| Times each advertiser is discovered | Over all searchers of a service, how many found each registrant; shows the never-found tail. | `metrics.json` `findCountByTopic`, `results[].foundRegistrantIds` | `05_id_space_found_vs_missed` | ✓ | ✓ |
| Placement to first discovery | From an ad's first admission to the first time any searcher returns it, on the common clock. | `metrics.json` `registrationTimingNs` × `results[].uniqueFoundAtMs + searchStartMs` | `oh_06_idspace_found_time` | ✓ | ✓ |

## §2 Load distribution

| Plot | Measures | Computed from | Stem | simnet | real |
|---|---|---|---|---|---|
| Registration and lookup contacts across the service-centred ID space | Which nodes, by XOR distance from the service id, get contacted by registrations and by lookups. | contacts are not recorded; traffic is | — | ◐ traffic proxy | ◐ |
| Total messages / bytes sent and received per node | Per-node totals across the ID space. | `oh.json` `txBytes / rxBytes / txPkts / rxPkts` | `oh_01_idspace_traffic` | ✓ | ✓ |
| Per-node traffic by registration, renewal and lookup | Same split by message type; renewal is not distinguished from a fresh REGTOPIC. | `oh.json` `byType{}` | `oh_03_idspace_msgtype`, `oh_10_reg_vs_lookup` | ✓ `REGTOPIC(renewal)/v5` | ✓ `REGTOPIC(renewal)/v5` |
| Peak per-node rate across the ID space, total and by type | Highest sustained send/receive rate per node over a sliding window. | `series.json` `samples[]` differenced | `oh_02_idspace_peak_rate`, `oh_04_idspace_peak_msgtype` | ✓ | ✓ |
| Load vs node distance from service ids | Traffic against XOR distance to the topics a node is close to. | `oh.json` × `metrics.json` `topicIds` | `oh_07_load_vs_topic_distance` | ✓ | ✓ |
| Lookup traffic per searcher vs popularity | Cost of a lookup as a function of the service's size. | `oh.json` × `metrics.json` `perTopic` | `oh_09_cost_per_lookup` | ✓ | ✓ |
| Summary statistics: median, p95, p99, max, coefficient of variation | One line per run for the load distribution. | `oh.json` | — (coordinator prints percentiles) | ✗ | ◐ |

## §2 Churn resilience

| Plot | Measures | Computed from | Stem | simnet | real |
|---|---|---|---|---|---|
| Fraction of returned ads referring to unavailable nodes vs churn rate | Of the registrants a lookup returns, how many were down at that moment. | result ids × the churn schedule (harness-known on simnet, coordinator-known on real backends) | `churn_deadresult` | ✓ | ◐ ids and times are in the traces; the cross-run script still reads simnet's churn log |
| Age of stale ads vs churn rate | How long ago a dead registrant left when it was returned. | same | `churn_deadage` | ✓ | ◐ same |
| Lookup success rate and latency vs churn rate | Share of lookups reaching F_lookup, and their latency, overlaid by rate. | `metrics.json` `results[]` per run | `search_ttf_by_rate`, `search_discovery_by_rate` | ✓ | ✓ per run |
| Registrations maintained per advertiser vs churn rate | Registrars holding a live ad for each advertiser over time. | `registrationCoverage` sampled; needs renewal | `reg_fanout_by_rate`, `reg_hostload_by_rate` | ◐ renewal (#77) | ◐ same |

## §3 TopDisc vs legacy discv5

All six plots compare the same metric between a TopDisc run and a legacy run
of the same population. Legacy nodes are stock upstream geth (`legacy/`),
finding providers by table scan plus random-target lookups filtered on the
`svc` ENR entry; their lookups land in the same trace record, so latency and
fraction discovered are ✓ on real backends, contacts and bytes per lookup are
still ✗ (#116).

| Plot | Measures |
|---|---|
| Lookup latency, by popularity | Time to find F_lookup providers by topic search vs by random-walk discovery filtered on the ENR service entry |
| Fraction of peers discovered, by popularity | Providers found within the timeout |
| Nodes contacted per lookup, by popularity | Queries per lookup (#116) |
| Messages / bytes per lookup | Wire cost attributed to the lookup |
| Per-node lookup traffic distribution | Across nodes |
| Background registration / renewal overhead | TopDisc's extra cost: total REGTOPIC/REGCONFIRMATION traffic (`oh_10_reg_vs_lookup`, ✓ on every backend) |

## §4 Partial deployment

| Plot | Measures | simnet | real |
|---|---|---|---|
| Startup to first TopDisc-capable peer via legacy discv5, vs deployment % | How long a node takes to get an RLPx peer whose ENR carries the `ng` key. | ✗ | ✓ `first_capable_ms` in every trace |
| Start of registration to first successful registration, vs deployment % | As §2, per deployment level. | ◐ visibility runs exist | ✓ |
| Fraction of advertisers discovered, vs deployment % | As §2. | ✓ (incremental-deployment runs, 10–100 %) | ✓ |
| Lookup latency, vs deployment % | As §2. | ✓ | ✓ |
| Registrars contacted per registration and per lookup, vs deployment % | Contacts (#116). | ✗ | ✗ |
| Messages / bytes per lookup, vs deployment % | Wire cost. | ◐ | ◐ |
| Per-node load, vs deployment % | `oh_01` per run. | ✓ | ✓ |
| Small / medium / popular services separately | Every plot above split by popularity bin (#113). | ✗ | ✗ |

## §5 Standalone registrar overhead — not covered

Memory vs cache capacity and occupancy, registration and lookup processing
latency vs occupancy, CPU vs request rate, throughput vs offered rate,
lower-bound-state overhead, validation cost. These need a load generator
driving a single registrar at controlled rates with a chosen advertiser IP
distribution, plus pprof profiles (#121, a future `cmd/regbench`). The host
monitoring planned for the hostagent (CPU, RSS, fds, UDP drops per node)
supplies the resource axes but not the driver.

## §6 Admission-control security — not testbed work

Six plots from the Python simulator (#123, #124).

## What real backends must export for parity

The coordinator already holds the ground truth (which node registers which
service, when each node is up or down, node ids and topic ids). To build
`metrics.json` and `series.json` in the format above, each node must report:

- per registration attempt: service, registrar id, bucket or distance, quoted
  wait, admission time, whether it was a renewal;
- per lookup: start, end, result ids with the time each was first seen, nodes
  contacted, messages and bytes attributed to the lookup;
- periodic snapshot of the ad cache: service and advertiser id of every ad
  held, and the counters already in `wire{}`;
- the first time a TopDisc-capable peer was seen (§4).

The hostagent samples the periodic items on the series period; the coordinator
joins everything with the assignments and the churn schedule. Three items are
missing on simnet as well and need node-side instrumentation in the fork:
contacts per lookup, the renewal flag on REGTOPIC, and popularity binning
(#116, #113).

## Totals, §2–§4

| | done | partial | missing |
|---|---|---|---|
| simnet (34 plots) | 18 | 8 | 8 |
| real backends (34 plots) | 21 | 6 | 7 |
