#!/usr/bin/env python3
"""Drop chosen arrays from a MarshalIndent'ed metrics.json, streaming.

writeMetrics pretty-prints, so every array element sits on its own line at
~18 bytes a piece. At 25k searchers each recording every provider it found,
that put the 2026-10-12 run's metrics.json at 8.39 GB -- past what figures.py
can json.load. The arrays below are the bulk and none of them is read by the
figure a given run can actually draw, so emptying them in place keeps the file
loadable without touching anything a script reads.

  newFoundAtMs        read by no script at all
  foundRegistrantIds  fan-out figures only, which need registrationCoverage
  foundIds            .byRegistrant -- empty in an all-legacy run, so those
                      figures produce nothing whatever this holds

Keeps uniqueFoundAtMs (the discovery curves) and uniqueFoundIds (the ID-space
figure in figures_overhead.py). Memory stays flat: one line at a time.
"""
import re, sys

DROP = {"newFoundAtMs", "foundRegistrantIds", "foundIds"}

open_re = re.compile(r'^(\s*)"([A-Za-z]+)": \[\s*$')

def main(src, dst, drop):
    kept = dropped = 0
    with open(src, "r") as f, open(dst, "w") as o:
        it = iter(f)
        for line in it:
            m = open_re.match(line)
            if m and m.group(2) in drop:
                indent, key = m.group(1), m.group(2)
                # consume to the matching close, which is the next line whose
                # stripped form is "]" or "],"  (these arrays are flat)
                for inner in it:
                    st = inner.strip()
                    if st in ("]", "],"):
                        o.write('%s"%s": []%s\n' % (indent, key, "," if st.endswith(",") else ""))
                        break
                    dropped += 1
                continue
            o.write(line)
            kept += 1
    print("lines kept %d, array lines dropped %d" % (kept, dropped))

if __name__ == "__main__":
    main(sys.argv[1], sys.argv[2], DROP)
