#!/usr/bin/env python3
"""Run a testbed config, or document the config format.

    run.py <config.yaml>            run it; traces and a resolved run.yaml go
                                    into a timestamped directory
    run.py <config.yaml> --dry-run  print the command instead
    run.py --reference              print a config with every parameter, its
                                    default and its meaning

A config is the complete description of a test. Every flag the binary accepts
has a documented parameter here, so run.yaml written next to a run's traces is
enough to reproduce it: nothing about the machine or the checkout is recorded,
because those are given by where you run it and what you check out.
"""
import argparse, shlex, subprocess, sys, time
from pathlib import Path

try:
    import yaml
except ImportError:
    sys.exit("needs PyYAML: pip install pyyaml")

# (yaml path, flag, type, default, doc). Order is the order of the reference
# document. Types: int, float, bool, str, dur. Trace files are relative to the
# run directory.
SCHEMA = [
  ("population", None, None, None, "who is in the network"),
  ("population.nodes",          "nodes",          "int",   5,     "discv5 nodes to spawn"),
  ("population.topics",         "topics",         "int",   1,     "distinct topics; >1 assigns one per node by Zipf"),
  ("population.all_register",   "all-register",   "bool",  False, "one shared topic that every node registers and searches"),
  ("population.register_frac",  "register-frac",  "float", 0.5,   "single-topic mode: fraction that register, the rest search"),
  ("population.zipf_s",         "zipf-s",         "float", 1.07,  "Zipf skew for topic assignment when topics > 1"),
  ("population.common_topic",   "common-topic",   "bool",  False, "topics > 1: everyone also registers and searches topic 0"),
  ("population.seed",           "seed",           "int",   0,     "RNG seed for every random draw; 0 = time"),
  ("population.legacy_frac",    "legacy-frac",    "float", 0.0,   "fraction of nodes without the topic-discovery ENR flag"),
  ("population.vanilla_frac",   "vanilla-frac",   "float", 0.0,   "fraction running stock upstream geth (needs -tags vanilla)"),

  ("network", None, None, None, "the simulated links and router"),
  ("network.latency_ms",        "latency",        "int",   30,    "per-pair one-way latency, ms"),
  ("network.bandwidth_mibps",   "bandwidth-mibps","int",   100,   "per-direction link bandwidth"),
  ("network.link_buf",          "link-buf",       "int",   0,     "link input queue depth; 0 = simnet default 1024"),
  ("network.link_no_aqm",       "link-no-aqm",    "bool",  False, "skip fq_codel and rate limiting on links"),
  ("network.router_buf",        "router-buf",     "int",   0,     "router per-shard queue; 0 = simnet default 8192"),
  ("network.router_shards",     "router-shards",  "int",   0,     "router shards; 0 = simnet default 16"),

  ("phases", None, None, None, "how the run is paced"),
  ("phases.spawn_delay",        "spawn-delay",    "dur",   "0s",  "gap between spawning consecutive nodes"),
  ("phases.max_bootnodes",      "max-bootnodes",  "int",   20,    "bootnodes each new node contacts"),
  ("phases.bootstrap_wait",     "bootstrap-wait", "dur",   "3s",  "after spawning, before registrations start"),
  ("phases.register_stagger",   "register-stagger","dur",  "0s",  "gap between consecutive nodes starting to register"),
  ("phases.register_wait",      "register-wait",  "dur",   "5s",  "after the last node starts registering, before searches start"),
  ("phases.search_stagger",     "search-stagger", "dur",   "0s",  "gap between consecutive searchers starting"),
  ("phases.search_timeout",     "search-timeout", "dur",   "30s", "length of the search phase"),
  ("phases.refresh_interval",   "refresh-interval","dur",  "0s",  "discv5 table refresh; 0 = default 30m"),

  ("topic", None, None, None, "protocol parameters"),
  ("topic.ad_lifetime",         "ad-lifetime",    "dur",   "0s",  "ad lifetime; 0 = default 15m. Also sets reg_attempt_timeout"),
  ("topic.ad_cache_size",       "ad-cache-size",  "int",   0,     "ads a registrar holds; 0 = default 5000"),
  ("topic.reg_attempt_timeout", "reg-attempt-timeout","dur","0s", "give up on a registrar after this; 0 = 1.5 x ad_lifetime"),
  ("topic.search_bucket_size",  "search-bucket-size","int",0,     "search table entries per distance bucket; 0 = spec default 16"),
  ("topic.topic_nodes_limit",   "topic-nodes-limit","int", 0,     "topic nodes in a TOPICQUERY reply; 0 = default 16"),
  ("topic.aux_nodes_limit",     "aux-nodes-limit",  "int", 0,     "closest-to-topic nodes attached to TOPICQUERY and REGTOPIC replies; 0 = default 8"),
  ("topic.nodes_per_source_bucket","nodes-per-source-bucket","int",0,"cap per source per bucket; 0 = default 1 (inert on topdisc)"),
  ("topic.remove_on_expiry",    "remove-on-expiry","bool", False, "drop ads at expiry instead of renewing (inert on topdisc)"),
  ("topic.reg_probe_period",    "reg-probe-period","dur",  "500ms","how often the harness polls for registration admission"),

  ("search", None, None, None, "how searchers consume results"),
  ("search.model",              "search-model",   "str",   "conn","conn: search only while outbound slots are empty; continuous: lookups back to back"),
  ("search.request_delay",      "search-request-delay","dur","0s","continuous: pause between lookups"),
  ("search.request_timeout",    "search-request-timeout","dur","0s","continuous: give up on a lookup after this; 0 = only target_count ends it"),
  ("search.pause_max",          "search-pause-max","dur",  "0s",  "random sleep up to this between results; 0 with conn_model"),
  ("search.pause_novel_only",   "search-pause-novel-only","bool",False,"only pause on registrants not seen before"),
  ("search.target_count",       "search-target-count","int",0,    "conn: stop a searcher after this many distinct registrants; continuous: end each lookup at this many. 0 = never"),

  ("conn_model", None, None, None, "geth peer slots: a node stops searching once its outbound slots are full"),
  ("conn_model.enabled",        "conn-model",     "bool",  False, ""),
  ("conn_model.max_peers",      "conn-max-peers", "int",   50,    "total slots per node"),
  ("conn_model.dial_ratio",     "conn-dial-ratio","int",   3,     "1/N of slots are outbound"),
  ("conn_model.redial_wait",    "conn-redial-wait","dur",  "35s", "cooldown before re-dialing the same node"),

  ("session_churn", None, None, None, "nodes leave and return with session lengths from a measured discv5 crawl"),
  ("session_churn.enabled",     "session-churn",  "bool",  False, "42.3% stay all run; the rest fall off geometrically from a short mode"),
  ("session_churn.gap",         "session-churn-gap","dur", "30s", "how long a departed node is unreachable"),

  ("disconnect", None, None, None, "link failure without any node leaving"),
  ("disconnect.interval",       "disconnect-interval","dur","0s", "drop a fraction of live connections this often; 0 = off"),
  ("disconnect.frac",           "disconnect-frac","float", 0.01,  "fraction dropped per interval"),

  ("churn", None, None, None, "node kill/join churn (the older model)"),
  ("churn.interval",            "churn-interval", "dur",   "0s",  "churn round period; 0 = off"),
  ("churn.frac",                "churn-frac",     "float", 0.1,   "fraction of nodes acted on per round"),
  ("churn.mode",                "churn-mode",     "str",   "steadystate", "steadystate (50/50 leave/join) or killonly"),

  ("traces", None, None, None, "what to record; files are written into the run directory"),
  ("traces.metrics",            "metrics-out",    "str",   "",    "search and registration record (JSON)"),
  ("traces.overhead",           "overhead-out",   "str",   "",    "per-node traffic totals by message type (JSON)"),
  ("traces.overhead_series",    "overhead-series-out","str","",   "traffic and ad-cache samples over time (JSON)"),
  ("traces.overhead_series_period","overhead-series-period","dur","30s","sampling period for overhead_series"),
  ("traces.reach",              "reach-out",      "str",   "",    "per-searcher registrar reach sets (JSON)"),
  ("traces.snapshot_dir",       "snapshot-dir",   "str",   "",    "periodic find-count snapshots"),
  ("traces.checkpoint_interval","checkpoint-interval","dur","0s", "print coverage this often during search; 0 = off"),

  ("safety", None, None, None, ""),
  ("safety.abort_on_drop",      "abort-on-drop",  "bool",  True,  "exit on the first dropped packet: drops bias every timing"),
]

