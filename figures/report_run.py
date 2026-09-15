#!/usr/bin/env python3
"""Build the per-run report of a simnet run directory: the scenario, then
registration, discovery and overhead results with every per-run figure and log
table, then peer connections, churn and run health when the run has them.

Usage:
    report_run.py <run-dir> [--out DIR] [--reuse-figures]

Reads <run-dir>/run.log (PARAMS line and the === tables), metrics.json,
series.json and oh.json; runs figures.py and figures_overhead.py into
<out>/figures (skipped with --reuse-figures when figures already exist); writes
<out>/report.md (default <run-dir>/report). Figures or tables the run could not
produce are listed at the end with the reason.
"""
import argparse
import glob
import json
import os
import re
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))

# Report sections in order. Each lists log tables (heading, === title in
# run.log, required) and figures (stem, script, caption). Optional sections
# appear only when one of their tables is in the log.
SECTIONS = [
    {
        "name": "Registration and cache",
        "tables": [
            ("Registration coverage after the register-wait phase", "post-register-wait coverage", True),
            ("Registration latency", "registration timing", True),
        ],
        "figures": [
            ("06_fanout_both_views", "figures.py", "(a) Registrars holding each registrant's ad; (b) ads held per registrar."),
            ("04_id_space_registrants", "figures.py", "Fan-out of each registrant across the ID space."),
            ("04b_id_space_registrars", "figures.py", "Ads held by each registrar across the ID space."),
            ("07_registration_latency_bar", "figures.py", "Mean ± 1σ time to first remote admission per topic."),
            ("07_placement_time_idspace", "figures.py", "Time to first admission across the ID space (earliest registrar)."),
            ("oh_05_wait_time_cdf", "figures_overhead.py", "(a) Waiting times quoted by registrars; (b) total time of every successful registration, from its first REGTOPIC to admission."),
            ("oh_08_cache_utilisation", "figures_overhead.py", "Ads held network-wide and per topic against cache capacity, over time."),
        ],
    },
    {
        "name": "Discovery",
        "tables": [
            ("Per-topic search results", "per-topic search summary", True),
            ("How many searchers found each registrant", "per-registrant find-count distribution", True),
            ("Scheduled lookups", "scheduled lookups", False),
        ],
        "final": True,
        "figures": [
            ("02_recall_reached", "figures.py", "Distinct registrants found over time and where each searcher finished (1.0 = every registrant of its topic)."),
            ("03_time_to_fraction", "figures.py", "Time for each searcher to find 50%, 90% and 99% of its topic's registrants."),
            ("02b_time_to_first_cdf", "figures.py", "CDF of time to the first result, per topic."),
            ("08_lookup_latency_cdf", "figures.py", "Lookup latency: time to reach F_lookup distinct registrants, per topic."),
            ("09_lookup_contacts_cdf", "figures.py", "Registrars contacted per lookup: distinct nodes asked and TOPICQUERY requests sent to reach F_lookup, per topic."),
            ("05_id_space_found_vs_missed", "figures.py", "How many searchers returned each registrant, across the ID space."),
            ("oh_06_idspace_found_time", "figures_overhead.py", "Time from when an ad could first be found (placed, with searches running) to its first discovery, across the ID space."),
        ],
    },
    {
        "name": "Overhead and load",
        "tables": [],
        "figures": [
            ("oh_01_idspace_traffic", "figures_overhead.py", "Messages and bytes sent and received per node, across the ID space."),
            ("oh_03_idspace_msgtype", "figures_overhead.py", "Per-node traffic by message type, across the ID space."),
            ("oh_10_reg_vs_lookup", "figures_overhead.py", "Registration vs lookup traffic per node."),
            ("oh_02_idspace_peak_rate", "figures_overhead.py", "Peak sustained per-node rate, across the ID space."),
            ("oh_04_idspace_peak_msgtype", "figures_overhead.py", "Peak sustained per-node rate by message type."),
            ("oh_07_load_vs_topic_distance", "figures_overhead.py", "Median received and sent traffic per node against XOR distance to each topic."),
            ("oh_09_cost_per_lookup", "figures_overhead.py", "Lookup traffic per searcher against topic size."),
            ("oh_11_topic_load", "figures_overhead.py", "Load distribution per topic: requests received and reply bytes sent per registrar, and registration and lookup traffic per member."),
        ],
        "summary": ("Load summary", "load_summary.md"),
    },
    {
        "name": "Peer connections",
        "optional": True,
        "tables": [("Connection model", "connection model", False)],
        "figures": [],
    },
    {
        "name": "Churn",
        "optional": True,
        "tables": [
            ("Stale results returned by searches", "dead results in searches", False),
            ("Churn activity", "churn summary", False),
        ],
        "figures": [
            ("10_dead_results", "figures.py", "(a) Share of returned results whose registrant was offline, over time; (b) how long those registrants had been offline, against the ad lifetime."),
        ],
    },
]

