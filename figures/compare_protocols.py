#!/usr/bin/env python3
"""Short comparison of discovery protocols run on the same scenario: TopDisc
against plain discv5, with the same lookup model on both sides. For the full
per-run report layout with both protocols, use report_pair.py.

Usage:
    compare_protocols.py --out DIR [--title TITLE] [--note TEXT] LABEL=RUN_DIR [LABEL=RUN_DIR ...]

Writes <DIR>/report.md, <DIR>/stats.json and the figures in <DIR>/figures.
The figures and tables come from pairfigs.py.
"""
import argparse
import os

import pairfigs as pf


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    ap.add_argument("--title", default="")
    ap.add_argument("--note", action="append", default=[], help="caveat printed under the run list")
    ap.add_argument("--log-time", action="store_true", help="log-scale time axes (default linear)")
    ap.add_argument("runs", nargs="+", help="LABEL=RUN_DIR")
    args = ap.parse_args()
    pf.LOG_TIME = args.log_time
    ctx = pf.Ctx([pf.Run(*r.split("=", 1), i) for i, r in enumerate(args.runs)], args.out)
    for fn in (pf.found_over_time, pf.time_to_first, pf.final_share, pf.traffic_over_time,
               pf.search_cost, pf.serving_load, pf.peer_slots):
        fn(ctx)

    L = [f"# {args.title or 'Protocol comparison'}\n", "## Runs\n"] + pf.runs_table(ctx)
    caveats = pf.notes(ctx, args.note)
    if caveats:
        L += ["", "## Caveats\n"] + [f"- {n}" for n in caveats]
    L += ["", "## Search\n"] + pf.search_table(ctx)
    L += ["", "### Time to first provider per topic\n"] + pf.first_by_topic_table(ctx)
    L += ["", "## Load\n"] + pf.load_table(ctx)
    peers = pf.peer_table(ctx)
    if peers:
        L += ["", "## Peer slots\n"] + peers
    health = pf.health_table(ctx)
    if health:
        L += ["", "## Run health\n"] + health
    L += ["", "## Figures\n"]
    for stem, _ in ctx.figs:
        L += pf.figure_md(ctx, stem)
    if ctx.missing:
        L += ["## Not in this report\n"] + [f"- `{s}`: {why}" for s, why in ctx.missing]
    with open(os.path.join(args.out, "report.md"), "w") as f:
        f.write("\n".join(L) + "\n")
    pf.write_stats(ctx)
    print(os.path.join(args.out, "report.md"))


if __name__ == "__main__":
    main()
