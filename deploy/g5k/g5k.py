#!/usr/bin/env python3
"""Grid'5000 driver: the same verbs as deploy/aws.sh, on a Distem fleet.

  deploy/g5k/g5k.py up   scenarios/x.yaml   reserve machines + a /22, deploy, bootstrap Distem,
                                            create one vnode per node (distemctl), write inventory.json
  deploy/g5k/g5k.py run  scenarios/x.yaml   run the scenario on the coordinator machine, wait,
                                            pull the run directory into runs/, then release (KEEP=1 keeps)
  deploy/g5k/g5k.py pull                    fetch the latest run directory from the coordinator
  deploy/g5k/g5k.py down                    remove the vnodes and release the OAR job
  deploy/g5k/g5k.py check                   my running OAR jobs on the site

Runs from a Grid'5000 frontend (no credentials needed) or from outside with
~/.python-grid5000.yaml. State (job id, node list, subnet) is kept in
deploy/g5k/state.json next to this file. Untested until a Grid'5000 account
exists; the vnode layer it calls (distemctl) is unit-tested and the same on a
validation host.
"""
import json, os, pathlib, subprocess, sys, time

import yaml

HERE = pathlib.Path(__file__).resolve().parent
REPO = HERE.parent.parent
STATE = HERE / "state.json"
KEEP = bool(os.environ.get("KEEP"))


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
        "image": g5k.get("image", f"file:///home/{os.environ.get('USER','USER')}/topdisc-vnode.tar.gz"),
    }


def build():
    sh(f"cd {REPO} && CGO_ENABLED=0 go build -o testbed ./cmd/testbed && CGO_ENABLED=0 go build -o distemctl ./cmd/distemctl")
    sh(f"cd {REPO} && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o out/testbed ./cmd/testbed")


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
    conf = conf.add_network(id="vnet", type="slash_22", roles=["vnet"], site=g["site"])
    mach = dict(roles=["pnode"], nodes=machines, primary_network="prod")
    if g["cluster"]:
        mach["cluster"] = g["cluster"]
    conf = conf.add_machine(**mach)
    provider = en.G5k(conf)
    try:
        roles, networks = provider.init()
        pnodes = [h.address for h in roles["pnode"]]
        subnet = str(networks["vnet"][0].network)  # e.g. 10.144.0.0/22
        coordinator = pnodes[0]
        save_state({"site": g["site"], "pnodes": pnodes, "subnet": subnet, "coordinator": coordinator, "scenario": path})
        # Distem on the deployed machines: coordinator on the first one.
        nodefile = HERE / "nodes.txt"
        nodefile.write_text("\n".join(pnodes) + "\n")
        sh(f"distem-bootstrap -f {nodefile} --node-name {coordinator} --debian-version bookworm")
        # Push the vnode image where Distem reads it (a shared home on Grid'5000).
        img = g["image"]
        if img.startswith("file://") and not os.path.exists(img[7:]):
            sys.exit(f"vnode image {img} missing: run deploy/g5k/build-image.sh first")
        sh(f"cd {REPO} && ./distemctl -coordinator {coordinator}:4567 -scenario {path} -pnodes {','.join(pnodes)} "
           f"-subnet {subnet} -image {img} -inventory {HERE}/inventory.json")
        # The coordinator machine runs ./testbed against the vnodes.
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
    if st.get("coordinator"):
        subprocess.run(f"cd {REPO} && ./distemctl -coordinator {st['coordinator']}:4567 -down", shell=True)
    conf = en.G5kConf.from_settings(job_name="topdisc", walltime="00:01:00", job_type=["deploy"]).add_network(
        id="prod", type="prod", roles=["prod"], site=st.get("site", "nancy")).add_machine(roles=["pnode"], nodes=1, primary_network="prod")
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
