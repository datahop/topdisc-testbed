#!/usr/bin/env python3
"""Grid'5000 driver: the same verbs as deploy/aws.sh, on kadeploy'd machines
running hostagent directly (no Distem).

  deploy/g5k/g5k.py up   scenarios/x.yaml   reserve + kadeploy machines, push and start
                                            hostagent on each, write inventory.json
  deploy/g5k/g5k.py run  scenarios/x.yaml   run the scenario on the coordinator machine, wait,
                                            pull the run directory into runs/, then release (KEEP=1 keeps)
  deploy/g5k/g5k.py pull                    fetch the latest run directory from the coordinator
  deploy/g5k/g5k.py down                    release the OAR job
  deploy/g5k/g5k.py check                   my running OAR jobs on the site

Runs from a Grid'5000 frontend (no credentials needed) or from outside with
~/.python-grid5000.yaml. State (job id, node list) is kept in
deploy/g5k/state.json next to this file. G5K_CLUSTER=<cluster> overrides
testbed.g5k.cluster, and G5K_RESERVATION="YYYY-MM-DD HH:MM:SS" sets an advance
reservation, for one run (e.g. when the scenario's usual cluster is busy, or
when the job has to start in the night window) without editing a tracked
scenario file. WORKDIR=<path> (default
/tmp/topdisc) is where agents and the coordinator write; `run` refuses to start
unless the coordinator has MIN_FREE_GB (default 100) free there.

Distem (LXC-vnode-per-node, one real IP per node out of a reserved /22) was
the original design here but has no working install path on any Debian
release Grid'5000 currently deploys (its packaging targets buster/stretch,
both EOL; see git history on this file). This drives testbed.wan's own
netns+netem oversubscription (pkg/host, already used by the local and cloud/
AWS backends) directly on kadeploy'd machines instead: one hostagent per
physical machine, `vnodes_per_machine` node processes each in their own
namespace on that host's private bridge, no per-node real IP or container
needed. No subnet reservation either -- hosts route each other's private
/16s over the ordinary prod network hostagent already needs for SSH.
"""
import json, os, pathlib, socket, subprocess, sys, time

import yaml

HERE = pathlib.Path(__file__).resolve().parent
REPO = HERE.parent.parent
STATE = HERE / "state.json"
KEEP = bool(os.environ.get("KEEP"))
SKIP_BUILD = bool(os.environ.get("SKIP_BUILD"))
# Machines reserved beyond what the run needs, so one lost to kadeploy does
# not cost the reservation. None lets `up` size it from the fleet.
SPARES = int(os.environ["G5K_SPARES"]) if os.environ.get("G5K_SPARES") else None
AGENT_PORT = 9000
# Everything a run writes -- each agent's per-node working directories, and on
# the coordinator the fetched traces and the aggregates built from them -- goes
# here rather than under /root. A 25k-node run's traces are far larger than the
# root filesystem of a gros machine (31 GB), and when the coordinator fills up
# mid-collection the aggregates are lost with no way back short of rebuilding
# them from the agents. /tmp on these machines is the 375 GB data disk.
WORK = os.environ.get("WORKDIR", "/tmp/topdisc")
MIN_FREE_GB = int(os.environ.get("MIN_FREE_GB", "100"))


def sh(cmd, **kw):
    print("+", cmd, file=sys.stderr)
    return subprocess.run(cmd, shell=True, check=True, text=True, capture_output=kw.pop("capture", False), **kw)


def load_state():
    return json.loads(STATE.read_text()) if STATE.exists() else {}


def save_state(st):
    STATE.write_text(json.dumps(st, indent=1))


def scenario(path):
    cfg = yaml.safe_load(open(path))
    g5k = (cfg.get("testbed") or {}).get("g5k") or {}
    # defaults as in `testbed reference`. G5K_CLUSTER and G5K_RESERVATION
    # override the scenario (same pattern as KEEP below) for picking a different
    # cluster or start time between runs without editing a tracked scenario
    # file, e.g. when gros is busy, or when a run has to land in the night
    # window the usage policy reserves for jobs this size.
    return cfg, {
        "site": g5k.get("site", "nancy"), "cluster": os.environ.get("G5K_CLUSTER", g5k.get("cluster", "")),
        "walltime": g5k.get("walltime", "02:00:00"),
        "reservation": os.environ.get("G5K_RESERVATION", g5k.get("reservation", "")),
        "queue": g5k.get("queue", "default"), "env": g5k.get("env", "debian11-x64-base"),
        "vnodes_per_machine": int(g5k.get("vnodes_per_machine", 250)),
    }


