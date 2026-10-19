#!/usr/bin/env python3
"""Prices a fleet before and after it exists, for deploy/aws.sh.

  aws-cost.py estimate <regions-json> <home-region> <public_ips> <types-csv> <max_hours>
      Prints the expected hourly cost (every node on the first, cheapest
      type) and the ceiling (every node on the most expensive type the
      autoscaling group may fall back to), each with volumes, the
      coordinator and a cross-region traffic allowance. Last line:
      CEILING <total over max_hours>.
  aws-cost.py fleet <regions-json> <home-region> <types-csv> <mix-json>
      mix-json is {region: {instance type: count}} as launched. Prints the
      mix priced per region against the expected cost and exits 2 when the
      fleet costs more than FLEET_TOLERANCE (10 %) over the expectation, or
      is short.

Prices are on-demand, USD per hour, from the pricing API on 2026-10-19; a
region not listed is priced at us-east-1 plus 30 % and flagged. The traffic
allowance is TRANSFER_GB_NODE_H (0.1) GB per node-hour at $0.02/GB, the
inter-region rate: the 10-18 run moved 0.06 GB per node-hour across
regions with plain discv5 crawling, so 0.1 is a ceiling for today's
workloads.
"""
import json
import os
import sys

PRICES = {  # region: {type: $/h}; gp3 is $/GB-month
    "us-east-1": {"t4g.nano": 0.0042, "t4g.micro": 0.0084, "t4g.small": 0.0168, "t4g.medium": 0.0336, "m7g.large": 0.0816, "gp3": 0.080},
    "us-west-2": {"t4g.nano": 0.0042, "t4g.micro": 0.0084, "t4g.small": 0.0168, "t4g.medium": 0.0336, "m7g.large": 0.0816, "gp3": 0.080},
    "eu-central-1": {"t4g.nano": 0.0048, "t4g.micro": 0.0096, "t4g.small": 0.0192, "t4g.medium": 0.0384, "m7g.large": 0.0978, "gp3": 0.0952},
    "ap-southeast-1": {"t4g.nano": 0.0053, "t4g.micro": 0.0106, "t4g.small": 0.0212, "t4g.medium": 0.0424, "m7g.large": 0.1020, "gp3": 0.096},
    "sa-east-1": {"t4g.nano": 0.0067, "t4g.micro": 0.0134, "t4g.small": 0.0268, "t4g.medium": 0.0536, "m7g.large": 0.1301, "gp3": 0.152},
}
NODE_GB, COORD_GB, COORD_TYPE = 8, 20, "m7g.large"
HOURS_PER_MONTH = 730
TRANSFER_PER_GB = 0.02
TRANSFER_GB_NODE_H = float(os.environ.get("TRANSFER_GB_NODE_H", "0.1"))
PUBLIC_IP = 0.005
unknown = set()


def price(region, what):
    p = PRICES.get(region)
    if p is None:
        unknown.add(region)
        p = {k: v * 1.3 for k, v in PRICES["us-east-1"].items()}
    if what not in p:
        sys.exit("no price for %s; add it to deploy/aws-cost.py" % what)
    return p[what]


def hourly(regions, home, types, pub, per_region_type=None):
    """$/h for the fleet, every node on types[region] (or the given mix)."""
    nodes = coord = volumes = transfer = 0.0
    for r, n in regions.items():
        if n <= 0 and r != home:
            continue
        mix = per_region_type[r] if per_region_type else {types[r]: n}
        for t, c in mix.items():
            nodes += c * price(r, t)
        cnt = sum(mix.values())
        volumes += cnt * NODE_GB * price(r, "gp3") / HOURS_PER_MONTH
        transfer += cnt * TRANSFER_GB_NODE_H * TRANSFER_PER_GB
        if pub:
            nodes += cnt * PUBLIC_IP
    coord = price(home, COORD_TYPE) + COORD_GB * price(home, "gp3") / HOURS_PER_MONTH + (PUBLIC_IP if pub else 0)
    return {"nodes": nodes, "coordinator": coord, "volumes": volumes, "transfer": transfer, "total": nodes + coord + volumes + transfer}


def show(label, h):
    print("%s: $%.2f/h (nodes $%.2f, volumes $%.2f, coordinator $%.2f, traffic allowance $%.2f)"
          % (label, h["total"], h["nodes"], h["volumes"], h["coordinator"], h["transfer"]))


def main():
    cmd = sys.argv[1]
    regions = json.loads(sys.argv[2])
    home = sys.argv[3]
    if cmd == "estimate":
        pub = sys.argv[4] == "true"
        types = sys.argv[5].split(",")
        hours = float(sys.argv[6])
        cheap = {r: types[0] for r in regions}
        dear = {r: max(types, key=lambda t: price(r, t)) for r in regions}
        exp = hourly(regions, home, cheap, pub)
        ceil = hourly(regions, home, dear, pub)
        show("expected (all nodes on %s)" % types[0], exp)
        if len(types) > 1:
            show("ceiling (all nodes on %s)" % dear[home], ceil)
            print("the autoscaling group falls back along %s when a pool runs dry; the 10-18 run of 7,500 nodes landed two thirds of them off t4g.nano" % ",".join(types))
        print("over %g h: expected $%.2f, ceiling $%.2f" % (hours, exp["total"] * hours, ceil["total"] * hours))
        for r in sorted(unknown):
            print("warning: no price table for %s; priced at us-east-1 plus 30 %%" % r)
        print("CEILING %.2f" % (ceil["total"] * hours))
    elif cmd == "fleet":
        types = sys.argv[4].split(",")
        mix = json.loads(sys.argv[5])
        pub = False
        exp = hourly(regions, home, {r: types[0] for r in regions}, pub)
        act = hourly(regions, home, None, pub, per_region_type={r: mix.get(r, {}) for r in regions})
        short = []
        for r, n in regions.items():
            have = sum(mix.get(r, {}).values())
            parts = ", ".join("%d %s" % (c, t) for t, c in sorted(mix.get(r, {}).items(), key=lambda kv: -kv[1]))
            print("%s: %d of %d nodes (%s) $%.2f/h" % (r, have, n, parts or "none", sum(c * price(r, t) for t, c in mix.get(r, {}).items())))
            if have < n:
                short.append("%s short by %d" % (r, n - have))
        over = (act["total"] - exp["total"]) / exp["total"] * 100 if exp["total"] else 0
        tol = float(os.environ.get("FLEET_TOLERANCE", "10"))
        show("fleet as launched", act)
        show("expected", exp)
        bad = []
        if over > tol:
            bad.append("the fleet costs %.0f%% more than expected (tolerance %g%%)" % (over, tol))
        bad += short
        if bad:
            print("WARNING: " + "; ".join(bad))
            sys.exit(2)
        print("fleet within %.0f%% of the expected cost" % max(over, 0))
    else:
        sys.exit(__doc__)


main()
