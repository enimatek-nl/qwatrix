#!/usr/bin/env bash
# Run the whole upstream Complement csapi suite against qwatrix and report an honest score.
#
# Why this exists instead of a single `go test ./tests/csapi`:
#
#   Complement's own push tests leak a goroutine that fails after its parent test has finished
#   ("panic: Fail in goroutine after TestPushSync/... has completed"). A panic in a goroutine
#   aborts the whole test binary, so a single `go test` invocation silently stops after
#   whichever test happens to be compiled 50th — everything after it never runs, and `go test`
#   still prints a tidy `FAIL <package> 242.921s` summary that reads exactly like a complete run.
#   Measuring that way reported 50 tests when the package holds 106.
#
# So the suite is enumerated with -list and run in chunks, each chunk its own `go test` process:
# a panic then costs at most one chunk, which this script reports as a crash rather than a pass.
#
# Usage:  COMPLEMENT_CHUNK_SIZE=8 scripts/complement-chunks.sh [log-prefix]
set -uo pipefail

COMPLEMENT_DIR="${COMPLEMENT_DIR:-$HOME/complement}"
IMAGE="${COMPLEMENT_BASE_IMAGE:-localhost/qwatrix-complement:latest}"
CHUNK="${COMPLEMENT_CHUNK_SIZE:-8}"
# Which Complement package to enumerate. The suite lives in two: ./tests/csapi holds the
# client-API tests, ./tests the server-server ones and several client tests AC-5 names as
# mandatory. Sweeping only the first is a real mistake this file made once, so the package is
# a parameter and the sweep script runs both.
PKG="${COMPLEMENT_PKG:-./tests/csapi}"
PREFIX="${1:-/tmp/complement-chunk}"
TAGS="${COMPLEMENT_TAGS:-blacklist_common}"
# Tests that abort their chunk's process. Kept in their own chunk of one so the crash cannot
# take other tests with it: they are still failures, just never evidence about anything else.

export COMPLEMENT_BASE_IMAGE="$IMAGE"
export COMPLEMENT_HOSTNAME_RUNNING_COMPLEMENT="${COMPLEMENT_HOSTNAME_RUNNING_COMPLEMENT:-host.containers.internal}"

cd "$COMPLEMENT_DIR" || exit 1

LIST="$PREFIX.list"
go test -tags "$TAGS" -list '.*' "$PKG" 2>/dev/null | grep -E '^Test[A-Za-z0-9_]*$' | sort -u > "$LIST"
TOTAL=$(wc -l < "$LIST")
echo "suite: $PKG — $TOTAL top-level tests (tags: $TAGS)"

RESULT="$PREFIX.results"
LOGS="$PREFIX.log"
: > "$RESULT"
: > "$LOGS"

i=0
batch=0
while true; do
  mapfile -t tests < <(tail -n +"$((i + 1))" "$LIST" | head -n "$CHUNK")
  [ ${#tests[@]} -eq 0 ] && break
  batch=$((batch + 1))
  pattern=$(printf '%s|' "${tests[@]}"); pattern=${pattern%|}
  out="$PREFIX.batch$batch.log"
  # Complement leaves a named volume behind for every container it runs. Podman caps those locks
  # at 2048, and the twelfth full sweep of a session hits the ceiling — after which every Deploy
  # fails with "allocation failed; exceeded num_locks" and the verdict file reports sixty
  # regressions that never happened. Reclaiming between batches is cheap; a void measurement is
  # not, because it costs an hour and teaches nothing.
  docker system prune -f --volumes >/dev/null 2>&1 || true
  timeout 900 go test -tags "$TAGS" -count=1 -timeout=15m -v -run "^($pattern)\$" "$PKG" \
    > "$out" 2>&1
  code=$?
  cat "$out" >> "$LOGS"
  # A chunk whose process died mid-flight leaves tests without a result line. Naming them is
  # the point: reporting them as merely absent is how the 50-of-106 mistake happened.
  for t in "${tests[@]}"; do
    if grep -qE "^--- (PASS|FAIL|SKIP): $t " "$out"; then
      grep -E "^--- (PASS|FAIL|SKIP): $t " "$out" | head -1 |
        awk -v t="$t" '{print $2, t}' >> "$RESULT"
    else
      echo "CRASH $t" >> "$RESULT"
    fi
  done
  echo "  batch $batch: ${#tests[@]} tests, exit $code"
  i=$((i + CHUNK))
done

echo
for state in PASS FAIL SKIP CRASH; do
  printf '%-6s %s\n' "$state" "$(awk -v s="$state" '$1==s' "$RESULT" | wc -l)"
done
echo
echo "failing or crashed (top-level):"
awk '$1!="PASS" && $1!="SKIP" {print "  " $1" "$2}' "$RESULT" | sort
echo
echo "full log: $LOGS"
