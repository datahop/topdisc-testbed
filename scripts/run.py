#!/usr/bin/env python3
"""Run a testbed config.

Turns a YAML run config into the binary's flags, creates a timestamped output
directory, and streams the log to it. Use --dry-run to see the command.
"""
import argparse, os, platform, shlex, subprocess, sys, time
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

    # Anything else the binary accepts, verbatim: {flag-name: value}.
    for k, v in (cfg.get("flags") or {}).items():
        emit(k, v)
    return args


def provenance(binary):
    """Exact sources behind this run: testbed commit plus the module versions
    baked into the binary (the pinned go-ethereum and simnet forks)."""
    out = {"host": platform.node(), "started": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())}
    try:
        out["testbed_commit"] = subprocess.check_output(
            ["git", "rev-parse", "HEAD"], stderr=subprocess.DEVNULL, text=True).strip()
    except Exception:
        pass
    try:
        info = subprocess.check_output(["go", "version", "-m", binary], text=True)
        mods = {}
        lines = info.splitlines()
        for i, ln in enumerate(lines):
            f = ln.split()
            if len(f) >= 3 and f[0] == "dep":
                # a following "=>" line is the replacement actually used
                repl = lines[i + 1].split() if i + 1 < len(lines) else []
                if repl and repl[0] == "=>":
                    mods[f[1]] = f"{repl[1]} {repl[2]}"
                else:
                    mods[f[1]] = f[2]
        out["modules"] = mods
    except Exception:
        pass
    return out


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
    # Snapshot what was run next to its traces, so a result directory is
    # reproducible on its own: the config as given, and the exact command.
    snapshot = dict(cfg)
    snapshot["provenance"] = provenance(a.binary)
    snapshot["provenance"]["command"] = " ".join(shlex.quote(c) for c in cmd)
    (outdir / "run.yaml").write_text(yaml.safe_dump(snapshot, sort_keys=False))
    log = outdir / "run.log"
    print(f"{outdir}\n{' '.join(shlex.quote(c) for c in cmd)}", flush=True)
    with log.open("w") as f:
        rc = subprocess.call(cmd, stdout=f, stderr=subprocess.STDOUT)

    # The binary logs every flag it ran with, defaults included. Fold that in
    # so run.yaml alone pins the run even if a default changes later.
    for ln in log.read_text().splitlines():
        if ln.startswith("PARAMS: "):
            snapshot["resolved_flags"] = dict(kv.split("=", 1) for kv in ln[8:].split() if "=" in kv)
            (outdir / "run.yaml").write_text(yaml.safe_dump(snapshot, sort_keys=False))
            break
    print(f"exit {rc}; log at {log}")
    sys.exit(rc)


if __name__ == "__main__":
    main()
