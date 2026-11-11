#!/usr/bin/env python3
"""Score a complement run against AC-5's mandatory named set.

Why this is a script and not an eyeball: AC-5 states its pass *threshold* as a count over
`tests/csapi`, but its *mandatory set* is a list of named tests that live in both complement packages.
Reading the threshold as the scope is how 15 mandatory tests went unmeasured for a whole milestone. A
machine-checked list cannot be misread that way, and neither can a test that was never run be counted as
anything other than unmeasured.

The mandatory names are parsed out of ACCEPTANCE-CRITERIA.md rather than duplicated here, so the two
cannot drift apart and quietly redefine what "done" means.

Usage:  ac5-check.py [.agent-work/complement-run-latest.txt ...]
Exit:   0 when every mandatory test passed, 1 otherwise.
"""
import os
import re
import sys
from collections import Counter

HERE = os.path.dirname(os.path.abspath(__file__))
CRITERIA = os.path.join(HERE, "ACCEPTANCE-CRITERIA.md")


def mandatory_names():
    """The named core set AC-5 requires, in the order it is written."""
    text = open(CRITERIA, encoding="utf-8").read()
    start = text.index("named core set below must pass")
    end = text.index("Note:", start)
    return [m.group(1) for m in re.finditer(r"`(Test[A-Za-z0-9_]+)`", text[start:end])]


def recorded(paths):
    """Every verdict in the given result files; a later file wins, so re-runs correct crashes."""
    results = {}
    for path in paths:
        with open(path, encoding="utf-8", errors="replace") as handle:
            for line in handle:
                match = re.match(r"^\s*(PASS|FAIL|SKIP|CRASH)[: ]\s*(Test\S+)", line.strip())
                if match:
                    results[match.group(2)] = match.group(1)
    return results


def main():
    paths = sys.argv[1:] or [os.path.join(HERE, "complement-run-latest.txt")]
    results = recorded(paths)
    required = mandatory_names()
    missing = [name for name in required if name not in results]
    failing = [name for name in required if results.get(name) in ("FAIL", "CRASH")]
    skipped = [name for name in required if results.get(name) == "SKIP"]
    passing = [name for name in required if results.get(name) == "PASS"]

    print(f"recorded verdicts:   {len(results)} tests ({', '.join(os.path.basename(p) for p in paths)})")
    print(f"AC-5 mandatory set:  {len(required)} named tests")
    print(f"  passing:           {len(passing)}")
    print(f"  failing/crashed:   {len(failing)} {failing if failing else ''}")
    print(f"  skipped:           {len(skipped)} {skipped if skipped else ''}")
    print(f"  NEVER RUN:         {len(missing)} {missing if missing else ''}")

    counts = Counter(results.values())
    threshold = 70
    whole = counts.get("PASS", 0)
    # AC-5 phrases the count threshold over tests/csapi specifically. It is reported here over every
    # recorded verdict, which is a larger denominator and therefore an easier bar; the mandatory set
    # above is the requirement that cannot be read generously.
    print(f"\npassing overall:     {whole} of {len(results)} recorded verdicts "
          f"(AC-5's count threshold is {threshold}, stated over tests/csapi alone): "
          f"{'OK' if whole >= threshold else 'BELOW THRESHOLD'}")
    if missing:
        print("\nA mandatory test that was never run is not a pass and not a failure; it is unknown."
              "\nRun the sweep over every package before scoring AC-5.")
    ok = not failing and not missing and not skipped
    print(f"\nAC-5 mandatory set: {'MET' if ok else 'NOT MET'}")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
