# crawl — continuous discv5 crawl with session tracking

`crawl` walks the discv5 DHT continuously and pings every node it has ever
seen, so that for each node the run records when it was first seen, when it
stopped answering, and when it answered again. The chain a node belongs to
is read from its ENR. The output feeds trace-driven churn scenarios.

## Running

```
CGO_ENABLED=0 go build -o crawl ./cmd/crawl
./crawl -listen :30303 -extip <public ip> -bootnodes bootnodes.txt -out data -duration 24h
```

The host must receive UDP on the listen port from the internet. `bootnodes.txt`
holds one `enr:` per line (lines starting with `- ` or quoted are accepted, so
the consensus-layer `bootstrap_nodes.yaml` files can be pasted as they are).
The node key is kept in `<out>/key`, so a restart keeps the crawler's identity.

| flag | default | meaning |
|---|---|---|
| `-crawlers` | 16 | concurrent random-walk lookups |
| `-probe` | 2m | ping period for nodes that ever answered, also after they went down |
| `-misses` | 3 | unanswered pings in a row before a live node counts as down (and at least two probe periods since its last answer). Sessions are better rebuilt from the raw `pong`/`miss` events with `sessions.py --down-after` |
| `-new-tries` | 3 | attempts at the probe rate before a new node counts as a never-responder |
| `-probe-dead` | 30m | ping period for never-responders |
| `-workers` / `-dead-workers` | 256 / 32 | ping concurrency for the two classes; never-responders can never delay the live schedule |
| `-sweep` | 10m | period of the count events |

Load at the full discv5 population (~170k records, ~25k nodes that answer at
least once): about 100 pings/s, under 1 MB/s.

## Output

`<out>/events.jsonl`, one JSON object per line, `t` in unix milliseconds:

| `ev` | fields | meaning |
|---|---|---|
| `seen` | `id seq ip udp tcp keys eth2 eth ethNext opstack src` | a record learned for the first time |
| `enr` | same | the node's record changed (higher seq) |
| `up` | `id` | the node answered a ping after not being known alive |
| `pong` | `id rtt` | a node that ever answered answered again (rtt in ms) |
| `miss` | `id err` | a node that ever answered did not answer |
| `down` | `id misses sinceUp pongs err` | the node stopped answering (run-time rule) |
| `sweep` | `n known everUp up new dead late pings pongs deadPings deadPongs errs` | counts every `-sweep` |

Chain identifiers are taken verbatim from the record: `eth2` is the consensus
fork digest, `eth` the execution fork hash (`ethNext` the next fork block),
`opstack` the OP stack chain id. `keys` lists every key in the record and
`enr` is the full record, so anything else can be decoded later
(`devp2p enrdump`).

`late` in a sweep counts nodes on the probe schedule that are overdue by more
than one period; it should stay near zero. If it grows, raise `-workers`.

## Sessions

```
python3 cmd/crawl/sessions.py data/events.jsonl [--initial 15]
```

writes next to the event log (or in `--out`); `--down-after` (default 10 min)
is the silence that ends a session, applied to the raw `pong` events:

- `nodes.csv` — `id, chain, first_seen, ip, src, keys`
- `sessions.csv` — `id, chain, start, end, initial, index`: one row per
  period a node was answering; `end` empty when it was still up at the end;
  `initial` = 1 when the node was seen within the first `--initial` minutes;
  `index` counts a node's sessions, so `index > 0` is a rejoin
- `enr_changes.csv` — `id, t, seq, chain_before, chain`

Times are seconds since the first event. Arrival precision is one crawl
sweep (minutes); leave and rejoin precision is the probe period.

## Model for scenarios

```
python3 cmd/crawl/model.py data/crawl/2026-09-18/derived/gap30/sessions.csv --span 24 --cadence 2 \
    --names names.json --out scenarios/models/crawl-2026-09-18.json
testbed preview scenarios/crawl-5k-24h.yaml
```

`model.py` fits, per chain with at least `--min-alive` nodes (the rest folded
into `other`) and globally: the share of live nodes, the fraction present at
the start, the fraction of those that never leave, the survival of a first
session and of a session after a return (Kaplan-Meier at the probe cadence),
the lengths of absences that ended in a return, the share of departures that
never returned, and the arrival, return and permanent-leave rates. A scenario
uses it through `population.topic_model` (topics drawn by share) and
`session_churn.model` (each node churns by its topic's fit); `testbed preview`
shows what a given node count and window produce next to the crawl's rates.

## Host

Run it from a host with a public IP and no NAT in front. A home router's
connection-tracking table fills within minutes at this rate, after which it
drops new flows: the crawler then sees a fifth of the live network and
records spurious leaves. `cmd/pingset` pings a list of ENRs from a fresh
endpoint and is the check for that condition (nodes that answer the crawler
but not a fresh endpoint mean the path, not the nodes, is failing).

Published crawls and how to fetch them: `data/crawl/README.md`.
