#!/usr/bin/env bash
# Sweep complement's WHOLE upstream suite against qwatrix and score AC-5 honestly.
#
# Two rules this encodes, both learned the hard way:
#
#   1. Complement's suite lives in two packages, and AC-5's mandatory named set draws from both. A
#      sweep of ./tests/csapi alone — which is what `make complement` defaults to, because the count
#      threshold is phrased over that package — left 15 mandatory tests unmeasured for a milestone.
#   2. A panic inside a test binary aborts everything after it in that package, and `go test` still
#      prints a tidy summary that reads like a complete run. So each chunk is its own process, and any
#      test left without a verdict is retried on its own; only then is it reported as CRASH.
#
# Usage:  scripts/complement-sweep.sh [out-dir]
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT="${1:-/tmp/qwatrix-sweep}"
PKG_LIST="${COMPLEMENT_PKGS:-./tests/csapi ./tests}"
mkdir -p "$OUT"

COMBINED="$OUT/results.txt"
: > "$COMBINED"

for pkg in $PKG_LIST; do
  slug=$(echo "$pkg" | tr -d './')
  prefix="$OUT/$slug"
  COMPLEMENT_PKG="$pkg" COMPLEMENT_CHUNK_SIZE="${COMPLEMENT_CHUNK_SIZE:-8}" \
    "$HERE/complement-chunks.sh" "$prefix"
  cp "$prefix.results" "$prefix.pass1"

  # Retry the tests whose process died before reaching a verdict, one per process. A test that panics
  # the binary then costs itself a verdict and nobody else's.
  crashed=$(awk '$1=="CRASH"{print $2}' "$prefix.results" | sort -u)
  if [ -n "$crashed" ]; then
    echo "  retrying $(echo "$crashed" | wc -l) undecided tests individually"
    : > "$prefix.retry"
    for t in $crashed; do
      out="$OUT/retry-$t.log"
      (cd "${COMPLEMENT_DIR:-$HOME/complement}" && \
        COMPLEMENT_BASE_IMAGE="${COMPLEMENT_BASE_IMAGE:-localhost/qwatrix-complement:latest}" \
        COMPLEMENT_HOSTNAME_RUNNING_COMPLEMENT="${COMPLEMENT_HOSTNAME_RUNNING_COMPLEMENT:-host.containers.internal}" \
        timeout 420 go test -tags "${COMPLEMENT_TAGS:-blacklist_common}" -count=1 -timeout=7m -v \
          -run "^$t\$" "$pkg" > "$out" 2>&1)
      if grep -qE "^--- (PASS|FAIL|SKIP): $t " "$out"; then
        grep -E "^--- (PASS|FAIL|SKIP): $t " "$out" | head -1 | awk -v t="$t" '{print $2, t}' \
          >> "$prefix.retry"
      else
        echo "CRASH $t" >> "$prefix.retry"
      fi
    done
    # The retry replaces the crash, and only the crash: a verdict from the first pass is kept.
    awk '$1!="CRASH"' "$prefix.results" > "$prefix.kept"
    cat "$prefix.kept" "$prefix.retry" > "$prefix.results"
  fi
  cat "$prefix.results" >> "$COMBINED"
done

echo
echo "=== combined: $(wc -l < "$COMBINED") verdicts across $PKG_LIST ==="
for state in PASS FAIL SKIP CRASH; do
  printf '%-6s %s\n' "$state" "$(awk -v s="$state" '$1==s' "$COMBINED" | wc -l)"
done
echo
echo "=== per package (AC-5 states its count threshold over tests/csapi, and its federation"
echo "    reporting separately, so they are reported on their own denominators)"
for pkg in $PKG_LIST; do
  slug=$(echo "$pkg" | tr -d './')
  file="$OUT/$slug.results"
  [ -e "$file" ] || continue
  total=$(wc -l < "$file")
  passing=$(awk '$1=="PASS:"' "$file" | wc -l)
  case "$slug" in
    testscsapi) echo "  $pkg: $passing of $total passing (AC-5 threshold 70): $( [ "$passing" -ge 70 ] && echo OK || echo BELOW )" ;;
    *)          echo "  $pkg: $passing of $total passing (federation, reported separately)" ;;
  esac
done

echo
echo "=== AC-5 ==="

# Keep the verdicts. AC-5's regression clause — "any test that passed in an earlier recorded run
# must still pass" — is a comparison, and a comparison needs both sides: a run whose verdicts
# live only under /tmp cannot be the earlier run of anything, and one host reboot once took the
# only copy of a run along with the run in flight, which left a four-name improvement
# unreconcilable against a two-name net.
ARCHIVE_DIR="${ARCHIVE_DIR:-$(cd "$HERE/.." && pwd)/.agent-work}"
RUN_SLUG="$(basename "$OUT" | tr -cd '0-9')"
[ -n "$RUN_SLUG" ] || RUN_SLUG="$(date +%Y%m%d-%H%M%S)"
ARCHIVE="$ARCHIVE_DIR/complement-run-$RUN_SLUG.txt"
mkdir -p "$ARCHIVE_DIR"
cp "$COMBINED" "$ARCHIVE"
echo "verdicts archived: $ARCHIVE"

# Compare against the previous *numbered* run, and only a numbered run. An earlier version of this
# took the newest file in the directory, which found a hand-copied `complement-run-latest.txt`
# covering 106 of the 197 tests — so a comparison that had checked almost nothing printed "no
# regressions", which is the one thing this check is allowed never to do. Run numbers are the only
# names with an order that means "before".
PREVIOUS=""
best=-1
for candidate in "$ARCHIVE_DIR"/complement-run-[0-9]*.txt; do
  [ -e "$candidate" ] || continue
  label="$(basename "$candidate" .txt)"; label="${label#complement-run-}"
  number="${label%%[!0-9]*}"
  [ -n "$number" ] || continue
  [ "$candidate" = "$ARCHIVE" ] && continue
  if [ "$number" -lt "$RUN_SLUG" ] && [ "$number" -gt "$best" ]; then best="$number"; PREVIOUS="$candidate"; fi
done
CHECK_ARGS=()
if [ -n "$PREVIOUS" ]; then
  echo "comparing against: $PREVIOUS"
  CHECK_ARGS=(--previous "$PREVIOUS")
else
  echo "comparing against: nothing archived yet, so no regression check is possible"
fi
python3 "$HERE/ac5-check.py" "${CHECK_ARGS[@]+"${CHECK_ARGS[@]}"}" "$COMBINED"
echo
echo "results: $COMBINED"
