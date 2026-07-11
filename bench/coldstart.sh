#!/usr/bin/env bash
# Cold-start benchmark: time from "create sandbox" to "first exec result".
# This is vessel's headline number; run it on every release.
#
# Requires a running daemon:
#   VESSEL_KERNEL=... VESSEL_ROOTFS=... vessel serve &
#
# Usage: ./coldstart.sh [-u http://localhost:7070] [-d cloudhypervisor] [-n 10]
set -euo pipefail

URL=http://localhost:7070
DRIVER=cloudhypervisor
N=10

while getopts "u:d:n:h" opt; do
  case $opt in
    u) URL=$OPTARG ;;
    d) DRIVER=$OPTARG ;;
    n) N=$OPTARG ;;
    h) grep '^#' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) exit 2 ;;
  esac
done

curl -fsS "$URL/healthz" >/dev/null || { echo "daemon not reachable at $URL" >&2; exit 1; }

bench() { # bench <label> <command-producing-one-iteration>
  local label=$1; shift
  echo "$label ($N iterations)"
  local total=0 best=999999 worst=0 ms t0 t1
  for i in $(seq 1 "$N"); do
    t0=$(date +%s%N)
    "$@" || { echo "  run $i FAILED" >&2; return 1; }
    t1=$(date +%s%N)
    ms=$(( (t1 - t0) / 1000000 ))
    total=$((total + ms))
    [ "$ms" -lt "$best" ] && best=$ms
    [ "$ms" -gt "$worst" ] && worst=$ms
    printf "  run %2d: %5d ms\n" "$i" "$ms"
  done
  printf "  avg %d ms | best %d ms | worst %d ms\n\n" $((total / N)) "$best" "$worst"
}

boot_iter() {
  local id
  id=$(curl -fsS -X POST "$URL/v1/sandboxes" \
        -d "{\"driver\":\"$DRIVER\",\"spec\":{}}" | sed -E 's/.*"id":"([^"]+)".*/\1/')
  curl -fsS -X POST "$URL/v1/sandboxes/$id/exec" -d '{"cmd":["true"]}' >/dev/null
}

FORK_DIR=$(mktemp -d)
fork_iter() {
  local id
  id=$(curl -fsS -X POST "$URL/v1/sandboxes/$TEMPLATE/fork" \
        -d "{\"path\":\"$FORK_DIR/snap\"}" | sed -E 's/.*"id":"([^"]+)".*/\1/')
  curl -fsS -X POST "$URL/v1/sandboxes/$id/exec" -d '{"cmd":["true"]}' >/dev/null
}

echo "driver=$DRIVER"
bench "cold start: full boot (create -> exec)" boot_iter

restore_iter() {
  local id
  id=$(curl -fsS -X POST "$URL/v1/sandboxes/restore" \
        -d "{\"driver\":\"$DRIVER\",\"path\":\"$FORK_DIR/tpl\"}" | sed -E 's/.*"id":"([^"]+)".*/\1/')
  curl -fsS -X POST "$URL/v1/sandboxes/$id/exec" -d '{"cmd":["true"]}' >/dev/null
}

# Fork path: clone a prewarmed template (snapshot + restore, no kernel boot).
# MEM env overrides template memory (MiB, default 256): restore latency is
# dominated by reading the snapshot memory file, so it scales with this.
MEM=${MEM:-256}
TEMPLATE=$(curl -fsS -X POST "$URL/v1/sandboxes" \
  -d "{\"driver\":\"$DRIVER\",\"spec\":{\"MemMiB\":$MEM}}" | sed -E 's/.*"id":"([^"]+)".*/\1/')
echo "(template memory: ${MEM}MiB)"
if [ -n "$TEMPLATE" ]; then
  bench "warm start: fork (snapshot+restore -> exec)" fork_iter \
    || echo "(fork benchmark failed; driver may not support Restore)"

  # Hot path: template snapshotted ONCE, then restore-only per session.
  # Each clone gets its own vsock path (per-clone snapshot overlay).
  curl -fsS -X POST "$URL/v1/sandboxes/$TEMPLATE/snapshot" \
    -d "{\"path\":\"$FORK_DIR/tpl\"}" >/dev/null \
    && bench "hot start: restore-only from cached snapshot (-> exec)" restore_iter \
    || echo "(restore benchmark failed)"

  # Concurrency: N clones restored from one template in parallel.
  echo "concurrent restore: $N clones in parallel"
  t0=$(date +%s%N)
  for i in $(seq 1 "$N"); do restore_iter & done
  FAIL=0; for j in $(jobs -p); do wait "$j" || FAIL=1; done
  t1=$(date +%s%N)
  if [ "$FAIL" = 0 ]; then
    ms=$(( (t1 - t0) / 1000000 ))
    printf "  %d clones ready in %d ms total (%d ms/clone effective)\n" "$N" "$ms" $((ms / N))
  else
    echo "  FAILED (check daemon.log)"
  fi
fi
rm -rf "$FORK_DIR"

# The benchmark creates many sandboxes and never deletes them inside the
# timed loops (a delete would skew latency). Sweep them all now so no
# cloud-hypervisor process is left running after the run.
echo
echo "cleanup: deleting all benchmark sandboxes"
IDS=$(curl -fsS "$URL/v1/sandboxes" | grep -o '"id":"[^"]*"' | sed -E 's/"id":"([^"]+)"/\1/')
COUNT=0
for id in $IDS; do
  curl -fsS -X DELETE "$URL/v1/sandboxes/$id" >/dev/null 2>&1 && COUNT=$((COUNT + 1))
done
echo "  deleted $COUNT sandboxes"
