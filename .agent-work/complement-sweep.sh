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
# Usage:  .agent-work/complement-sweep.sh [out-dir]
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
echo "=== AC-5 ==="
python3 "$HERE/ac5-check.py" "$COMBINED"
echo
echo "results: $COMBINED"
