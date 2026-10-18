#!/usr/bin/env python3
"""Build the per-run report layout for a pair of runs of the same scenario,
TopDisc against plain discv5: every section shows both protocols, except
Registration and cache, which only TopDisc has.

Usage:
    report_pair.py --out DIR [--title TITLE] [--note TEXT] [--reuse-figures] LABEL=RUN_DIR LABEL=RUN_DIR

Runs report_run.py on each run into <DIR>/runs/<label>/ (its checks, log
tables and TopDisc-only figures are reused here), draws the paired figures
(pairfigs.py) into <DIR>/figures and writes <DIR>/report.md.
"""
import argparse
import json
import os
import re
import subprocess
import sys

import pairfigs as pf
import report_run as rr

HERE = os.path.dirname(os.path.abspath(__file__))

DISCOVERY = [pf.found_over_time, pf.final_share, pf.time_to_fraction, pf.discovery_rate, pf.time_to_first,
             pf.search_requests, pf.lookup_latency, pf.idspace_found_share]
LOAD = [pf.idspace_traffic, pf.msgtype_split, pf.traffic_over_time, pf.peak_rate_idspace,
        pf.load_vs_topic_distance, pf.cost_vs_topic_size, pf.search_cost, pf.serving_load]
DISCOVERY_FIGS = ["cp_01_found_over_time", "cp_03_final_share", "cp_08_time_to_fraction", "cp_09_discovery_rate",
                  "cp_02_time_to_first", "cp_10_search_requests", "cp_11_lookup_latency", "cp_12_idspace_found_share"]
LOAD_FIGS = ["cp_13_idspace_traffic", "cp_14_msgtype", "cp_04_traffic_over_time", "cp_15_peak_rate_idspace",
             "cp_16_load_vs_topic_distance", "cp_17_cost_vs_topic_size", "cp_05_search_cost", "cp_06_serving_load"]
# Figures of the TopDisc per-run report that have no plain-discv5 counterpart.
TOPDISC_DISCOVERY = ["12_search_bucket", "oh_06_idspace_found_time"]
TOPDISC_LOAD = ["oh_10_reg_vs_lookup", "oh_11_topic_load"]


def slug(label):
    return re.sub(r"[^a-z0-9]+", "-", label.lower()).strip("-") or "run"


def sections(md):
    """{section name without its number: body} of a report.md."""
    out, name, buf = {}, None, []
    for line in md.splitlines():
        if line.startswith("## "):
            if name:
                out[name] = "\n".join(buf).strip()
            name, buf = re.sub(r"^## (\d+\. )?", "", line).strip(), []
        elif name:
            buf.append(line)
    if name:
        out[name] = "\n".join(buf).strip()
    return out


def subsections(body):
    """[(heading, text)] of the ### blocks in a section body."""
    out, name, buf = [], None, []
    for line in body.splitlines():
        if line.startswith("### "):
            if name:
                out.append((name, "\n".join(buf).strip()))
            name, buf = line[4:].strip(), []
        elif name:
            buf.append(line)
    if name:
        out.append((name, "\n".join(buf).strip()))
    return out


