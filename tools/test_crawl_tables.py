#!/usr/bin/env python3
"""Fixture test for crawl_tables.py: python3 -I tools/test_crawl_tables.py"""
import importlib.util
import json
import os
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location("crawl_tables", os.path.join(HERE, "crawl_tables.py"))
ct = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ct)


def nid(x):
    return "%064x" % x


# Distances from P1 (id 0) are the bit lengths of the ids.
P1, P2 = nid(0), nid(1 << 255)
A, B, E = nid((1 << 255) | 1), nid(1 << 254), nid((1 << 255) | 7)
X238, X240, X243 = nid(1 << 237), nid(1 << 239), nid((1 << 242) | 5)
MIN = 60000
def ev(minute, **kw):
    return dict(t=minute * MIN, **kw)


def reply(minute, peer, dist, nodes=None, err=None):
    o = ev(minute, ev="reply", dist=dist, **{"from": peer})
    if nodes:
        o["nodes"] = nodes
    if err:
        o["err"] = err
    return o


def read(minute, peer, by_dist):
    return [reply(minute + (d < 240), peer, [d], by_dist.get(d)) for d in range(256, 223, -1)]


EVENTS = (
    [ev(0, ev="seen", id=P1, eth2="11111111"), ev(0, ev="seen", id=P2, eth="22222222"),
     ev(1, ev="up", id=P1), ev(1, ev="up", id=P2)]
    + read(2, P1, {256: [B], 243: [X243]})
    + [reply(4, P2, [d]) for d in range(256, 244, -1)] + [reply(5, P2, [244], err="RPC timeout")]
    + read(10, P1, {238: [X238], 240: [X240], 243: [X243], 255: [B], 256: [A]})
    + [ev(12, ev="pong", id=A, rtt=20), ev(13, ev="up", id=X240)]
    + read(50, P1, {256: [E]})
    + [ev(51, ev="up", id=E), ev(60, ev="sweep", n=-1)]
)


class Test(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.path = os.path.join(self.dir.name, "events.jsonl")
        with open(self.path, "w") as f:
            for e in EVENTS:
                f.write(json.dumps(e) + "\n")

    def tearDown(self):
        self.dir.cleanup()

    def test_exclude_15m(self):
        r = ct.analyze(self.path, 15 * 60)
        self.assertEqual((r["t_end"], r["peers_read"], r["peers_partial"], r["alive_ids"]), (60 * MIN, 1, 1, 5))
        self.assertNotIn(P2, r["nodes"])
        n = r["nodes"][P1]
        self.assertEqual((n["t"], n["chain"], n["entries"], n["dead"]), (11 * MIN, "eth2:11111111", 5, 3))
        self.assertEqual(n["buckets"], {
            "256": [{"id": A, "alive": True}],
            "255": [{"id": B, "alive": False}],
            "243": [{"id": X243, "alive": False}],
            "240": [{"id": X240, "alive": True}, {"id": X238, "alive": False}],
        })
        s = r["summary"]
        self.assertEqual((s["entries_mean"], s["dead_share_entries"]), (5, 0.6))
        self.assertEqual(s["per_node_dead_share"]["n"], 1)
        self.assertEqual(s["per_node_dead_share"]["mean"], 0.6)
        self.assertEqual(s["bucket_fill"]["240"], {"n": 1, "mean": 2})
        self.assertEqual(s["bucket_fill"]["256"], {"n": 1, "mean": 1})
        self.assertEqual(s["bucket_fill"]["241"], {"n": 0, "mean": 0})
        self.assertEqual(len(s["bucket_fill"]), 17)

    def test_no_exclusion(self):
        r = ct.analyze(self.path, 0)
        self.assertEqual((r["peers_read"], r["peers_partial"]), (1, 1))
        self.assertEqual(r["nodes"][P1]["buckets"], {"256": [{"id": E, "alive": True}]})

    def test_error_below_240(self):
        p3 = nid(1 << 100)
        y238, y256 = nid((1 << 237) | (1 << 100)), nid(1 << 255)
        evs = [reply(2, p3, [d], {256: [y256], 238: [y238]}.get(d)) for d in range(256, 236, -1)]
        evs += [reply(3, p3, [236], err="RPC timeout"), ev(30, ev="sweep", n=-1)]
        with open(self.path, "w") as f:
            for e in evs:
                f.write(json.dumps(e) + "\n")
        r = ct.analyze(self.path, 0)
        self.assertEqual((r["peers_read"], r["peers_partial"]), (1, 0))
        self.assertEqual(r["nodes"][p3]["t"], 3 * MIN)
        self.assertEqual(r["nodes"][p3]["buckets"], {"256": [{"id": y256, "alive": False}],
                                                     "240": [{"id": y238, "alive": False}]})

    def test_cli(self):
        out = os.path.join(self.dir.name, "x.json")
        p = subprocess.run([sys.executable, "-I", os.path.join(HERE, "crawl_tables.py"), self.path,
                            "--exclude-last", "15m", "--out", out], capture_output=True, text=True, check=True)
        self.assertIn("peers read 1  partial 1", p.stdout)
        with open(out) as f:
            self.assertEqual(json.load(f)["nodes"][P1]["entries"], 5)
        subprocess.run([sys.executable, "-I", os.path.join(HERE, "crawl_tables.py"), self.path],
                       capture_output=True, check=True)
        self.assertTrue(os.path.exists(os.path.join(self.dir.name, "tables.json")))


if __name__ == "__main__":
    unittest.main()
