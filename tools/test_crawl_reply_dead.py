#!/usr/bin/env python3
"""Fixture test for crawl_reply_dead.py: python3 -I tools/test_crawl_reply_dead.py"""
import importlib.util
import json
import os
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location("crawl_reply_dead", os.path.join(HERE, "crawl_reply_dead.py"))
crd = importlib.util.module_from_spec(spec)
spec.loader.exec_module(crd)


def nid(x):
    return "%064x" % x


P1, P2 = nid(0), nid(1 << 255)
A, B, C, D = nid(1 << 254), nid(1 << 253), nid((1 << 255) | 1), nid((1 << 255) | (1 << 250))
MIN = 60000


def ev(minute, **kw):
    return dict(t=minute * MIN, **kw)


EVENTS = [
    ev(0, ev="seen", id=P1, eth2="11111111"),
    ev(1, ev="seen", id=P2, eth="22222222"),
    ev(2, ev="up", id=P1),
    ev(3, ev="up", id=P2),
    ev(4, ev="seen", id=A, opstack=10),
    ev(5, ev="seen", id=B),
    ev(6, ev="seen", id=C, eth2="44444444"),
    ev(7, ev="pong", id=A, rtt=20),
    ev(8, ev="up", id=C),
    ev(10, ev="reply", **{"from": P1}, dist=[254, 255, 256], nodes=[A, B]),
    ev(11, ev="reply", **{"from": P1}, dist=[255, 256, 248], nodes=[A, C]),
    ev(12, ev="reply", **{"from": P2}, dist=[251, 256, 249], nodes=[A, B, D]),
    ev(12, ev="seen", id=D, src="reply"),
    ev(13, ev="reply", **{"from": P2}, dist=[250, 252, 253], err="RPC timeout"),
    ev(14, ev="reply", **{"from": P1}, dist=[248, 249, 250]),
    ev(16, ev="enr", id=P2, eth="33333333"),
    ev(17, ev="sweep", n=1),
    ev(50, ev="reply", **{"from": P1}, dist=[254, 250, 249], nodes=[B]),
    ev(55, ev="pong", id=P1, rtt=30),
    ev(60, ev="sweep", n=-1),
]


class Test(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.path = os.path.join(self.dir.name, "events.jsonl")
        with open(self.path, "w") as f:
            for e in EVENTS:
                f.write(json.dumps(e) + "\n")
        self.assertEqual(len(EVENTS), 20)

    def tearDown(self):
        self.dir.cleanup()

    def test_exclude_15m(self):
        r = crd.analyze(self.path, 15 * 60)
        self.assertEqual((r["replies"], r["replies_used"], r["replies_with_nodes"], r["replies_empty"]), (6, 5, 3, 1))
        self.assertEqual(r["reply_errors"], {"RPC timeout": 1})
        self.assertAlmostEqual(r["per_reply"]["all"]["mean"], (0.5 + 0 + 2 / 3) / 3)
        self.assertEqual(r["per_reply"]["by_peer_chain"]["eth2:11111111"]["n"], 2)
        self.assertAlmostEqual(r["per_reply"]["by_peer_chain"]["eth:33333333"]["mean"], 2 / 3)
        self.assertAlmostEqual(r["per_peer"]["all"]["mean"], (0.25 + 2 / 3) / 2)
        rc = r["records_by_chain"]
        self.assertEqual(rc["op:10"], {"entries": 3, "dead_share": 0.0, "unique": 1, "unique_dead_share": 0.0})
        self.assertEqual(rc["none"], {"entries": 3, "dead_share": 1.0, "unique": 2, "unique_dead_share": 1.0})
        self.assertEqual(rc["eth2:44444444"]["dead_share"], 0.0)
        rd = r["records_by_distance"]
        self.assertEqual(rd["254"], {"entries": 1, "dead_share": 1.0})
        self.assertEqual(rd["255"], {"entries": 2, "dead_share": 0.0})
        self.assertEqual(rd["251"], {"entries": 1, "dead_share": 1.0})
        self.assertEqual(rd["256"]["entries"], 3)
        self.assertAlmostEqual(rd["256"]["dead_share"], 1 / 3)

    def test_no_exclusion(self):
        r = crd.analyze(self.path, 0)
        self.assertEqual(r["replies_used"], 6)
        self.assertEqual(r["per_reply"]["all"]["n"], 4)
        self.assertEqual(r["per_reply"]["all"]["p90"], 1.0)

    def test_cli(self):
        out = os.path.join(self.dir.name, "x.json")
        p = subprocess.run([sys.executable, "-I", os.path.join(HERE, "crawl_reply_dead.py"), self.path,
                            "--exclude-last", "15m", "--out", out], capture_output=True, text=True, check=True)
        self.assertIn("dead share per FINDNODE request (dedup records): n 3", p.stdout)
        with open(out) as f:
            self.assertEqual(json.load(f)["replies_used"], 5)
        subprocess.run([sys.executable, "-I", os.path.join(HERE, "crawl_reply_dead.py"), self.path],
                       capture_output=True, check=True)
        self.assertTrue(os.path.exists(os.path.join(self.dir.name, "reply_dead.json")))


if __name__ == "__main__":
    unittest.main()