# Figures the scripts draw that the report leaves out on purpose.
EXCLUDED = {"01_topic_distribution"}  # the topic assignment is a scenario input, shown as a table

PARAM_KEYS = [
    ("backend", "testbed.backend"),
    ("nodes", "scenario.population.nodes"),
    ("topics", "scenario.population.topics"),
    ("zipf_s", "scenario.population.zipf_s"),
    ("all_register", "scenario.population.all_register"),
    ("legacy_frac", "scenario.population.legacy_frac"),
    ("seed", "scenario.population.seed"),
    ("latency_ms", "scenario.network.latency_ms"),
    ("bandwidth_mibps", "scenario.network.bandwidth_mibps"),
    ("regions", "scenario.network.regions"),
    ("bootstrap_wait", "scenario.phases.bootstrap_wait"),
    ("start_window", "scenario.phases.start_window"),
    ("register_stagger", "scenario.phases.register_stagger"),
    ("register_wait", "scenario.phases.register_wait"),
    ("search_stagger", "scenario.phases.search_stagger"),
    ("search_timeout", "scenario.phases.search_timeout"),
    ("search model", "scenario.search.model"),
    ("intervals", "scenario.search.intervals"),
    ("target_count", "scenario.search.target_count"),
    ("request_timeout", "scenario.search.request_timeout"),
    ("initial_results", "scenario.search.initial_results"),
    ("result_interval", "scenario.search.result_interval"),
    ("ad_lifetime", "scenario.topic.ad_lifetime"),
    ("ad_cache_size", "scenario.topic.ad_cache_size"),
    ("reg_attempt_timeout", "scenario.topic.reg_attempt_timeout"),
    ("reg_table_depth", "scenario.topic.reg_table_depth"),
    ("reg_bucket_size (K_register)", "scenario.topic.reg_bucket_size"),
    ("reg_bucket_standby", "scenario.topic.reg_bucket_standby"),
    ("search_table_depth", "scenario.topic.search_table_depth"),
    ("search_bucket_size (K_lookup)", "scenario.topic.search_bucket_size"),
    ("topic_nodes_limit (F_return)", "scenario.topic.topic_nodes_limit"),
    ("aux_nodes_limit", "scenario.topic.aux_nodes_limit"),
    ("conn_model", "scenario.conn_model.enabled"),
    ("session_churn", "scenario.session_churn.enabled"),
    ("churn interval", "scenario.churn.interval"),
    ("disconnect interval", "scenario.disconnect.interval"),
    ("build.testbed", "build.testbed"),
    ("build.fork", "build.fork"),
]


# Fork defaults (topicindex.Config.withDefaults) shown when a key is unset or 0.
TOPIC_DEFAULTS = {
    "scenario.topic.ad_lifetime": "15m0s",
    "scenario.topic.ad_cache_size": "5000",
    "scenario.topic.reg_table_depth": "10",
    "scenario.topic.reg_bucket_size": "5",
    "scenario.topic.reg_bucket_standby": "20",
    "scenario.topic.search_table_depth": "10",
    "scenario.topic.search_bucket_size": "16",
    "scenario.topic.topic_nodes_limit": "16",
    "scenario.topic.aux_nodes_limit": "8",
}


def param_value(params, key, searched_h=None):
    v = params.get(key)
    if key == "scenario.phases.search_timeout" and searched_h is not None and v:
        return f"{v} (stopped after {searched_h:.1f} h)"
    if key == "scenario.topic.reg_attempt_timeout" and v in (None, "0s", "0"):
        life = params.get("scenario.topic.ad_lifetime")
        return "1.5 × ad_lifetime (default)" if life in (None, "0s", "0") else f"1.5 × {life} (default)"
    if key in TOPIC_DEFAULTS and v in (None, "0s", "0"):
        return f"{TOPIC_DEFAULTS[key]} (default)"
    return v


def parse_params(log):
    m = re.search(r"^PARAMS: (.*)$", log, re.M)
    if not m:
        return {}
    return dict(re.findall(r"(\S+?)=(\{[^}]*\}|\S*)", m.group(1)))


