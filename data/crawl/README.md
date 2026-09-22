# Crawl datasets

Traces from `cmd/crawl`: every discv5 record seen over a day, with its chain,
and every answer and miss of the nodes that answered at least once, so
sessions (arrival, leave, rejoin) can be rebuilt at any threshold. The raw
event logs live in S3; `cmd/crawl/fetch.sh <date>` downloads a crawl, checks
it and unpacks it here.

```
cmd/crawl/fetch.sh 2026-09-18            # -> data/crawl/2026-09-18/
python3 cmd/crawl/sessions.py data/crawl/2026-09-18/data/events.jsonl --down-after 30
```

## 2026-09-18

24 h, 2026-09-18 09:29 → 2026-09-19 09:29 UTC, from an AWS t3.small in
us-east-1 (public IP, no NAT). 16 concurrent random walks; nodes that ever
answered pinged every 2 min (also after going down), new nodes given 3 tries
at that rate, never-responders every 30 min. Bootnodes: the execution-layer
discv5 list plus the consensus lists of mainnet, hoodi, holesky and sepolia.

| file | size | content |
|---|---:|---|
| `events.jsonl.gz` | 966 MB (2.40 GB raw, 31.9M events) | the event log |
| `derived/gap10/`, `derived/gap30/` | ~10 MB gz | `nodes.csv`, `sessions.csv`, `enr_changes.csv`, `summary.txt` for a 10-min and a 30-min gap |
| `crawl.log`, `bootnodes.txt`, `SHA256SUMS` | | run log, inputs, checksums |

S3 prefix: `s3://datahop-testbed-data/crawl/2026-09-18/` (public read).

### What is in it

- 29,184 nodes answered at least once; ~25.4k answering at any moment. Every
  count, share and rate in the dataset's derived files and in the model is
  over these nodes. (The crawl also saw 200,653 records that never answered:
  NATed nodes, stale records and rotated identities; 71 % of them share an
  IP with another record. They are kept in `nodes.csv` for the reachability
  comparison only.)
- 455 chains with at least one answering node: 5 with ≥1000 nodes (68 % of
  the nodes), 22 with ≥100 (88 %), 110 with ≥10 (97 %); the remaining 345
  chains hold 869 nodes.
- A node is its discv5 ID. 19 % of the permanent leaves (673 of 3,513) are
  followed within 2 h by a new ID answering at the same IP:port (median 23
  min): restarts with a rotated key, counted as a leave plus an arrival, as
  the protocol sees them. `sessions.py --merge-endpoint` chains them into one
  node for machine-level sessions.
- Population is stationary: 25,076 answering nodes at the start, 25,671 at
  the end; 94 % of the initial nodes still up after 24 h; joins 0.40 %/h,
  permanent leaves 0.50 %/h of the live set (30-min gap).
- Short outages ("flicker": a node silent for the gap, then back with the
  same identity) are the larger churn term and differ by topic: consensus
  mainnet 1.5 %/h, small consensus chains 3.5–4 %/h, execution mainnet
  0.5 %/h, other execution chains ≈ 0. With a 10-min gap these numbers
  roughly double, so the threshold is a parameter of any replay.

Largest topics (answering nodes; chain from the ENR):

| chain | alive | note |
|---|---:|---|
| eth2 `8c9f62fe` | 8456 | consensus mainnet, current fork |
| eth `07c9462e` | 6295 | execution mainnet |
| none | 2207 | records without a chain key |
| eth `4be0e445` | 1787 | execution chain, no churn |
| eth `22d523b2` | 1073 | execution chain |
| eth2 `c6ecb76c` | 829 | hoodi |
| op 8453 | 807 | Base |
| eth2 `74d01459` | 536 | sepolia |
| eth2 `845ee52a` | 445 | consensus, fork family `…036c` |
| eth2 `3237dab6` | 425 | gnosis |
| eth2 `ad532ceb`, `6a95a1a9` | 92, 52 | mainnet nodes still on Electra / Deneb |

### Caveats

- Timeouts on known-alive nodes rose from 1.5 % to ~8 % over the day and
  are weakly clustered; sessions therefore need a gap of several probe
  periods (10 min or more), which the raw log allows.
- Arrival precision is a crawl sweep (minutes); the first 15 min mark the
  initially-present set. Late discovery of nodes that were up all along
  shows as apparent growth on topics with no churn.
- The crawler was reachable inbound (public IP), so nodes could revalidate
  it; a run behind NAT under-reports the live network by ~5× (measured on
  a home router: 4.6k vs 24k answering nodes).