PARAMS = [e for e in SCHEMA if e[1]]
BY_FLAG = {e[1]: e for e in PARAMS}


def get(cfg, path):
    cur = cfg
    for k in path.split("."):
        if not isinstance(cur, dict) or k not in cur:
            return None
        cur = cur[k]
    return cur


def put(cfg, path, v):
    ks = path.split(".")
    cur = cfg
    for k in ks[:-1]:
        cur = cur.setdefault(k, {})
    cur[ks[-1]] = v


def flags(cfg, outdir):
    args = []
    for path, flag, typ, default, _ in PARAMS:
        v = get(cfg, path)
        if v is None:
            continue
        if typ == "bool":
            if v:
                args.append(f"-{flag}")
            continue
        if path.startswith("traces.") and typ == "str" and v:
            v = str(Path(outdir) / v)
        args += [f"-{flag}", str(v)]
    return args


def parse(typ, s):
    if typ == "int":
        return int(s)
    if typ == "float":
        return float(s)
    if typ == "bool":
        return s == "true"
    return s


def resolved(cfg, log):
    """The config with every parameter filled in from what the binary reported
    it actually ran with, so the snapshot does not depend on defaults."""
    out = {"name": cfg.get("name")}
    for ln in log.read_text().splitlines():
        if not ln.startswith("PARAMS: "):
            continue
        for kv in ln[8:].split():
            if "=" not in kv:
                continue
            flag, val = kv.split("=", 1)
            e = BY_FLAG.get(flag)
            if not e:
                continue
            if e[0].startswith("traces.") and e[2] == "str":
                val = Path(val).name if val else ""
            put(out, e[0], parse(e[2], val))
        return out
    return dict(cfg)


