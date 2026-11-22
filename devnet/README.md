# Compatibility devnet

A post-Merge devnet on one host that mixes TopDisc geth (NEW, the
`datahop/go-ethereum` `topdisc` branch with `--discovery.topic`) and stock geth
(OLD), each EL paired with a stock Lighthouse beacon node and validator client,
to show that the two versions share discv5 tables and eth peering and that the
old nodes are unaffected. It runs the Kurtosis
[ethereum-package](https://github.com/ethpandaops/ethereum-package) from a
pinned, unpatched local checkout on the host's Docker.

## Pins

All pins are in `lib.sh`.

| what | pin |
|---|---|
| NEW geth | `topdisc-geth:3b8bef9e31d8`, fork commit `3b8bef9e3` (v1.17.10 + TopDisc), built by `build-image.sh` |
| OLD geth | `ethereum/client-go:v1.17.10` |
| CL and VC | `sigp/lighthouse:v8.3.3` |
| ethereum-package | `d3014d6d23d683362025b9e23a0d69de49e77cca` (2026-11-16) in `~/workspace/datahop/ethereum-package` |
| Kurtosis | CLI and engine 1.23.0 |
| stock devp2p | `github.com/ethereum/go-ethereum/cmd/devp2p@v1.17.10` |

Both geth builds report `Geth/v1.17.10-stable-<commit8>`, so nodes are told
apart by the ENR `td` key and node ID (`inventory.json`); NEW nodes also run
with `--identity=topdisc`, which puts `topdisc` into their `admin_peers` name.

## Host preparation (once)

Docker is installed with `docker.service` and `docker.socket` masked. The
user unmasks and starts it (sudo, not enabled at boot):

```sh
sudo systemctl unmask docker.service docker.socket
sudo systemctl start docker
```

The Kurtosis CLI comes from the release tarball
(`kurtosis-tech/kurtosis-cli-release-artifacts`, checksum checked) into
`~/.local/bin`; then `kurtosis analytics disable` and `kurtosis engine start`.
Docker 29.10 worked without a `DOCKER_MIN_API_VERSION` override. When the
project ends, `sudo systemctl stop docker.socket docker.service && sudo
systemctl mask docker.service docker.socket` leaves the host as it was.

## Build

```sh
devnet/build-image.sh    # topdisc-geth:<sha12> from ~/workspace/datahop/geth-d79 at NEW_COMMIT
devnet/build-tools.sh    # bin/devp2p-v1.17.10, bin/devp2p-topdisc, bin/crawl
```

`build-image.sh` and `build-tools.sh` refuse to build unless the fork worktree
is at `NEW_COMMIT` with no changes or untracked files. `build-image.sh` builds with `go run build/ci.go install` on the host, wraps the
binary in `ubuntu:22.04` and checks that `geth version` reports `1.17.10-stable` and the commit and
`geth --help` lists `--discovery.topic`. The stock devp2p builds offline from
the Go module cache.

## Parameters

```sh
devnet/gen-params.sh <run> <n_new> <n_old> <new|old> [old image]
```

writes `params/<run>.yaml`. The checked-in files:

| file | run | participants |
|---|---|---|
| `params/smoke.yaml` | M2 smoke | 2 new, 2 old, participant 0 OLD |
| `params/control.yaml` | R0 | 40 old |
| `params/mixed-oldboot.yaml` | R1 | 20 new, 20 old, participant 0 OLD |
| `params/mixed-newboot.yaml` | R2 | 20 new, 20 old, participant 0 NEW |

The package starts participant 0 first with no bootnodes and gives every other
EL (and CL) only participant 0's ENR, so participant 0 is the one bootnode.
Without `--bootnodes=` geth falls back to the mainnet discv5 bootnodes, so
participant 0 gets an explicit empty list. Every EL logs at debug; NEW nodes
add `--discovery.topic`, `--log.vmodule=p2p/discover/topicindex/*=5` and
`--identity=topdisc`. Each participant has 32 validators. Fulu is at genesis,
and the package then requires one supernode or at least one node with 128 or
more validators, so participant 0's beacon node is a supernode in every run.
No additional services.

## Running

```sh
setsid nohup devnet/run.sh mixed-oldboot </dev/null >/dev/null 2>&1 & disown
tail -f ~/workspace/datahop/runs/<date>-d79-mixed-oldboot/run.log
```

`run.sh <run>` checks the package checkout is clean and at the pin, runs
`kurtosis run --enclave d79-<run>`, writes `inventory.json`
(`[{name, kind, ip, id, enr, td, image}]`, one entry per EL; kind from the
image, which must agree with `td`), waits for genesis and the first block,
calls the snapshot hook at `SNAP_MIN` minutes after genesis (default
`10 30 60 75`; the hook only creates `snapshots/t<m>m/` so far) and
stops at `DURATION_MIN` (default 75), skipping snapshot times beyond it. On exit, also after a failure, it takes
`kurtosis enclave dump`, `docker logs` of every container and `docker
inspect`, then removes the enclave (`KEEP_ENCLAVE=1` keeps it). The run
directory `~/workspace/datahop/runs/<date>-d79-<run>/` is outside git.

Geth RPC (8545), metrics (9001), the beacon API (4000) and discv5 are reached
from the host on the container IPs on the enclave bridge.

## Measured load (M2 smoke, 2 new + 2 old)

From `runs/2026-11-18-d79-smoke/SMOKE.md`, per pair after finality:

| container | RSS | CPU |
|---|---:|---:|
| geth | 82–89 MiB | < 1 % |
| Lighthouse bn | 340–365 MiB | 1–4 % (up to 11 % before finality) |
| Lighthouse vc | 36 MiB | 1–3 % |

About 0.47 GiB and 0.05–0.15 core per pair; geth at debug writes about
11 KB/min. Projected to 40 pairs:

| | 40 pairs | gate |
|---|---:|---:|
| RAM | ≈ 19 GiB | ≤ 100 GB |
| CPU | ≈ 2–6 cores | ≤ 16 cores |
| geth logs per 75-min run | ≈ 33 MB | ≤ 2 GB |
| host neighbour entries | ≈ 125 per bridge | ≪ 65,536 |

## d79check

`cmd/d79check` is a separate Go module (`devnet/go.mod`, the fork at the NEW
commit replacing `github.com/ethereum/go-ethereum`, used to sign transactions).
It talks to the containers over the enclave bridge.

```sh
(cd devnet && go build -o bin/d79check ./cmd/d79check)
R=~/workspace/datahop/runs/<date>-d79-<run>
devnet/bin/d79check inventory -run $R
devnet/bin/d79check peers   -run $R -out $R/snapshots/t30m
devnet/bin/d79check metrics -run $R -out $R/snapshots/t30m
devnet/bin/d79check heads   -run $R          # 75 min by default (-duration)
devnet/bin/d79check txs     -run $R
devnet/bin/d79check tables      -run $R -out $R/snapshots/t30m
devnet/bin/d79check crawl-reach -run $R -out $R/snapshots/t30m
devnet/bin/d79check topic       -run $R -out $R/snapshots/t30m
devnet/bin/d79check logs    -run $R          # after the dump, reads $R/logs/*.log
devnet/bin/d79check report  -run $R [-control <R0 run dir>]
```

`-run` holds `inventory.json`, `genesis.txt` and `logs/`; `-out` (default
`-run`) receives the JSON. The enclave name for docker (`heads` finds the
beacon nodes, `metrics` reads `docker stats`) comes from the run directory
name, or `-enclave`.

| command | writes | checks |
|---|---|---|
| `inventory` | `inventory-check.json` | kind vs `td`, unique names, IDs and IPs, participant 0 first |
| `peers` | `peers.json` | `admin_peers` per EL, peers by kind and direction (kind from the inventory by node ID, name `Geth/topdisc/…` as cross-check); E3(a): every OLD node has a NEW peer and vice versa |
| `heads` | `heads.jsonl`, `heads.json` | every 12 s for `-duration` (default 75 min, a full M4 run; start it at genesis): EL latest and finalized (number, hash), `eth_syncing`, CL head slot and finalized epoch; E3 heads |
| `txs` | `txs.json` | 20 transfers (`-n`) from prefunded account 20 via one OLD and one NEW EL (not participant 0 when possible); per node the time from send to the first poll (every `-poll`, default 100 ms, watchers started before the first send) that shows the tx pending; with the default the figure is the poll period, an upper bound on propagation, and the inclusion block |
| `metrics` | `metrics.json` | DV7 series from `/debug/metrics` (procload, goroutines, `p2p/*` and `discover/*` traffic, fork topic meters and `discover/regtopic/*` when present, `discover/v5/bad`), container memory from `docker stats` |
| `logs` | `logs.json` | per EL: `Bad discv5 packet`, `Removing p2p peer` by removed peer kind and, for inventory peers, by peer name (`removed_by_peer`), WARN/ERROR/CRIT message templates, reorgs (all and after `-warmup`, 10 min after genesis), topic registration and query lines; `Topic registration *` lines whose `id=` belongs to a host probe in `<run>/probes.json` (see Host probes; no file, nothing ignored) are not counted, and the number ignored is printed and kept per node |
| `tables` | `tables.json`, `tables-crawl/` | E1(a,b): `bin/crawl -sample-full -no-td -sample 2m -duration 6m` from 31400, seeded with every ENR, then `tools/crawl_tables.py`; each node's live table entries as old, new or other (not in the inventory), and its NEW count against the 99 % binomial band, p = N_new/(N−1) for an OLD node and (N_new−1)/(N−1) for a NEW one |
| `crawl-reach` | `crawl-reach.json`, `crawl-reach/` | E1(c): stock `bin/devp2p-v1.17.10 discv5 crawl` (`cmd/devp2p` of upstream go-ethereum v1.17.10, built by `build-tools.sh`) twice in parallel for 5 min, from 31410 seeded with the first OLD ENR and from 31411 with the first NEW ENR; nodes reached and time to full reach from `firstResponse` |
| `topic` | `topic.json`, `topic-<seed>.log` | E4(a,b): a fork discv5 node on 31420 runs `TopicSearch` for `eth.DiscoveryTopic(chain id, genesis hash)` (from `eth_chainId` and block 0 of the first EL) for 10 min, seeded with the first NEW node, then the first OLD node (`-seed new\|old\|both\|enr:…`), one key per seed; refuses to run earlier than 20 min after genesis; a later run in the same dir adds its seeds |
| `badpacket-control` | `badpacket-control.json` | E2 positive control: from 31430, a random packet to an OLD node (first OLD, or `-target`), then a handshake carrying a REGTOPIC; counts that node's `Bad discv5 packet` lines from the probe in `docker logs`, pass at exactly one with `unknown packet type 7`; refuses `d79-control` and `d79-mixed-*` |
| `report` | `checks.json`, `summary.md` | merges the run dir and `snapshots/*/`, prints the E table; re-judges E3c from `heads.jsonl` when present (else uses `heads.json`); `-out` writes elsewhere, so a saved run can be re-reported without touching it |

Thresholds in `report`. A check passes only on complete data: a node, log,
metric or snapshot that is absent, unreadable or not comparable makes it
`missing` (with the gap in the detail), unless a violation already makes it
FAIL. `info` and `n/a` rows do not count towards the parent's status.

| check | pass when |
|---|---|
| E1 | `tables.json` reports PASS at every labelled snapshot `t<m>m` with m ≥ 30 (each needs a PASS or FAIL; earlier or unlabelled ones are shown as n/a, an n/a or absent one from t30m on is missing), and `crawl-reach.json` reports PASS |
| E1a | every inventory node's table was read; every OLD table holds ≥ 1 live NEW entry and every NEW table ≥ 1 live OLD entry; `tables.json` is judged on E1a alone |
| E1b | `info`: per node the NEW count against the 99 % band, nodes outside listed; `missing` when a table is unread or has no live OLD or NEW entry |
| E1c | each crawl reaches every inventory node within 5 min; a crawl that fails to run is `missing` |
| E4a, E4b | seeded with a NEW (E4a) or OLD (E4b) node: ≥ N_new−1 NEW nodes within 3 min, all within 10 min, no OLD node in the 10 min; a seed not run is `missing` |
| E2a, E4e | every OLD node has a readable log with 0 `Bad discv5 packet` lines, and a `discover/v5/bad` reading of 0 in every `metrics.json` of the run |
| E2 | the control (`-control`) has no NEW nodes and the same node count as the run, else E2 is `missing` naming both counts |
| E2b | the control has no NEW nodes; every OLD node's log (run and control) is readable; the run's OLD-node WARN/ERROR/CRIT templates ⊆ the control's (the control's participant 0 counts only when the run's participant 0 is OLD) |
| E2c | `info` only, never PASS or FAIL: every removal comes from geth's random eth-layer peer dropper, which never sees discovery. (A/B in a single-kind run) per OLD node except participant 0, with DEBUG lines in its log: removals of NEW peers per NEW node vs 2 × removals of OLD peers per other OLD node, nodes above 2× listed; a node with fewer than 5 removals is listed but not compared; an unreadable log, no DEBUG lines or no `removed_by_peer` (split) is `missing` |
| E2d | the control has no NEW nodes; at every location with a `metrics.json` in the run or the control, both have one, both ages are within 60 s of the nominal `t<m>m` (or of each other for the run dir), and every OLD node except participant 0 has every series; then the medians of `system/cpu/procload/total`, memory, `p2p/ingress\|egress`, `discover/ingress\|egress` and goroutines are within ±25 % of the control's |
| E3a | (A/B in a single-kind run) every snapshot dir has a `peers.json` (else `missing`); at every snapshot every EL answered `admin_peers`, every OLD node has ≥ 1 NEW peer and every NEW node ≥ 1 OLD peer |
| E3b | every tx shows pending (no `blockNumber`) on every other EL in a response completed within 10 s of its send, and is mined ≤ 2 blocks after the head at send; the sends are a burst, so only the maximum first poll hit is reported, as an upper bound |
| E3c | every sample has one EL and one CL per inventory entry, and every node answered every assessed field at least once; per sample ≤ 2 distinct EL heads; one hash per block number; `eth_syncing` false from +600 s (missing without a sample there); finality (below); query errors at most max(1, 1 % of the samples) per node (listed as `info`), more fail |
| E3c finality | judged from the first sample in which a CL reports finalized epoch ≥ 1 (before it, geth's "finalized block not found" means not finalized yet); no such sample by the end of the run fails ("never finalized"); after it, every CL's finalized epoch is monotone and ≥ the epoch of the CL's own head slot − 3, every CL's head slot is at most 32 slots (one epoch) behind the wall-clock slot (elapsed time since the beacon genesis / 12 s), so a stalled chain fails, and every EL's finalized block is monotone, unique per number and never lost (each EL gets one sample of grace after the first finality before "not found" counts as an error); `heads.json` records the first finalization and the syncing window |
| E3d | every EL has a readable log with 0 `Chain reorg detected` after warm-up |
| E4c | every NEW node has the four topic meters and `discover/regtopic/*` counters: `REGCONFIRMATION`, `TOPICNODES`, `REGTOPIC`, `TOPICQUERY` ingress > 0, `discover/regtopic/*` = 0 |
| E4d | every NEW node has a readable log with ≥ 1 `Topic registration successful`, 0 failed, ≥ 1 `Topic query answered` with n > 0 |

E2 shows "needs control" without `-control`. A single-kind run (the control)
is split by inventory position: A = even 0-based positions (participant 0 in
A, the extra node for an odd count), B = odd ones. A plays the OLD role and B
the NEW role, so participant 0 is left out of E2c and E2d as in R1. E1a/E1b
(from the entry IDs in `tables.json`), E2c (from `removed_by_peer`; an older
`logs.json` without it makes E2c `missing`) and E3a (from the peer IDs in
`peers.json`) judge A against B; E2a, E2b, E2d, E3b, the saved name-mismatch
list and every collection command use the real kind; E1c, E3c and E3d have
no kind. `checks.json` (`split`) and `summary.md` name both groups. E4 stays
n/a. E1 and E4 show "needs M3b" until the M3b files
(`tables.json`, `crawl-reach.json`, `topic.json`, in the run dir or a snapshot
dir) exist. Each holds its data and a top-level `check` object
`{id, desc, status, detail, sub}` with status PASS, FAIL, missing, n/a or
info; a file without one counts as missing. A crawler output that already
exists stops `tables` or `crawl-reach`: use a fresh `-out` per snapshot.

### Host probes

`tables`, `crawl-reach`, `topic` and `badpacket-control` run host-side discv5
nodes on the gateway of the enclave's docker network `kt-<enclave>` (`-bridge`
must name it), UDP 31400–31499 only. They run only in snapshot windows, use
fixed keys (one per role), advertise no TCP port, and are excluded from every
count (kind `other`). Their records have no `td` key, and no packet leaves
before it is gone (the `d79check` probes and `cmd/crawl -no-td` hold writes
until then; stock devp2p never sets it), so NEW nodes neither register ads
with them nor send them topic queries.

Each probe records itself in `<run>/probes.json`, `[{name, id, started,
stopped}]` with the full node ID: one entry per ID, never removed. A start
adds the entry or, for a known ID, keeps its first `started` and clears
`stopped` while the probe runs; the stop sets `stopped`. The file is updated
under `probes.json.lock` through `probes.json.tmp` and a rename; an empty or
unparsable file is an error. `logs` ignores per-peer lines whose `id=` (the
first 8 bytes, as geth logs it) is a prefix of a probe ID.

ufw INPUT may drop the containers' unsolicited PINGs to a probe; replies to
the probe's own requests pass. Probes therefore drop out of the containers'
tables between windows, which is not table churn. `badpacket-control` leaves
one `Bad discv5 packet` line in the target's log, which is why it refuses the
enclaves judged on E2.


## Evidence notes

- Participant 0 is the bootnode and the supernode, and its kind differs per
  run (OLD in R0 and R1, NEW in R2), so it is left out of the old-node medians
  and removal rates in E2.
- Finality is not tied to elapsed time: plan DV7/A7 expected finality from
  about 13 min, but the smoke run was still at finalized epoch 0 at +25 min
  and reached epoch 5 at +47 min.
- `system/cpu/procload` is an instantaneous whole-percent gauge (0–3 on an
  idle geth), so E2d compares the cumulative `system/cpu/procload/total`;
  `metrics.json` keeps both.
- `Topic query answered` logs n after the search filter, which drops the
  searcher's own ad and holds results back until 10 registrars replied with
  ads or 5 s passed. In the 2+2 smoke the other NEW node holds only the
  searcher's own ad, so n is always 0 (0 of 117 on el-2) and E4d fails there;
  it is meant for the 20/20 runs.

## Port and load policy

- The devnet never binds or publishes host UDP 9000, 9001, 30303 or TCP 8545,
  5052 (reserved for the Hoodi node). Kurtosis publishes TCP only, on random
  host ports (to be verified in M2). After `kurtosis run` and every minute
  after, `run.sh` aborts if an enclave container publishes a forbidden host
  port, and warns if a host listener on one appears that was not there before
  the run (another process, not the enclave).
- One run at a time, never alongside a testbed run. Other agents on the host
  build with `-p 4` while a run is live.
- Builds use `GOFLAGS=-p=16` and `nice -n 19`.