def build():
    # The Grid'5000 frontends have no Go toolchain, so the usual way to drive a
    # run from one is to cross-compile out/ elsewhere and rsync it over.
    # SKIP_BUILD=1 then says "these binaries are the ones I meant"; it checks
    # they are all there rather than letting `up` discover a missing one after
    # the machines are already deployed.
    if SKIP_BUILD:
        missing = [b for b in ("testbed", "hostagent", "topdisc-node", "topdisc-node-legacy")
                   if not (REPO / "out" / b).exists()]
        if missing or not (REPO / "testbed").exists():
            sys.exit(f"SKIP_BUILD=1 but these are not built: {missing + ([] if (REPO / 'testbed').exists() else ['./testbed'])}")
        print(f"build: skipped, using out/ ({', '.join(b.name for b in sorted((REPO / 'out').iterdir()))})")
        return
    sh(f"cd {REPO} && CGO_ENABLED=0 go build -o testbed ./cmd/testbed")
    for bin, pkg in [("testbed", "./cmd/testbed"), ("hostagent", "./cmd/hostagent"), ("topdisc-node", "./cmd/node")]:
        sh(f"cd {REPO} && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o out/{bin} {pkg}")
    sh(f"cd {REPO}/legacy && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o ../out/topdisc-node-legacy ./cmd/node-legacy")


def wait_healthy(hosts, need, timeout=60):
    """Return the hosts whose agent answered, in the order given.

    A machine that kadeploy lost, or whose agent never came up, used to abort
    the run here. Returning the survivors lets `up` decide: a run only needs
    `need` of them, and a spare or two is cheaper than losing the night.
    """
    import urllib.request
    deadline = time.time() + timeout
    left, ok = set(hosts), set()
    while left and time.time() < deadline:
        for h in list(left):
            try:
                urllib.request.urlopen(f"http://{h}:{AGENT_PORT}/health", timeout=2).read()
                left.discard(h); ok.add(h)
            except Exception:
                pass
        if left:
            time.sleep(2)
    if left:
        print(f"hostagent never came up on {len(left)}: {', '.join(sorted(left))}", file=sys.stderr)
    if len(ok) < need:
        sys.exit(f"only {len(ok)} of {len(hosts)} machines are healthy, need {need}")
    return [h for h in hosts if h in ok]


