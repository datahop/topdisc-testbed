#!/usr/bin/env python3
"""Steady-state check for a long simnet run: per 5-minute series sample, the
change in ads held, TOPICQUERY and REGTOPIC rates, and per-topic find counts."""
import re, subprocess, sys
log = open(sys.argv[1], errors="replace").read()
rows = [tuple(map(int, m)) for m in re.findall(r"\[series t=(\d+)s\] ads=(\d+) cap=\d+ topicquery_tx=(\d+) regtopic_tx=(\d+) regtopic_renewal_tx=(\d+)", log)]
print(f"series samples: {len(rows)}")
rates = []
for a, b in zip(rows, rows[1:]):
    dt = (b[0] - a[0]) / 60 or 1
    rates.append((b[0], b[1], (b[2] - a[2]) / dt, (b[3] + b[4] - a[3] - a[4]) / dt))
for t, ads, tq, rg in rates[-8:]:
    print(f"  t={t/3600:5.2f}h ads={ads:>9} topicquery/min={tq:>10.0f} regtopic/min={rg:>8.0f}")
def spread(v):
    m = sum(v) / len(v)
    return (max(v) - min(v)) / m if m else 0
if len(rates) >= 6:
    w = rates[-6:]
    s_ads, s_tq, s_rg = spread([r[1] for r in w]), spread([r[2] for r in w]), spread([r[3] for r in w])
    print(f"last 30 min spread: ads {100*s_ads:.1f}%  topicquery rate {100*s_tq:.1f}%  regtopic rate {100*s_rg:.1f}%")
    print("STABLE" if s_ads < 0.05 and s_tq < 0.10 and s_rg < 0.10 else "NOT STABLE YET")
cov = re.findall(r"\[checkpoint t=(\d+)s\] topic (\d+): registrants=(\d+) coveredBy>=1=(\d+) \S+ neverFound=(\d+) p50=(\d+)", log)
last = {}
for t, topic, reg, covd, nf, p50 in cov:
    last.setdefault(topic, []).append((int(t), int(p50), int(covd)))
for topic in sorted(last):
    print(f"  topic {topic}: last find-count p50 " + " ".join(f"{p}@{t//60}m" for t, p, _ in last[topic][-4:]))
print(subprocess.run(["free", "-g"], capture_output=True, text=True).stdout.splitlines()[1])
