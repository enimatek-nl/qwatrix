#!/usr/bin/env python3
"""Summarise a chunked Complement run against the AC-5 mandatory list.

Usage: complement-score.py RESULTS [BASELINE]   (e.g. /tmp/cc2.results)

The results file is one "<STATE> <TestName>" per line, as written by complement-chunks.sh.
"""
import re
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent


def load(path):
    status = {}
    for line in Path(path).read_text().splitlines():
        parts = line.split()
        if len(parts) >= 2:
            status[parts[1].rstrip(":")] = parts[0].rstrip(":").upper()
    return status


def mandatory():
    text = (HERE / "ACCEPTANCE-CRITERIA.md").read_text()
    start = text.index("Rooms/events: `TestRoomCreate`")
    end = text.index("Note: `TestTxnIdWithRefreshToken`")
    return set(re.findall(r"`(Test[A-Za-z0-9_]+)`", text[start:end]))


status = load(sys.argv[1])
baseline = load(sys.argv[2]) if len(sys.argv) > 2 else None

def count(state):
    return sum(1 for v in status.values() if v == state)

# Name the files in the header. Passing the two arguments in the wrong order once made a
# completed run score as its own baseline, hiding a test that had just started passing — and
# nothing in the output looked wrong, which is the worst kind of tooling failure.
print(f"run: {sys.argv[1]}   baseline: {sys.argv[2] if len(sys.argv) > 2 else 'none'}")
print(f"measured: {len(status)}   pass: {count('PASS')}   "
      f"fail: {count('FAIL')}   crash: {count('CRASH')}   skip: {count('SKIP')}")

# A run whose runner has not finished measures a subset, and diffing a subset against a full
# baseline reports every test it never reached as a regression. Say so, because the alternative
# is printing a regression list that looks like a catastrophe and is an artefact.
if len(status) < 100:
    print(f"WARNING: only {len(status)} verdicts, so this run looks incomplete (the suite is "
          f"106 tests). Wait for the runner before trusting the counts or the regression list.")

must = mandatory()
measured = {name for name in must if name in status}
passed = {name for name in measured if status[name] == "PASS"}
print(f"\nmandatory: {len(must)} named, {len(measured)} present in this Complement checkout, "
      f"{len(passed)} passing ({100*len(passed)//max(len(measured),1)}%)")
absent = sorted(must - set(status))
if absent:
    print(f"named by AC-5 but not in the suite ({len(absent)}): {', '.join(absent)}")

if baseline:
    gained = sorted(n for n, v in status.items() if v == "PASS" and baseline.get(n) != "PASS")
    lost = sorted(n for n, v in baseline.items() if v == "PASS" and status.get(n) != "PASS")
    print(f"\nnewly passing ({len(gained)}): {', '.join(gained) or 'none'}")
    print(f"REGRESSIONS ({len(lost)}): {', '.join(lost) or 'none'}")

print("\nstill failing or crashed:")
for name in sorted(n for n, v in status.items() if v != "PASS"):
    marker = "mandatory" if name in must else "         "
    print(f"  {status[name]:6} {marker}  {name}")