def up(path):
    import enoslib as en
    cfg, g = scenario(path)
    build()
    fleet = json.loads(sh(f"cd {REPO} && ./testbed fleet {path}", capture=True).stdout)
    machines = max(1, fleet["g5k_machines"])
    # Reserve a couple more than the run needs. On 2026-10-05 one machine of a
    # hundred failed kadeploy, the inventory came out 250 nodes short of the
    # population, and the coordinator refused to start -- twenty minutes of a
    # night window spent on a single dead node. Spares are far cheaper.
    spares = SPARES if SPARES is not None else max(1, round(machines * 0.04))
    print(f"fleet: {fleet['regions']} -> {machines} machines x {g['vnodes_per_machine']} vnodes "
          f"on {g['site']} (reserving {machines + spares})")

    conf = en.G5kConf.from_settings(job_name="topdisc", walltime=g["walltime"], job_type=["deploy"],
                                    env_name=g["env"], queue=g["queue"],
                                    **({"reservation": g["reservation"]} if g["reservation"] else {}))
    conf = conf.add_network(id="prod", type="prod", roles=["prod"], site=g["site"])
    # primary_network is left unset: EnOSlib's Configuration.add_machine_conf()
    # auto-resolves it to the "prod"-type network added above for this site.
    # Passing the string "prod" here instead (as this used to) skips that
    # resolution and crashes later in to_dict(), which expects an object.
    if not g["cluster"]:
        sys.exit(f"g5k: testbed.g5k.cluster is required (site {g['site']}); "
                  f"EnOSlib's G5kConf needs a specific cluster, not just a site")
    conf = conf.add_machine(roles=["pnode"], nodes=machines + spares, cluster=g["cluster"])
    provider = en.G5k(conf)
    # Only the reservation itself is worth unwinding automatically. Once there
    # are machines in hand, a failure past this point is something to look at,
    # not something to answer by throwing the night away: on 2026-10-07 one
    # machine came back from kadeploy with an unwritable /root, the scp to it
    # raised, and the handler released all 52 with the night half gone.
    try:
        roles, networks = provider.init()
    except Exception:
        if not KEEP:
            print("reservation failed; releasing", file=sys.stderr)
            provider.destroy()
        raise
    try:
        deployed = [h.address for h in roles["pnode"]]
        broken = []
        for h in deployed:
            # A machine that cannot take the binaries is not one of the machines
            # this run has. Dropping it here lets the healthy count below decide
            # whether enough are left, instead of aborting on the first bad one.
            try:
                sh(f"scp -q {REPO}/out/hostagent {REPO}/out/topdisc-node {REPO}/out/topdisc-node-legacy root@{h}:/root/")
                # ulimit: one agent supervises vnodes_per_machine node processes,
                # each with its own netns, veth and log; the login default of 1024
                # is the first thing to bind as that number grows.
                sh(f"ssh root@{h} 'chmod +x /root/hostagent /root/topdisc-node /root/topdisc-node-legacy; "
                   f"ulimit -n 65536; "
                   f"nohup /root/hostagent -serve :{AGENT_PORT} -node-binary /root/topdisc-node "
                   f"-legacy-binary /root/topdisc-node-legacy -workdir {WORK}/run "
                   f">/root/hostagent.log 2>&1 </dev/null &'")
            except subprocess.CalledProcessError as e:
                print(f"{h}: cannot stage the binaries, dropping it ({e})", file=sys.stderr)
                broken.append(h)
        deployed = [h for h in deployed if h not in broken]
        # Take the machines the run needs from the ones that actually answered,
        # and spread the population over them. With spares in hand this is the
        # configured density exactly; if kadeploy lost more than the spares, it
        # is a denser run rather than a dead one -- but only up to a point,
        # since density is what the host-level limits were sized against.
        healthy = wait_healthy(deployed, machines)
        pnodes = healthy[:machines]
        pop = int(((cfg.get("scenario") or {}).get("population") or {}).get("nodes") or 0)
        per = g["vnodes_per_machine"]
        if len(pnodes) < machines or per * len(pnodes) < pop:
            per = -(-pop // len(pnodes))            # ceil
            cap = int(g["vnodes_per_machine"] * 1.25)
            if per > cap:
                sys.exit(f"{len(pnodes)} healthy machines would need {per} nodes each for a "
                         f"population of {pop}, over the {cap} ceiling "
                         f"({g['vnodes_per_machine']} configured + 25%)")
            print(f"{len(deployed) - len(healthy)} machines lost; {len(pnodes)} x {per} "
                  f"covers {pop} (configured {g['vnodes_per_machine']})", file=sys.stderr)
        coordinator = pnodes[0]
        save_state({"site": g["site"], "cluster": g["cluster"], "env": g["env"], "pnodes": pnodes,
                    "coordinator": coordinator, "scenario": path, "vnodes_per_machine": per})
        # Resolve to addresses: every host routes its peers' /16s with
        # `ip route replace ... via <peer>`, which takes an address, not the
        # name EnOSlib hands back. A single-host run never routes a peer, so
        # this only shows up once there are two.
        inventory = {"coordinator": coordinator,
                     "hosts": [{"index": i, "ip": socket.gethostbyname(h), "nodes": per}
                               for i, h in enumerate(pnodes)]}
        (HERE / "inventory.json").write_text(json.dumps(inventory, indent=1))
        sh(f"ssh root@{coordinator} 'mkdir -p {WORK}'")
        sh(f"scp -q {REPO}/out/testbed {HERE}/inventory.json {path} root@{coordinator}:{WORK}/")
        sh(f"scp -q -r {REPO}/scenarios/models root@{coordinator}:{WORK}/")
        print(f"up: {len(pnodes)} machines x {per} nodes, coordinator {coordinator}, inventory {HERE}/inventory.json")
    except Exception:
        print("up failed after the machines were reserved. They are still held: "
              "fix and re-run `up`, or release with `g5k.py down`.", file=sys.stderr)
        raise


def run(path):
    st = load_state()
    c = st["coordinator"]
    name = os.path.basename(path)
    sh(f"scp -q {path} root@{c}:{WORK}/{name}")
    free_gb = int(subprocess.run(f"ssh root@{c} 'df -BG --output=avail {WORK} | tail -1'",
                                 shell=True, text=True, capture_output=True).stdout.strip().rstrip("G") or 0)
    if free_gb < MIN_FREE_GB:
        sys.exit(f"run: coordinator has {free_gb} GB free on {WORK}, want >= {MIN_FREE_GB}. "
                 f"The traces of a 25k-node run do not fit on the root filesystem; "
                 f"set WORKDIR to a larger partition.")
    print(f"coordinator {WORK}: {free_gb} GB free")
    sh(f"ssh root@{c} 'cd {WORK} && (nohup ./testbed {name} > run.out 2>&1; echo RUN-EXIT $? >> run.out) >/dev/null 2>&1 &'")
    print("started on the coordinator; waiting")
    while True:
        out = subprocess.run(f"ssh root@{c} tail -c 4000 {WORK}/run.out", shell=True, text=True, capture_output=True).stdout
        if "RUN-EXIT" in out:
            break
        last = [l for l in out.splitlines() if l.startswith(("backend", "nodes started", "[churn]"))]
        if last:
            print(last[-1])
        time.sleep(30)
    print("\n".join(l for l in out.splitlines() if not l.startswith("PARAMS")))
    try:
        pull()
    except Exception:
        print("pull failed: deployment kept; fix, then g5k.py pull && g5k.py down", file=sys.stderr)
        raise
    if not KEEP:
        down()


def free_gb(path):
    import shutil
    return shutil.disk_usage(path).free // 1024**3


def pull():
    """Fetch the run directory in order of what cannot be rebuilt.

    At 25k nodes the traces are ~2 MB each -- 50 GB -- while the aggregates
    built from them are under 2 GB. Pulling the whole directory in one rsync
    means a transfer that runs out of room takes the aggregates down with it,
    and a Grid'5000 frontend's home quota is smaller than the traces. So the
    aggregates land first and the traces follow, compressed, only if they fit.
    """
    st = load_state()
    c = st["coordinator"]
    d = subprocess.run(f"ssh root@{c} 'ls -td {WORK}/*-2* | head -1'", shell=True, text=True, capture_output=True, check=True).stdout.strip()
    if not d:
        sys.exit("pull: no run directory on the coordinator")
    (REPO / "runs").mkdir(exist_ok=True)
    local = REPO / "runs" / os.path.basename(d)
    local.mkdir(parents=True, exist_ok=True)

    sh(f"rsync -a --info=progress2 --exclude=traces/ root@{c}:{d}/ {local}/")
    if not (local / "nodes.json").exists():
        sys.exit(f"pull: {local} has no nodes.json -- the run did not finish writing its aggregates. "
                 f"The traces are still on {c}:{d}/traces; rebuild with cmd/recollect.")
    print(f"runs/{local.name}: aggregates and hostmetrics in ({free_gb(local)} GB free here)")

    raw_mb = int(subprocess.run(f"ssh root@{c} 'du -sm {d}/traces 2>/dev/null | cut -f1'",
                                shell=True, text=True, capture_output=True).stdout.strip() or 0)
    want = max(1, raw_mb // 1024 // 4)   # gzipped JSON traces run about a fifth
    if free_gb(local) < want + 5:
        print(f"pull: traces left on the coordinator -- {raw_mb // 1024} GB raw needs about {want} GB "
              f"compressed and there are {free_gb(local)} GB free here. They are a rebuild path for the "
              f"aggregates (cmd/recollect), not an input to any figure, so the run above is complete "
              f"without them. To fetch them somewhere with room:\n"
              f"  ssh root@{c} 'tar czf - -C {d} traces' > traces.tgz", file=sys.stderr)
        return
    sh(f"ssh root@{c} 'tar czf - -C {d} traces' > {local}/traces.tgz")
    print(f"runs/{local.name}: traces.tgz in ({(local / 'traces.tgz').stat().st_size // 1024**3} GB)")


def down():
    import enoslib as en
    st = load_state()
    # env_name is required by EnOSlib whenever job_type includes "deploy",
    # even for this throwaway conf that's only used to address the existing
    # job by (job_name, site) and call destroy() -- it never reserves
    # anything of its own.
    conf = en.G5kConf.from_settings(job_name="topdisc", walltime="00:01:00", job_type=["deploy"],
                                    env_name=st.get("env", "debian11-x64-base")).add_network(
        id="prod", type="prod", roles=["prod"], site=st.get("site", "nancy"))
    conf = conf.add_machine(roles=["pnode"], nodes=1, cluster=st.get("cluster", "gros"))
    en.G5k(conf).destroy()
    if STATE.exists():
        STATE.unlink()
    check()


def check():
    st = load_state()
    site = st.get("site", "nancy")
    sh(f"oarstat -u 2>/dev/null || ssh {site}.g5k oarstat -u")


if __name__ == "__main__":
    if len(sys.argv) < 2 or sys.argv[1] not in ("up", "run", "pull", "down", "check"):
        print(__doc__)
        sys.exit(2)
    verb = sys.argv[1]
    if verb in ("up", "run"):
        if len(sys.argv) < 3:
            sys.exit(f"{verb} needs a scenario")
        globals()[verb](sys.argv[2])
    else:
        globals()[verb]()
