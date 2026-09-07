# topdisc-testbed

Large-scale evaluation harness for TopDisc topic discovery on discv5. Runs
thousands of real discv5 nodes in one process over a simulated network, and
turns the traces into figures.

- `cmd/simnet` — the testbed binary: workloads, connection model, churn drivers
- `configs/` — run configurations (YAML)
- `scripts/run.py` — turns a config into a run
- `figures/` — trace processing and figure generation
- `docs/TRACES.md` — every trace field and which figure it drives

## Dependencies

Two forks, both pinned in `go.mod`:

- [`datahop/simnet`](https://github.com/datahop/simnet) — in-process packet
  network. Forked from `marcopolo/simnet` for node-count scaling and a
  drop-on-full link queue; without the latter a saturated link blocks the
  router shard and discovery freezes network-wide.
- [`datahop/go-ethereum`](https://github.com/datahop/go-ethereum) — the TopDisc
  implementation, plus the instrumentation the testbed reads (per-message-type
  wire counters, registrar wait-time quotes, search provenance).

## Running

```sh
go build -o simnet ./cmd/simnet
python3 scripts/run.py configs/10k-baseline.yaml
```

`--dry-run` prints the command instead of running it. Every flag is also
settable directly; the config is only a convenience.

Figures, once a run has finished:

```sh
python3 figures/figures.py <run>/m.json --out-dir figs --label baseline
python3 figures/figures_overhead.py <run>/series.json \
    --metrics <run>/m.json --overhead <run>/oh.json --out-dir figs --label baseline
```

## Scale

10,000 nodes needs roughly 80 GB of RAM and finishes in about 25 minutes on 24
cores. Check the `[buf]` lines in the run log before trusting any timing: they
report router and link buffer occupancy, and a run that reaches 100% has
queueing delay folded into its measurements.

## The connection model

Without `-conn-model` a searcher consumes discovery results forever, so load
grows without bound and never settles. With it, each node has geth's peer slots
(`MaxPeers` 50, `DialRatio` 3, so 16 dialled and 34 accepted) and stops
searching once its outbound slots are full — which is what a real node does, and
what gives a churn-free run a steady state.

Two churn models sit on top:

- `-session-churn` gives each node a session length drawn from a measured discv5
  crawl: 42.3% outlast the run, the rest fall off geometrically from a mode at
  the resolution floor. A departing node drops every connection, stops accepting
  dials for `-session-churn-gap`, then returns and refills.
- `-disconnect-interval` / `-disconnect-frac` drop a fraction of live
  connections per tick, without any node leaving. Transient link failure.

## Mixed-binary interop

The `-vanilla-frac` workload runs part of the network on stock upstream geth to
measure incremental deployment. It needs a second copy of go-ethereum whose
module path is renamed, so it is behind a build tag:

```sh
go build -tags vanilla -o simnet-vanilla ./cmd/simnet
```

Default builds stub it out.
