"""The run's bootnodes, which load distributions report apart from the registrars.

Every node bootstraps from node 0 (pkg/assign), so node 0 sits in every
routing table and receives several times the topic requests of any registrar.
The bootstrap_sample peers listed after it are ordinary nodes and stay in.
"""
import glob
import json
import os
import re


def from_assignments(assign):
    """Indexes of the nodes that are some node's first bootnode; {0} when no
    assignment names one (simnet, which bootstraps from node 0)."""
    at = {}
    for i, a in assign.items():
        for ip in (a.get("ip"), a.get("ext_ip")):
            if ip and a.get("port"):
                at[(ip, int(a["port"]))] = i
    boot = set()
    for a in assign.values():
        m = re.search(r"@([^:/?]+):(\d+)", (a.get("bootnodes") or [""])[0])
        if m and (m.group(1), int(m.group(2))) in at:
            boot.add(at[(m.group(1), int(m.group(2)))])
    return boot or {0}


def of_run(rd):
    assign = {}
    for p in glob.glob(os.path.join(rd, "assignments", "node*.json")):
        with open(p) as f:
            a = json.load(f)
        assign[a["idx"]] = a
    return from_assignments(assign)
