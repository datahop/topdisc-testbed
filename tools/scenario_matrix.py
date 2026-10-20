#!/usr/bin/env python3
"""Writes the evaluation matrix: four backends x two protocols x two lookup
models x churn or not, 32 scenarios named
<backend>-<size>-<topdisc|discv5>-<cont|conn>[-churn].yaml. Every run uses
the 2026-09-18 crawl as its topic and churn model, bootstrap sampling, and
the same phases within a backend, so any two differ in one thing only.
Re-run after editing the tables below; the files it writes are tracked."""
import os
import yaml

MODEL = "models/crawl-2026-09-18.json"
BACKENDS = {
    # size, population, network, phases, testbed section
    "g5k-25k": dict(
        nodes=25000, topics=100,
        network={"latency_ms": 30, "bandwidth_mibps": 100, "model": "regions",
                 "regions": {"us-east-1": 0.4, "eu-central-1": 0.3, "ap-southeast-1": 0.3},
                 "rtt_table": "models/region-rtt.json"},
        phases={"bootstrap_wait": "180s", "start_window": "30m", "register_wait": "22m30s", "search_stagger": "10ms", "search_timeout": "2h"},
        testbed={"backend": "cloud",
                 "cloud": {"inventory": "inventory.json", "home_region": "us-east-1", "verbosity": 2, "grace": "30s"},
                 "g5k": {"site": "nancy", "cluster": "gros", "env": "debian11-x64-base", "vnodes_per_machine": 500, "walltime": "05:00:00"},
                 "wan": {"enabled": True, "delay_min_ms": 4, "delay_max_ms": 45, "jitter_ms": 3, "rate_kbps": 1000},
                 "traces": {"host_sample_period": "30s"}},
        addresses="crawl"),
    "aws-7k5": dict(
        nodes=7500, topics=100,
        network={"regions": {"us-east-1": 0.3334, "eu-central-1": 0.3333, "ap-southeast-1": 0.3333}},
        phases={"bootstrap_wait": "3m", "start_window": "15m", "register_wait": "15m", "search_stagger": "10ms", "search_timeout": "90m"},
        testbed={"backend": "cloud", "cloud": {"inventory": "inventory.json", "home_region": "us-east-1", "public_ips": False}},
        addresses=None),
    "simnet-7k5": dict(
        nodes=7500, topics=100,
        network={"latency_ms": 30, "bandwidth_mibps": 1000},
        phases={"bootstrap_wait": "180s", "start_window": "15m", "register_wait": "15m", "search_stagger": "100ms", "search_timeout": "90m"},
        testbed={"simulator": {"link_buf": 32768, "router_buf": 32768, "router_shards": 16},
                 "traces": {"metrics": "metrics.json", "overhead": "oh.json", "overhead_series": "series.json",
                            "overhead_series_period": "5m", "checkpoint_interval": "15m"}},
        addresses="crawl"),
    "local-200": dict(
        nodes=200, topics=5,
        network=None,
        phases={"bootstrap_wait": "20s", "start_window": "2m", "register_wait": "30s", "search_stagger": "10ms", "search_timeout": "4m"},
        testbed={"backend": "local"},
        addresses=None),
}
DESC = {
    "g5k-25k": "Grid'5000, 25,000 nodes on 50 gros machines with region RTTs",
    "aws-7k5": "AWS, 7,500 nodes in three regions, private addresses",
    "simnet-7k5": "simnet, 7,500 in-process nodes",
    "local-200": "local backend, 200 nodes on one machine",
}


def scenario(backend, proto, model, churn):
    b = BACKENDS[backend]
    pop = {"nodes": b["nodes"], "topics": b["topics"], "seed": 1, "bootstrap_sample": 5}
    if b["topics"] >= 100:
        pop.update({"zipf_s": 1.07, "topic_model": MODEL})
    else:
        pop["zipf_s"] = 1.0
    if b["addresses"]:
        pop["addresses"] = b["addresses"]
    if proto == "discv5":
        pop.update({"legacy_frac": 1, "legacy_bootnode": True, "legacy_build": "fork"})
    sc = {"population": pop}
    if b["network"]:
        sc["network"] = b["network"]
    sc["phases"] = dict(b["phases"])
    sc["topic"] = {"ad_cache_size": 5000, "search_yield_floor": 8}
    sc["search"] = {"model": "continuous" if model == "cont" else "conn"}
    sc["conn_model"] = {"enabled": model == "conn"}
    if churn:
        sc["session_churn"] = {"enabled": True, "model": MODEL, "window_real_hours": 24}
    name = f"{backend}-{proto}-{model}" + ("-churn" if churn else "")
    head = (f"# {DESC[backend]}: {'TopDisc' if proto == 'topdisc' else 'plain discv5 built on the fork'}, "
            f"{'one continuous search per node' if model == 'cont' else 'the connection model (searches feed the dialer, stop at full slots)'}, "
            f"{'each node leaving and returning as its chain did in the crawl, one day over the run' if churn else 'no churn'}.\n"
            f"# One of the 32 matrix scenarios written by tools/scenario_matrix.py.\n")
    body = yaml.safe_dump({"name": name, "scenario": sc, "testbed": b["testbed"]}, sort_keys=False, width=120)
    # a bare hh:mm:ss is a YAML 1.1 sexagesimal number
    return name, head + body.replace("walltime: 05:00:00", 'walltime: "05:00:00"')


def main():
    out = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "scenarios")
    n = 0
    for backend in BACKENDS:
        for proto in ("topdisc", "discv5"):
            for model in ("cont", "conn"):
                for churn in (False, True):
                    name, text = scenario(backend, proto, model, churn)
                    open(os.path.join(out, name + ".yaml"), "w").write(text)
                    n += 1
    print("wrote", n, "scenarios")


main()
