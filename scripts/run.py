#!/usr/bin/env python3
"""Run a testbed config.

Turns a YAML run config into the binary's flags, creates a timestamped output
directory, and streams the log to it. Use --dry-run to see the command.
"""
import argparse, os, shlex, subprocess, sys, time
from pathlib import Path

try:
    import yaml
except ImportError:
    sys.exit("needs PyYAML: pip install pyyaml")

# config key -> flag name, for keys whose flag is not just the key with dashes
FLAG = {
    "latency_ms": "latency", "bandwidth_mibps": "bandwidth-mibps",
    "max_peers": "conn-max-peers", "dial_ratio": "conn-dial-ratio",
    "redial_wait": "conn-redial-wait", "gap": "session-churn-gap",
    "metrics": "metrics-out", "overhead": "overhead-out",
    "overhead_series": "overhead-series-out", "reach": "reach-out",
}


def flags(cfg, outdir):
    args = []
    def emit(k, v):
        if isinstance(v, bool):
            if v:
                args.append(f"-{k}")
        else:
            args.extend([f"-{k}", str(v)])

    for k in ("nodes", "topics", "seed", "zipf_s"):
        if k in cfg:
            emit(k.replace("_", "-"), cfg[k])
    if cfg.get("all_register"):
        emit("all-register", True)

    for section in ("network", "phases", "topic", "traces"):
        for k, v in (cfg.get(section) or {}).items():
            flag = FLAG.get(k, k.replace("_", "-"))
            # trace outputs are filenames inside the run directory
            if section == "traces" and isinstance(v, str) and v.endswith(".json"):
                v = str(Path(outdir) / v)
            emit(flag, v)

    cm = cfg.get("conn_model") or {}
    if cm.get("enabled"):
        emit("conn-model", True)
        for k, v in cm.items():
            if k == "enabled":
                continue
            emit(FLAG.get(k, k.replace("_", "-")), v)

    sc = cfg.get("session_churn") or {}
    if sc.get("enabled"):
        emit("session-churn", True)
        for k, v in sc.items():
            if k == "enabled":
                continue
            emit(FLAG.get(k, "session-churn-" + k.replace("_", "-")), v)

    dc = cfg.get("disconnect") or {}
    for k, v in dc.items():
        emit("disconnect-" + k.replace("_", "-"), v)
    return args


def main():
    p = argparse.ArgumentParser()
    p.add_argument("config")
    p.add_argument("--binary", default="./simnet")
    p.add_argument("--out-root", default=".")
    p.add_argument("--dry-run", action="store_true")
    a = p.parse_args()

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
    print(f"exit {rc}; log at {log}")
    sys.exit(rc)


if __name__ == "__main__":
    main()
