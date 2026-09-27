#!/usr/bin/env python3
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0

"""Summarizes the go -debug-trace files written by measure.sh.

    summarize.py <out-dir>

For each build: wall time, time before the first action runs (package
loading / module graph), link time, and build actions that took over
100 ms, which on a warm cache means the package was actually compiled
rather than served from $GOCACHE.
"""
import glob
import json
import os
import sys


def spans(path):
    ev = json.load(open(path))
    ev = ev if isinstance(ev, list) else ev["traceEvents"]
    stacks, out = {}, []
    for e in ev:
        if e.get("ph") == "B":
            stacks.setdefault(e["tid"], []).append(e)
        elif e.get("ph") == "E":
            b = stacks[e["tid"]].pop()
            out.append((b["name"], b["ts"] / 1000, e["ts"] / 1000))
    return out


def main():
    print("build\twall_ms\tload_ms\tlink_ms\tcompiled(>100ms)")
    for path in sorted(glob.glob(os.path.join(sys.argv[1], "*.trace.json"))):
        ss = spans(path)
        t0 = min(s[1] for s in ss)
        t1 = max(s[2] for s in ss)
        actions = [s for s in ss if s[0].startswith("Executing action")]
        first = min(s[1] for s in actions) if actions else t1
        link = sum(s[2] - s[1] for s in actions if "(link " in s[0])
        slow = sorted(
            ((s[2] - s[1], s[0][len("Executing action (build "):-1])
             for s in actions if "(build " in s[0] and s[2] - s[1] > 100),
            reverse=True)
        slow_s = ", ".join(f"{n}={d:.0f}" for d, n in slow[:4])
        more = f" (+{len(slow) - 4} more)" if len(slow) > 4 else ""
        name = os.path.basename(path)[: -len(".trace.json")]
        print(f"{name}\t{t1 - t0:.0f}\t{first - t0:.0f}\t{link:.0f}\t"
              f"{len(slow)}: {slow_s}{more}")


if __name__ == "__main__":
    main()