def rebase(text, sub):
    """Point a per-run report's figure links at its folder under runs/."""
    return text.replace("](figures/", f"](runs/{sub}/figures/")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    ap.add_argument("--title", default="")
    ap.add_argument("--note", action="append", default=[], help="caveat printed under the scenario")
    ap.add_argument("--reuse-figures", action="store_true", help="keep existing per-run reports and figures")
    ap.add_argument("--log-time", action="store_true", help="log-scale time axes (default linear)")
    ap.add_argument("runs", nargs="+", help="LABEL=RUN_DIR")
    args = ap.parse_args()
    pf.LOG_TIME = args.log_time
    out = args.out
    runs = [pf.Run(*r.split("=", 1), i) for i, r in enumerate(args.runs)]
    subs = {r.label: slug(r.label) for r in runs}

    # Per-run reports: checks, log tables and the TopDisc-only figures.
    per = {}
    for r in runs:
        rdir = os.path.join(out, "runs", subs[r.label])
        if not (args.reuse_figures and os.path.exists(os.path.join(rdir, "report.md"))):
            cmd = [sys.executable, os.path.join(HERE, "report_run.py"), r.dir, "--out", rdir]
            if args.reuse_figures:
                cmd.append("--reuse-figures")
            subprocess.run(cmd, stdout=subprocess.DEVNULL, check=False)
        md = os.path.join(rdir, "report.md")
        per[r.label] = sections(open(md).read()) if os.path.exists(md) else {}
    checks = {r.label: pf.load(os.path.join(out, "runs", subs[r.label], "checks.json"), {}) for r in runs}

    ctx = pf.Ctx(runs, out)
    for fn in DISCOVERY + LOAD + [pf.peer_slots, pf.dead_results]:
        fn(ctx)
    topdisc = next((r for r in runs if not r.plain), None)

    def topdisc_figs(stems, missing):
        L = []
        if not topdisc:
            return L
        fdir = os.path.join(out, "runs", subs[topdisc.label], "figures")
        for stem in stems:
            if os.path.exists(os.path.join(fdir, stem + ".png")):
                cap = next((c for sec in rr.SECTIONS for s, _, c in sec["figures"] if s == stem), "")
                L += [f"### {stem} (TopDisc only)\n", f"![{stem}](runs/{subs[topdisc.label]}/figures/{stem}.png)\n", f"*{cap}*\n"]
            else:
                missing.append((f"{stem} (TopDisc only)", "not in the TopDisc per-run report"))
        return L

    def per_run_tables(section, headings):
        L = []
        for r in runs:
            blocks = [(h, t) for h, t in subsections(per[r.label].get(section, "")) if h in headings]
            if blocks:
                L.append(f"<details><summary>{r.label}: {', '.join(h for h, _ in blocks)}</summary>\n")
                for h, t in blocks:
                    L += [f"#### {h}\n", rebase(t, subs[r.label]), ""]
                L.append("</details>\n")
        return L

    missing = list(ctx.missing)
    L = [f"# {args.title or 'TopDisc and plain discv5, paired report'}\n"]

    # Scenario.
    L += ["## Scenario\n"] + pf.runs_table(ctx) + [""]
    prm = [r.p or rr.params_from_yaml(r.dir) for r in runs]
    L += ["### Parameters\n", pf.row(["parameter"] + [r.label for r in runs]), "|---|" + "---|" * len(runs)]
    for label, key in rr.PARAM_KEYS:
        if not any(key in p or key.startswith("scenario.topic.") for p in prm):
            continue
        vals = [rr.param_value(p, key) for p in prm]
        name = f"**`{label}`**" if len(set(vals)) > 1 else f"`{label}`"
        L.append(pf.row([name] + [str(v) for v in vals]))
    L += ["", "Bold: the setting differs between the runs.\n"]
    sizes = [r.size_of for r in runs]
    caveats = list(args.note)
    if any(s != sizes[0] for s in sizes[1:]):
        caveats.append("The runs drew different topic sizes; per-topic rows compare different populations.")
    L += ["### Topic assignment\n", "| topic | nodes | share |", "|---:|---:|---:|"]
    total = sum(sizes[0].values()) or 1
    for t in sorted(sizes[0], key=lambda t: -sizes[0][t]):
        L.append(f"| {t} | {sizes[0][t]:,} | {100 * sizes[0][t] / total:.1f} % |")
    caveats = pf.notes(ctx, caveats)
    if caveats:
        L += ["", "### Caveats\n"] + [f"- {n}" for n in caveats]
    L.append("")

    # Correctness checks, one status column per run.
    L.append("## Correctness checks\n")
    if all(checks.values()):
        L.append("`checks.py` on each run. *n/a*: a TopDisc check on the plain discv5 run; *not-exercised*: "
                 "the run never reached the condition; *needs-data*: the traces lack a field the check needs.\n")
        by = {r.label: {c["id"]: c for c in checks[r.label].get("checks", [])} for r in runs}
        ids = [c["id"] for c in checks[runs[0].label].get("checks", [])]
        L += [pf.row(["check", "assertion"] + [r.label for r in runs]), "|---|---|" + "---|" * len(runs)]
        bad = []
        for cid in ids:
            first = by[runs[0].label][cid]
            cells = []
            for r in runs:
                c = by[r.label].get(cid, {})
                st = c.get("status", "–")
                if c.get("summary") == "every node runs plain discv5":
                    st = "n/a"
                cells.append(f"**{st}**" if st in ("fail", "error") else st)
                if st in ("fail", "error"):
                    bad.append(f"- **{cid}** {r.label}: {c.get('summary', '')}")
            L.append(pf.row([f"**{cid}** {first.get('group', '')}", first.get("assertion", "")] + cells))
        L += [""] + (["Failures:\n"] + bad if bad else ["No check failed on either run."]) + [""]
    else:
        missing.append(("table: Correctness checks", "checks.json missing for a run (see runs/*/gen.log)"))

    num = 0
    # Registration and cache: TopDisc only.
    reg = per[topdisc.label].get("Registration and cache") if topdisc else None
    if reg:
        num += 1
        L += [f"## {num}. Registration and cache (TopDisc only)\n",
              "Plain discv5 nodes publish their service in their node record and register nothing, so this section has one run.\n",
              rebase(reg, subs[topdisc.label]), ""]

    # Discovery.
    num += 1
    L += [f"## {num}. Discovery\n", "### Per-topic search results\n",
          "Median share of its topic each searcher found by the end, and median time to 90 % (– when fewer than half got there).\n"]
    L += pf.per_topic_table(ctx) + ["", "### Time to first provider per topic\n",
          "*in table*: share of plain discv5 searchers whose first provider was already in their routing table (found at 0 ms).\n"]
    L += pf.first_by_topic_table(ctx) + ["", "### Search summary\n"] + pf.search_table(ctx) + [""]
    for stem in DISCOVERY_FIGS:
        L += pf.figure_md(ctx, stem)
    L += topdisc_figs(TOPDISC_DISCOVERY, missing)
    L += per_run_tables("Discovery", {"Per-topic search results", "How many searchers found each registrant", "Scheduled lookups", "Search progress"})

    # Overhead and load.
    num += 1
    L += [f"## {num}. Overhead and load\n", "### Load summary\n"] + pf.load_table(ctx) + [""]
    for stem in LOAD_FIGS:
        L += pf.figure_md(ctx, stem)
    L += topdisc_figs(TOPDISC_LOAD, missing)

    # Peer connections.
    peers = pf.peer_table(ctx)
    if peers:
        num += 1
        L += [f"## {num}. Peer connections\n"] + peers + [""] + pf.figure_md(ctx, "cp_07_peer_slots")
        L += per_run_tables("Peer connections", {"Connection model"})

    # Churn.
    churn = any(r.p.get("scenario.session_churn.enabled") == "true" or r.p.get("scenario.churn.interval", "0s") not in ("0s", "")
                for r in runs) or ctx.has("cp_18_dead_results")
    if churn:
        num += 1
        L += [f"## {num}. Churn\n"] + pf.figure_md(ctx, "cp_18_dead_results")
        L += per_run_tables("Churn", {"Stale results returned by searches", "Churn activity"})
    else:
        missing = [(s, w) for s, w in missing if s != "cp_18_dead_results"]

    # Run health.
    num += 1
    L += [f"## {num}. Run health\n"] + pf.health_table(ctx) + [""]
    for r in runs:
        body = per[r.label].get("Run health", "")
        if body:
            L += [f"{r.label}:\n", body, ""]

    # Not in this report: the paired figures, then each run's own list
    # without the TopDisc-only items a plain run leaves out by design.
    L.append("## Not in this report\n")
    rows = [(s, w) for s, w in missing]
    for r in runs:
        for line in per[r.label].get("Not in this report", "").splitlines():
            m = re.match(r"^\| `(.+?)` \| (.+) \|$", line)
            if m:
                rows.append((f"{m.group(1)} ({r.label})", m.group(2)))
    if rows:
        L += ["| item | reason |", "|---|---|"] + [f"| `{s}` | {w} |" for s, w in rows]
    else:
        L.append("Every expected figure and table was produced.")
    with open(os.path.join(out, "report.md"), "w") as f:
        f.write("\n".join(L) + "\n")
    pf.write_stats(ctx)
    print(os.path.join(out, "report.md"))
    for s, w in rows:
        print(f"missing: {s}: {w}")


if __name__ == "__main__":
    main()
