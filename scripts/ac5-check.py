#!/usr/bin/env python3
"""Score a complement run against AC-5's mandatory named set.

Why this is a script and not an eyeball: AC-5 states its pass *threshold* as a count over
`tests/csapi`, but its *mandatory set* is a list of named tests that live in both complement packages.
Reading the threshold as the scope is how 15 mandatory tests went unmeasured for a whole milestone. A
machine-checked list cannot be misread that way, and neither can a test that was never run be counted as
anything other than unmeasured.

The mandatory names are parsed out of ACCEPTANCE-CRITERIA.md rather than duplicated here, so the two
cannot drift apart and quietly redefine what "done" means.

Usage:  scripts/ac5-check.py [--previous earlier-results.txt] [results-file ...]
        scripts/ac5-check.py --regressions-only --previous earlier.txt current.txt
            Score only AC-5's regression clause; the exit code then reflects nothing else.
Exit:   0 when every mandatory test passed and nothing regressed, 1 otherwise.
"""
import os
import re
import sys
from collections import Counter

HERE = os.path.dirname(os.path.abspath(__file__))
CRITERIA = os.path.join(os.path.dirname(HERE), "docs", "ACCEPTANCE-CRITERIA.md")


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


def regressions(previous, current):
    """Every test that passed in an earlier run and does not pass now.

    AC-5 requires this: "any test that passed in an earlier recorded run must still pass". It is
    a separate rule from the mandatory set for a reason worth stating, because the two can point
    in opposite directions. A run that fixes three tests and breaks one is an improvement by
    count and a regression by requirement, and a scorecard that reports only the count reports
    the improvement — which is exactly how a product gets worse while its scoreboard gets longer.

    A test with no verdict this run is reported too, and separately: an inventory that quietly
    stopped including a test keeps the pass rate intact while measuring less of the product, and
    "we did not look" is not a pass.
    """
    broken, unmeasured = [], []
    for name, verdict in sorted(previous.items()):
        if verdict != "PASS":
            continue
        now = current.get(name)
        if now is None:
            unmeasured.append(name)
        elif now != "PASS":
            broken.append(f"{name} ({verdict} -> {now})")
    return broken, unmeasured


def report_regressions(previous_path, previous, current):
    """Print the regression clause and report whether it holds."""
    broken, unmeasured = regressions(previous, current)
    shared = sum(1 for name in previous if name in current)
    print(f"\nregressions against {os.path.basename(previous_path)} "
          f"({sum(1 for v in previous.values() if v == 'PASS')} passing there):")
    if len(previous) < len(current):
        # Say what was not compared. A shorter earlier list can only find a subset of the
        # regressions — a test it never measured cannot be seen to go backwards — and "none" printed
        # over a subset is how a green light gets a bad name.
        print(f"  PARTIAL COMPARISON: that run recorded {len(previous)} verdicts and this one "
              f"{len(current)}; {shared} names are comparable, and anything the earlier run did not "
              f"measure cannot regress there")
    if not broken and not unmeasured:
        print("  none — every test that passed then passes now")
        return True
    for line in broken:
        print(f"  REGRESSED   {line}")
    for name in unmeasured:
        print(f"  NOT MEASURED {name} passed in the earlier run and has no verdict in this one")
    return False


def main():
    args = list(sys.argv[1:])
    previous_path = None
    if "--previous" in args:
        index = args.index("--previous")
        if index + 1 >= len(args):
            print("--previous wants a verdict file from an earlier run", file=sys.stderr)
            return 2
        previous_path = args[index + 1]
        del args[index:index + 2]
    regressions_only = "--regressions-only" in args
    if regressions_only:
        args.remove("--regressions-only")

    if args:
        paths = args
    else:
        # No silent default, and no default pointing at a file that may or may not exist: scoring a
        # run is not something to do against whatever is lying around.
        print("name the verdict file to score (a complement results.txt); this script does not "
              "guess which run you mean", file=sys.stderr)
        return 2
    results = recorded(paths)

    if regressions_only:
        # The mandatory set is deliberately not scored here. AC-5's regression clause has its own
        # answer and its own exit code, so a caller asked about one gets only that — a mode that
        # printed both would be a mode whose output somebody has to interpret by hand, and the
        # point of this script is that nobody does.
        if previous_path is None:
            print("--regressions-only also needs --previous", file=sys.stderr)
            return 2
        counts = Counter(results.values())
        print(f"recorded verdicts:   {len(results)} tests, {counts.get('PASS', 0)} passing")
        return 0 if report_regressions(previous_path, recorded([previous_path]), results) else 1

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
    clean = True
    if previous_path is not None:
        clean = report_regressions(previous_path, recorded([previous_path]), results)
    else:
        print("\nregressions: not checked (no --previous run given). AC-5 requires that any test "
              "\npassing in an earlier run still passes, so pass the archived verdicts of the "
              "\nrun this one should be compared against.")

    ok = not failing and not missing and not skipped and clean
    verdict = "MET" if ok else "NOT MET"
    print(f"\nAC-5 mandatory set: {verdict}" + ("" if clean else ", and this run regressed"))
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