def params_from_yaml(rd):
    """Flattened run.yaml, for backends whose log carries no PARAMS line."""
    try:
        import yaml
    except ImportError:
        return {}
    path = os.path.join(rd, "run.yaml")
    if not os.path.exists(path):
        return {}
    out = {}

    def walk(prefix, v):
        if isinstance(v, dict) and not prefix.endswith("regions"):
            for k, x in v.items():
                walk(f"{prefix}.{k}" if prefix else str(k), x)
        else:
            out[prefix] = str(v).lower() if isinstance(v, bool) else str(v)

    walk("", yaml.safe_load(open(path)) or {})
    return out


def metrics_tables(metrics):
    """The simnet log tables, rebuilt from metrics.json for real backends."""
    t = {}
    pt = sorted(metrics.get("perTopic") or [], key=lambda r: r["topic"])
    if pt:
        rows = ["topic     searchers   target   fullRecall   meanRecall   meanUnique"]
        for r in pt:
            full = f"{r['fullRecall']}/{r['numSearchers']}"
            rows.append(f"{r['topic']:<6} {r['numSearchers']:>12} {r['target']:>8} {full:>12} "
                        f"{r['meanRecall']:>12.4f} {r['meanUniqueCount']:>12.1f}")
        t["per-topic search summary"] = "\n".join(rows)
    fc = sorted(metrics.get("findCountByTopic") or [], key=lambda r: r["topic"])
    if fc:
        rows = ["topic   registrants neverFound      min       p5      p25      p50      p75      p95        max"]
        for r in fc:
            rows.append(f"{r['topic']:<6} {r['registrants']:>12} {r['neverFound']:>10} "
                        + " ".join(f"{r[k]:>8}" for k in ("min", "p5", "p25", "p50", "p75", "p95", "max"))
                        + f"   mean={r['mean']:.1f}")
        t["per-registrant find-count distribution"] = "\n".join(rows)
        t["final coverage"] = "\n".join(
            f"topic {r['topic']}: registrants={r['registrants']} coveredBy>=1={r['registrants'] - r['neverFound']} "
            f"({100 * (r['registrants'] - r['neverFound']) / max(r['registrants'], 1):.1f}%) neverFound={r['neverFound']} "
            f"p50={r['p50']} p95={r['p95']} max={r['max']}" for r in fc)
    cov = (metrics.get("registrationCoverage") or {}).get("byTopic") or {}
    rows = []
    for k in sorted(cov, key=int):
        fan = sorted((cov[k].get("byRegistrant") or {}).values())
        if fan:
            rows.append(f"  topic {k}: visible={len(fan)}  fan-out min={fan[0]} med={fan[len(fan) // 2]} max={fan[-1]}")
    if rows:
        t["post-register-wait coverage"] = "\n".join(rows)
    rt, ids = metrics.get("registrationTimingNs") or {}, metrics.get("topicIds") or {}
    rows = ["topic             n         mean          std"]
    for h in sorted(rt, key=lambda h: ids.get(h, 0)):
        vals = [v / 1e6 for v in rt[h].values()]
        if vals:
            mean = sum(vals) / len(vals)
            std = (sum((v - mean) ** 2 for v in vals) / len(vals)) ** 0.5
            rows.append(f"{ids.get(h, h[:8]):<6} {len(vals):>12} {mean:>12.1f} {std:>12.1f}")
    if len(rows) > 1:
        t["registration timing"] = "\n".join(rows)
    return t


