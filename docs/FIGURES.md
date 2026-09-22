# Figures and metrics

Every plot the Phase 3 plan asks for, the figure that answers it, where it
stands per backend, and how the implementation differs from the plan. Each
section ends with a **Differences and gaps** subsection.

Figures come from three scripts, all reading the files a run directory holds
(`metrics.json`, `series.json`, `oh.json`, `run.log`; `docs/TRACES.md`):

- `figures/report_run.py <run-dir>` builds the **per-run report**
  (`<run-dir>/report/report.md` plus `figures/`): it runs `figures.py` and
  `figures_overhead.py`, copies the log tables (or rebuilds them from
  `metrics.json` on real backends) and lists anything it could not produce.
- `figures/compare_runs.py --out DIR a=RUN b=RUN` overlays **runs of the same
  scenario** (e.g. simnet vs AWS) with a summary table.
- `figures/report_plots.py` draws the older **cross-run churn plots** from
  simnet sweep logs.

Backends: **simnet**, and **real** = local processes, AWS, Grid'5000.
Status: ✓ done, ◐ partial, ✗ missing. "Report" = drawn automatically in the
per-run report.

## Per-run report layout

| Section | Contents |
|---|---|
| Scenario | Parameters (unset protocol parameters shown with their fork default), topic assignment table, actual search duration when stopped early |
| 1. Registration and cache | Coverage and registration-latency tables; `06`, `04`, `04b`, `07_registration_latency_bar`, `07_placement_time_idspace`, `oh_05`, `oh_08` |
| 2. Discovery | Per-topic search results, find counts, scheduled lookups, final coverage, search provenance, search progress (with a re-walk flag); `02`, `03_time_to_fraction`, `11_discovery_rate` (continuous and conn runs), `12_search_bucket`, `02b`, `08`, `09`, `05`, `oh_06` |
| 3. Overhead and load | `oh_01`, `oh_03`, `oh_10`, `oh_02`, `oh_04`, `oh_07`, `oh_09`, `oh_11` and the load summary table |
| 4. Peer connections | Connection-model table (only when the run models peer slots) |
| 5. Churn | Dead-result and churn tables, `10_dead_results` (only churn runs) |
| Run health | Simnet buffer peaks and drops, or host CPU/RSS/UDP drops on real backends |
| Not in this report | Every expected figure or table the run could not produce, with the reason |

## §2 Functional correctness checks

| Plan check | Status | Where |
|---|---|---|
| Advertisements are stored correctly | ✗ | #122 |
| Advertisements expire and are renewed correctly | ✗ | #122; renewal itself is #77 |
| Caches never exceed capacity C | ✗ | #122 (data: `oh_08` samples) |
| Lookups return advertisements for the requested service | ✗ | #122 (data: result ids vs assignments) |
| Ticket and waiting-time behaviour is correct | ✗ | #122 |
| Registration state is maintained over time | ✗ | #122 |

#### Differences and gaps