def reference():
    lines = ["# Every parameter, with its default and meaning. Copy and edit.", ""]
    for path, flag, typ, default, doc in SCHEMA:
        if flag is None:
            lines += ["", f"# {doc}" if doc else "", f"{path}:"]
            continue
        key = path.split(".", 1)[1]
        if typ == "bool":
            val = "true" if default else "false"
        elif typ == "str":
            val = default if default else '""'
        else:
            val = str(default)
        kv = f"  {key}: {val}"
        lines.append(f"{kv:<36} # {doc}" if doc else kv)
    return "\n".join(l for l in lines if l is not None) + "\n"


def main():
    p = argparse.ArgumentParser()
    p.add_argument("config", nargs="?")
    p.add_argument("--binary", default="./simnet")
    p.add_argument("--out-root", default=".")
    p.add_argument("--dry-run", action="store_true")
    p.add_argument("--reference", action="store_true")
    a = p.parse_args()

    if a.reference:
        sys.stdout.write(reference())
        return
    if not a.config:
        p.error("config is required")

    cfg = yaml.safe_load(Path(a.config).read_text())
    name = cfg.get("name", Path(a.config).stem)
    outdir = Path(a.out_root) / f"{name}-{time.strftime('%Y%m%d-%H%M%S')}"
    cmd = [a.binary] + flags(cfg, outdir)

    if a.dry_run:
        print(" ".join(shlex.quote(c) for c in cmd))
        return

    outdir.mkdir(parents=True, exist_ok=True)
    log = outdir / "run.log"
    print(f"{outdir}\n{' '.join(shlex.quote(c) for c in cmd)}", flush=True)
    with log.open("w") as f:
        rc = subprocess.call(cmd, stdout=f, stderr=subprocess.STDOUT)
    (outdir / "run.yaml").write_text(yaml.safe_dump(resolved(cfg, log), sort_keys=False))
    print(f"exit {rc}; log at {log}")
    sys.exit(rc)


if __name__ == "__main__":
    main()