def search_progress(mpath):
    """Per-topic medians of the searches' progress counters, and a note when
    searches kept re-walking their tables with nothing new to return."""
    if not os.path.exists(mpath):
        return ""
    by = {}
    for r in json.load(open(mpath)).get("results", []):
        st = r.get("searchStats")
        if st and st.get("passes"):
            by.setdefault(r["topic"], []).append(st)
    if not by:
        return ""
    keys = ["passes", "queries", "contacted", "received", "duplicate", "filtered", "yielded"]
    med = lambda v: sorted(v)[len(v) // 2]
    rows = ["| topic | searches | " + " | ".join(keys) + " | filtered / received |", "|---:|---:|" + "---:|" * (len(keys) + 1)]
    rewalk = []
    for t in sorted(by):
        m = {k: med([x.get(k, 0) for x in by[t]]) for k in keys}
        share = m["filtered"] / m["received"] if m["received"] else 0
        rows.append(f"| {t} | {len(by[t])} | " + " | ".join(str(m[k]) for k in keys) + f" | {100 * share:.0f}% |")
        if m["filtered"] > m["yielded"]:
            rewalk.append(str(t))
    out = "Median per search over the run; `filtered` counts results dropped as recently returned.\n\n" + "\n".join(rows) + "\n"
    if rewalk:
        out += (f"\n> **Search re-walk:** on topic(s) {', '.join(rewalk)} searches dropped more results as recently returned than they handed out: "
                "with no new registrants left they re-walk their tables every pass (datahop/go-ethereum#142). "
                "Query traffic and registrar load on these topics measure that loop.\n")
    return out


def parse_tables(log):
    tables = {}
    blocks = re.split(r"^=== (.+?) ===$", log, flags=re.M)
    for title, body in zip(blocks[1::2], blocks[2::2]):
        lines = []
        for ln in body.splitlines():
            if ln.startswith("===") or re.match(r"^(metrics|overhead|overhead series) written to|^teardown complete", ln):
                break
            if ln.startswith("[checkpoint") or ln.startswith("[buf]") or ln.startswith("[final"):
                continue
            lines.append(ln)
        while lines and not lines[-1].strip():
            lines.pop()
        tables[title] = "\n".join(lines).strip("\n")
    return tables


def table_for(tables, title):
    return next((v for k, v in tables.items() if k.startswith(title)), None)


def final_lines(log, kind):
    finals = [ln.split("] ", 1)[1] for ln in log.splitlines() if ln.startswith("[final") and "] " in ln]
    if kind == "coverage":
        return [ln for ln in finals if ln.startswith("topic ")]
    return [ln for ln in finals if ln.startswith("search-")]


def topic_assignment(summary):
    rows = []
    for ln in (summary or "").splitlines()[1:]:
        f = ln.split()
        if len(f) >= 2 and f[0].isdigit() and f[1].isdigit():
            rows.append((int(f[0]), int(f[1])))
    return rows


def run(cmd, gen):
    gen.write("$ " + " ".join(cmd) + "\n")
    gen.flush()
    p = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    gen.write(p.stdout + f"[exit {p.returncode}]\n\n")
    return p.returncode


def code(text):
    return "```\n" + text + "\n```\n"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("run_dir")
    ap.add_argument("--out", default="")
    ap.add_argument("--reuse-figures", action="store_true")
    args = ap.parse_args()
    rd = os.path.abspath(args.run_dir)
    out = os.path.abspath(args.out or os.path.join(rd, "report"))
    figdir = os.path.join(out, "figures")
    os.makedirs(figdir, exist_ok=True)

    logpath = os.path.join(rd, "run.log")
    log = open(logpath, errors="replace").read() if os.path.exists(logpath) else ""
    params = parse_params(log) or params_from_yaml(rd)
    tables = parse_tables(log)
    mpath = os.path.join(rd, "metrics.json")
    fallback = metrics_tables(json.load(open(mpath))) if os.path.exists(mpath) else {}
    for title, body in fallback.items():
        if title != "final coverage" and table_for(tables, title) is None:
            tables[title] = body
    name = params.get("name") or os.path.basename(rd)
    missing = []

    reuse = args.reuse_figures and glob.glob(os.path.join(figdir, "*.png"))
    if not reuse:
        with open(os.path.join(out, "gen.log"), "w") as gen:
            metrics = os.path.join(rd, "metrics.json")
            series = os.path.join(rd, "series.json")
            oh = os.path.join(rd, "oh.json")
            if os.path.exists(metrics):
                rc = run([sys.executable, os.path.join(HERE, "figures.py"), metrics, "--out-dir", figdir, "--label", name], gen)
                if rc:
                    missing.append(("figures.py", f"exited {rc}, see gen.log"))
            else:
                missing.append(("figures.py", "no metrics.json in the run directory (testbed.traces.metrics unset)"))
            if os.path.exists(series):
                cmd = [sys.executable, os.path.join(HERE, "figures_overhead.py"), series, "--out-dir", figdir, "--label", name]
                if os.path.exists(metrics):
                    cmd += ["--metrics", metrics]
                if os.path.exists(oh):
                    cmd += ["--overhead", oh]
                rc = run(cmd, gen)
                if rc:
                    missing.append(("figures_overhead.py", f"exited {rc}, see gen.log"))
            else:
                missing.append(("figures_overhead.py", "no series.json in the run directory (testbed.traces.overhead_series unset)"))
    for stray in ["report.md"] + [s + ext for s in EXCLUDED for ext in (".png", ".pdf")]:
        p = os.path.join(figdir, stray)
        if os.path.exists(p):
            os.remove(p)

    backend = {"cloud": "cloud", "local": "local"}.get(params.get("testbed.backend"), "simnet")
    L = [f"# TopDisc {backend} run — `{name}`\n"]

    # Scenario: what was run.
    L.append("## Scenario\n")
    topics = params.get("scenario.population.topics", "?")
    desc = f"{params.get('scenario.population.nodes', '?')} nodes, {topics} topic{'s' if topics != '1' else ''}"
    if topics not in ("1", "?"):
        desc += f" (Zipf s={params.get('scenario.population.zipf_s')})"
    desc += f", search model `{params.get('scenario.search.model', '?')}`"
    churn = params.get("scenario.session_churn.enabled") == "true" or params.get("scenario.churn.interval", "0s") != "0s"
    desc += ", churn" if churn else ", no churn"
    L.append(desc + ".\n")
    L.append(f"Run directory: `{rd}`\n")
    searched_h = None
    if os.path.exists(mpath) and "stop signal" in log:
        done = sorted(r.get("timeToCompletionNs", 0) for r in json.load(open(mpath)).get("results", []))
        if done:
            searched_h = done[len(done) // 2] / 3.6e12
            L.append(f"Stopped by signal after {searched_h:.1f} h of search (median over searchers); `search_timeout` was only an upper bound.\n")
    L.append("### Parameters\n")
    L.append("| parameter | value |\n|---|---|")
    for label, key in PARAM_KEYS:
        if key in params or key.startswith("scenario.topic."):
            L.append(f"| `{label}` | {param_value(params, key, searched_h)} |")
    L.append("")
    assign = topic_assignment(table_for(tables, "per-topic search summary"))
    if assign:
        total = sum(n for _, n in assign)
        L.append("### Topic assignment\n")
        L.append("| topic | nodes | share |\n|---:|---:|---:|")
        for t, n in assign:
            L.append(f"| {t} | {n} | {100 * n / total:.1f}% |")
        L.append("")
    else:
        missing.append(("table: Topic assignment", "no per-topic search summary in run.log"))

    present = {f[:-4] for f in os.listdir(figdir) if f.endswith(".png")}
    listed = set()
    num = 0
    for sec in SECTIONS:
        found = [(h, table_for(tables, t), req) for h, t, req in sec["tables"]]
        if sec.get("optional") and not any(body for _, body, _ in found):
            continue
        num += 1
        L.append(f"## {num}. {sec['name']}\n")
        for heading, body, required in found:
            if body:
                L.append(f"### {heading}\n")
                L.append(code(body))
            elif required:
                missing.append((f"table: {heading}", "section missing from run.log"))
        if sec.get("final"):
            cov = final_lines(log, "coverage")
            if cov or fallback.get("final coverage"):
                L.append("### Final coverage\n")
                L.append(code("\n".join(cov) if cov else fallback["final coverage"]))
            else:
                missing.append(("table: Final coverage", "no [final] coverage lines in run.log (testbed.traces.checkpoint_interval unset)"))
            prov = final_lines(log, "provenance")
            if prov:
                L.append("### Search provenance and bucket occupancy\n")
                L.append(code("\n".join(prov)))
            progress = search_progress(mpath)
            if progress:
                L.append("### Search progress\n")
                L.append(progress)
        for stem, script, caption in sec["figures"]:
            listed.add(stem)
            if stem in present:
                L.append(f"### {stem}\n")
                L.append(f"![{stem}](figures/{stem}.png)\n")
                L.append(f"*{caption}*\n")
            else:
                missing.append((stem, f"not produced by {script} (see gen.log)"))
        if sec.get("summary"):
            heading, fname = sec["summary"]
            path = os.path.join(figdir, fname)
            if os.path.exists(path):
                L.append(f"### {heading}\n")
                L.append(open(path).read())
            else:
                missing.append((f"table: {heading}", f"{fname} not written (needs per-topic counters in oh.json)"))

    extra = sorted(present - listed - EXCLUDED)
    if extra:
        L.append("## Other figures\n")
        for stem in extra:
            L.append(f"### {stem}\n")
            L.append(f"![{stem}](figures/{stem}.png)\n")

    health = [ln for ln in log.splitlines() if ln.startswith("[buf] final peak") or ln.startswith("hostmetrics:")]
    L.append(f"## {num + 1}. Run health\n")
    if health:
        L.append(code(health[-1]))
    else:
        missing.append(("table: Run health", "no final buffer line in run.log"))
    if backend == "simnet":
        L.append(f"Abort on packet drop: `{params.get('testbed.safety.abort_on_drop', '?')}`.\n")

    L.append("## Not in this report\n")
    if missing:
        L.append("| item | reason |\n|---|---|")
        for item, why in missing:
            L.append(f"| `{item}` | {why} |")
    else:
        L.append("Every expected figure and table was produced.")
    L.append("")

    with open(os.path.join(out, "report.md"), "w") as f:
        f.write("\n".join(L))
    print(os.path.join(out, "report.md"))
    for item, why in missing:
        print(f"missing: {item}: {why}")


if __name__ == "__main__":
    main()
