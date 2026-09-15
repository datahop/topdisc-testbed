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
deploy/g5k/state.json next to this file.

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
import json, os, pathlib, subprocess, sys, time

import yaml

HERE = pathlib.Path(__file__).resolve().parent
REPO = HERE.parent.parent
STATE = HERE / "state.json"
KEEP = bool(os.environ.get("KEEP"))
AGENT_PORT = 9000


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
    # defaults as in `testbed reference`
    return cfg, {
        "site": g5k.get("site", "nancy"), "cluster": g5k.get("cluster", ""),
        "walltime": g5k.get("walltime", "02:00:00"), "reservation": g5k.get("reservation", ""),
        "queue": g5k.get("queue", "default"), "env": g5k.get("env", "debian11-x64-base"),
        "vnodes_per_machine": int(g5k.get("vnodes_per_machine", 250)),
    }


def build():
    sh(f"cd {REPO} && CGO_ENABLED=0 go build -o testbed ./cmd/testbed")
    for bin, pkg in [("testbed", "./cmd/testbed"), ("hostagent", "./cmd/hostagent"), ("topdisc-node", "./cmd/node")]:
        sh(f"cd {REPO} && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o out/{bin} {pkg}")
    sh(f"cd {REPO}/legacy && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o ../out/topdisc-node-legacy ./cmd/node-legacy")


def wait_healthy(hosts, timeout=60):
    import urllib.request
    deadline = time.time() + timeout
    left = set(hosts)
    while left and time.time() < deadline:
        for h in list(left):
            try:
                urllib.request.urlopen(f"http://{h}:{AGENT_PORT}/health", timeout=2).read()
                left.discard(h)
            except Exception:
                pass
        if left:
            time.sleep(2)
    if left:
        sys.exit(f"hostagent never came up on: {', '.join(sorted(left))}")


def up(path):
    import enoslib as en
    cfg, g = scenario(path)
    build()
    fleet = json.loads(sh(f"cd {REPO} && ./testbed fleet {path}", capture=True).stdout)
    machines = max(1, fleet["g5k_machines"])
    print(f"fleet: {fleet['regions']} -> {machines} machines x {g['vnodes_per_machine']} vnodes on {g['site']}")

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
    conf = conf.add_machine(roles=["pnode"], nodes=machines, cluster=g["cluster"])
    provider = en.G5k(conf)
    try:
        roles, networks = provider.init()
        pnodes = [h.address for h in roles["pnode"]]
        coordinator = pnodes[0]
        save_state({"site": g["site"], "cluster": g["cluster"], "env": g["env"], "pnodes": pnodes, "coordinator": coordinator, "scenario": path})
        for h in pnodes:
            sh(f"scp -q {REPO}/out/hostagent {REPO}/out/topdisc-node {REPO}/out/topdisc-node-legacy root@{h}:/root/")
            sh(f"ssh root@{h} 'chmod +x /root/hostagent /root/topdisc-node /root/topdisc-node-legacy; "
               f"nohup /root/hostagent -serve :{AGENT_PORT} -node-binary /root/topdisc-node "
               f"-legacy-binary /root/topdisc-node-legacy -workdir /root/run "
               f">/root/hostagent.log 2>&1 </dev/null &'")
        wait_healthy(pnodes)
        inventory = {"coordinator": coordinator,
                     "hosts": [{"index": i, "ip": h, "nodes": g["vnodes_per_machine"]} for i, h in enumerate(pnodes)]}
        (HERE / "inventory.json").write_text(json.dumps(inventory, indent=1))
        sh(f"scp -q {REPO}/out/testbed {HERE}/inventory.json {path} root@{coordinator}:/root/")
        sh(f"scp -q -r {REPO}/scenarios/models root@{coordinator}:/root/")
        print(f"up: {len(pnodes)} machines, coordinator {coordinator}, inventory {HERE}/inventory.json")
    except Exception:
        if not KEEP:
            print("up failed; releasing", file=sys.stderr)
            provider.destroy()
        raise


def run(path):
    st = load_state()
    c = st["coordinator"]
    name = os.path.basename(path)
    sh(f"scp -q {path} root@{c}:/root/{name}")
    sh(f"ssh root@{c} 'cd /root && (nohup ./testbed {name} > run.out 2>&1; echo RUN-EXIT $? >> run.out) >/dev/null 2>&1 &'")
    print("started on the coordinator; waiting")
    while True:
        out = subprocess.run(f"ssh root@{c} tail -c 4000 /root/run.out", shell=True, text=True, capture_output=True).stdout
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


def pull():
    st = load_state()
    c = st["coordinator"]
    d = subprocess.run(f"ssh root@{c} 'ls -td /root/*-2* | head -1'", shell=True, text=True, capture_output=True, check=True).stdout.strip()
    if not d:
        sys.exit("pull: no run directory on the coordinator")
    (REPO / "runs").mkdir(exist_ok=True)
    sh(f"rsync -aq root@{c}:{d} {REPO}/runs/")
    local = REPO / "runs" / os.path.basename(d)
    if not (local / "nodes.json").exists():
        sys.exit(f"pull: {local} incomplete")
    print(f"runs/{local.name} ({len(list((local / 'traces').glob('*.json')))} traces)")


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