- No check script exists; all six are assertions to evaluate over traces (#122).
- The plan also asks to compare cache utilisation and waiting times against the
  Python simulator. That comparison is outside this repo.

## §2 Registration and cache behaviour

| Plan plot | Figure | simnet | real | Report |
|---|---|---|---|---|
| Cache utilisation over time, compared with the Python simulator | `oh_08_cache_utilisation` | ✓ | ✓ | yes |
| Registration waiting time vs service popularity | `oh_05_wait_time_cdf`: quoted waits, and total time of every successful registration, per topic | ◐ | ◐ | yes |
| Time to first successful registration, and to complete the intended registrations across buckets, by popularity | `07_registration_latency_bar`, `07_placement_time_idspace` (first admission) | ◐ | ◐ | yes |
| Registrars storing each advertiser's ad, ads stored per registrar | `06_fanout_both_views`, `04_id_space_registrants`, `04b_id_space_registrars` | ✓ | ✓ | yes |

#### Differences and gaps

- **Popularity** is shown per topic, ranked by the Zipf draw. Binning services
  into small, medium and popular (#113) is missing; it only matters once runs
  use many topics (the plan's 300).
- **Completing all buckets:** real backends record `registrationBucketFullNs`
  and `registrationCompleteNs`, but no figure draws them and simnet does not
  record them.
- **Python simulator comparison:** not in this repo.
- **Renewal (#77)** is not in the fork, so ads held drop about half an ad
  lifetime after synchronised starts (`oh_08`); `phases.start_window` spreads
  them.
- `06_fanout_both_views` panel (b) is unreadable at scale (box collapses at 0).
- `oh_05` and `oh_08` label topics by hash where other figures use 0–4.
- On AWS, first admission took a 28.6 s median against 0.5 s on simnet
  (500-node comparison); not explained yet.

## §2 Search performance and discovery

| Plan plot | Figure | simnet | real | Report |
|---|---|---|---|---|
| Lookup latency vs service popularity | `08_lookup_latency_cdf`: time to F_lookup distinct registrants, per topic | ✓ | ✓ | yes |
| Number/fraction of available advertisers discovered vs popularity | `02_recall_reached`, `03_time_to_fraction` (time to 50/90/99% of registrants) | ✓ | ✓ | yes |
| Registrars/nodes contacted per lookup vs popularity | `09_lookup_contacts_cdf`: distinct nodes and TOPICQUERY requests | ✓ | ✓ | yes |
| Number of times each advertiser is discovered | `05_id_space_found_vs_missed` | ✓ | ✓ | yes |
| Time from successful ad placement to first discovery | `oh_06_idspace_found_time` | ◐ | ◐ | yes |

#### Differences and gaps

- **Lookup latency** exists per lookup only in `scheduled` runs. In
  `continuous` and `conn` runs it is the time the node's search took to its
  first F_lookup registrants.
- **Contacts per lookup** come from the lookups themselves: lookup-only runs
  (`scheduled`, `continuous`) never dial, so no second search runs next to
  them. In `conn` runs the dialer's search is the node's only search; its
  passes, queries, results, duplicates and filtered results are in
  `searchStats` (fork `v1.17.2-testbed.6`).
- **Placement to first discovery** counts from the later of the ad's placement
  and the search start. The plan's quantity needs a scenario where searches
  overlap registration; with a long `register_wait` the figure only shows
  discovery of ads that already exist.
- **Search re-walk:** the fork under test includes the search filter
  (datahop/go-ethereum#140). Once a long-lived search (`conn`, `continuous`)
  has returned every registrant of its topic, every later result is dropped as
  recently returned and the search re-walks its table every pass
  (datahop/go-ethereum#142). Query traffic and registrar load on those topics
  measure that loop: 184 passes per hour and 940 MB per node on the smallest
  topic of the 14.6 h 5k run. The report's *Search progress* table flags
  affected topics (more results filtered than handed out). No figure shows
  repeats per searcher yet.

## §2 Load distribution

| Plan plot | Figure | simnet | real | Report |
|---|---|---|---|---|
| Registration and lookup contacts across the service-centred ID space | `oh_11_topic_load` (requests received per registrar per topic), `oh_07_load_vs_topic_distance` | ◐ | ◐ | yes |
| Total messages/bytes sent and received per node | `oh_01_idspace_traffic` | ✓ | ✓ | yes |
| Per-node traffic by registration, renewal and lookup | `oh_03_idspace_msgtype`, `oh_10_reg_vs_lookup` | ✓ | ✓ | yes |
| Registration and lookup load vs node distance from service IDs | `oh_07_load_vs_topic_distance`: received and sent | ◐ | ◐ | yes |
| Summary statistics: median, p95, p99, max, coefficient of variation | Load summary table (from `oh_11`) | ✓ | ✓ | yes |

#### Differences and gaps

- **Service-centred ID space:** `oh_11` gives the distribution of per-topic
  load, not load placed by distance to each service ID. Per-topic counters are
  in `oh.json` (`topicLoad`), so an ID-space version needs only a figure.
- **Load vs distance:** `oh_07` uses whole-node totals, so every topic's line
  includes traffic caused by other topics, and it does not split registration
  from lookup traffic.
- `oh_09_cost_per_lookup` is flat by construction (network lookup traffic
  divided equally); per-topic cost is in `oh_11` and should replace it.
- The 500-node AWS run predates lookup-only runs without a dialer: geth's
  dialer search was most of its lookup traffic, so its load figures are not
  comparable with simnet.

## §2 Churn resilience

| Plan plot | Figure | simnet | real | Report |
|---|---|---|---|---|
| Fraction of returned ads referring to unavailable nodes vs churn rate | `10_dead_results` (per run); `churn_deadresult` (across rates) | ◐ | ✗ | per run |
| Age distribution of stale ads vs churn rate | `10_dead_results` panel (b); `churn_deadage` | ◐ | ✗ | per run |
| Lookup success rate and latency vs churn rate | `08_lookup_latency_cdf` and the scheduled-lookups table per run | ◐ | ◐ | per run |
| Successful registrations maintained per advertiser vs churn rate | — | ✗ | ✗ | no |

#### Differences and gaps

- **Churn-rate axis:** per-run figures exist; the across-rates figures come from
  `report_plots.py`, which reads simnet logs of the older steady-state churn
  sweeps, not session-churn runs or real backends.
- **Dead results** count every returned result, including repeats, and a node
  that comes back counts as live again. Real backends do not compute them yet.
- **Registrations maintained** needs periodic per-advertiser counts and
  renewal (#77).
- The rate multiplier `session_churn.scale` is ignored when a fitted churn
  model file is set (#116).

#### Planned: trace-driven churn (24 h crawl replay)

Figures for runs whose population, topics (chains) and sessions come from a
crawl trace (`cmd/crawl`), so that churn and popularity are the network's own
rather than a model. Both are drawn for the baseline and the adaptive search
distance on the same trace.

| Figure | What it shows | Why |
|---|---|---|
| `12_dead_vs_alive_results` | Of the nodes a search returns over time, the share that are alive at that moment (answering), unreachable (alive in the trace but never answering a stranger) and gone (left before the result was returned); per topic, and against the same split for plain discv5 records from the crawl (15 % answering, 71 % of the rest sharing an IP with other records) | A plain FINDNODE walk hands out ~6.5 records per node that answers a stranger; ads are placed by the registrant itself, so topic results should be mostly reachable. This puts a number on that difference and on how quickly stale ads are served after a departure |
| `13_discovery_rate_compare` | New alive registrants found per searcher per minute (`11b` style) for baseline vs adaptive on the trace, per topic, together with the queries spent per alive result | Discovery speed and cost under real churn, so the adaptive search's slower tail (seen on synthetic 5k runs) is measured where it matters |

Both need a hard departure in simnet (a departed node stops answering, #17)
and the trace loader; the alive/unreachable/gone split of a result comes
from the sessions file at the result's timestamp.

## §3 TopDisc vs legacy discv5

| Plan plot | simnet | real |
|---|---|---|
| Lookup latency, TopDisc vs Discv5, by popularity | ✗ | ✗ |
| Fraction of available peers discovered, by popularity | ✗ | ✗ |
| Nodes contacted per lookup, by popularity | ✗ | ✗ |
| Messages/bytes per lookup | ✗ | ✗ |
| Distribution of per-node lookup traffic | ✗ | ✗ |
| Total TopDisc background registration/renewal overhead | ✗ | ✗ |

#### Differences and gaps

- No comparison script for a TopDisc run against a legacy run.
  `compare_runs.py` overlays two runs but assumes both are TopDisc: the
  coordinator leaves legacy nodes out of `results[]`.
- Legacy lookups record latency and results, not contacts.
- Bytes per lookup: nodes now export per-operation counters (`ops` in
  `oh.json`), but all of a node's lookups share one operation ID, so the cost is
  per node, not per lookup.
- Registration overhead exists per run (`oh_10`); the side-by-side does not.

## §4 Partial deployment

| Plan plot | simnet | real |
|---|---|---|
| Startup to first TopDisc-capable peer via legacy Discv5, vs deployment % | ✗ | ◐ `first_capable_ms` recorded |
| Start of registration to first successful registration, vs deployment % | ✗ | ✗ |
| Fraction of available advertisers discovered, vs deployment % | ✗ | ✗ |
| Lookup latency, vs deployment % | ✗ | ✗ |
| Registrars contacted per registration and per lookup, vs deployment % | ✗ | ✗ |
| Messages/bytes per lookup, vs deployment % | ✗ | ✗ |
| Per-node load, vs deployment % | ✗ | ✗ |
| Separate results for small, medium and popular services | ✗ | ✗ |

#### Differences and gaps

- No sweep script over `population.legacy_frac`; every per-run figure exists
  for a single deployment level.
- Registrars contacted per registration: nodes export distinct nodes per
  registration operation (`ops`), but no figure uses it.
- Popularity bins: #113.

## §5 Standalone registrar overhead, §6 Admission-control security

Not testbed work: §5 needs a registrar load generator (#121); §6 uses the
Python simulator (#123, #124).

## Not in the plan

| Figure | Why it is kept |
|---|---|
| `02b_time_to_first_cdf` | Time to the first result, per topic |
| `11_discovery_rate` | New registrants per searcher per minute over time, per topic, for long-lived searches (continuous, conn): shows how fast discovery decays and when a topic runs out of new registrants |
| `12_search_bucket` | Share of TOPICQUERY requests per search-table bucket (0 = farthest from the topic), per topic or topic pool, and for adaptive searches (`search_yield_floor` > 0) the median active bucket over search time: where each topic's searches settle |
| `oh_02_idspace_peak_rate`, `oh_04_idspace_peak_msgtype` | Peak sustained rate per node, total and by type |
| `oh_09_cost_per_lookup` | To be replaced by a per-topic cost (see load) |
| `cmp_01`–`cmp_13` (`compare_runs.py`) | Same scenario on two backends: lookups, discovery, registration, cache, registrar load, traffic, load vs distance |
| Run health, peer connections tables | Separate testbed problems from protocol behaviour |

Removed from the per-run report: `01_topic_distribution` (now a table),
`03_unique_found_over_time` (replaced by `03_time_to_fraction`),
`07b_placement_mean_idspace` (mixed admissions over the whole run).

## Differences between the runs so far and the plan's workload

- **Scale and services:** the plan uses 10,000 nodes and 300 services (Zipf
  α = 1); runs so far used 500–10,000 nodes and 1 or 5 topics.
- **Protocol parameters:** the plan's K_lookup 5, K_register 3 and F_return 10
  are `search_bucket_size`, `reg_bucket_size` and `topic_nodes_limit`; runs so
  far used the fork defaults 16, 5 and 16 (#12).
- **Network:** the plan's WAN model (pair RTT 8–91 ms, 20 KB/s per node) is
  not in simnet, which uses one latency and bandwidth for every link.
- **Lookup workload:** the plan's `scheduled` lookups are available on every
  backend; most runs so far used `continuous` to find the performance limit.
- **Renewal** (#77) is not in the fork.

## Totals (§2–§4, 32 plan plots)

| | done | partial | missing |
|---|---|---|---|
| simnet | 9 | 8 | 15 |
| real backends | 9 | 6 | 17 |

Plus the six §2 correctness checks, all missing (#122).
