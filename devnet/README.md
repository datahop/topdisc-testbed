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
add `--discovery.topic`, `--vmodule=p2p/discover/topicindex/*=5` and
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
`10 30 60 75`; the hook is a placeholder until the check tool exists) and
stops at `DURATION_MIN` (default 75), skipping snapshot times beyond it. On exit, also after a failure, it takes
`kurtosis enclave dump`, `docker logs` of every container and `docker
inspect`, then removes the enclave (`KEEP_ENCLAVE=1` keeps it). The run
directory `~/workspace/datahop/runs/<date>-d79-<run>/` is outside git.

Geth RPC (8545), metrics (9001), the beacon API (4000) and discv5 are reached
from the host on the container IPs on the enclave bridge.

## Evidence notes

- Participant 0 is the bootnode and the supernode, and its kind differs per
  run (OLD in R0 and R1, NEW in R2), so it is left out of the old-node medians
  in E2.

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
