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

echo "cold start: driver=$DRIVER, $N iterations (create -> exec 'true')"
total=0 best=999999 worst=0
for i in $(seq 1 "$N"); do
  t0=$(date +%s%N)
  id=$(curl -fsS -X POST "$URL/v1/sandboxes" \
        -d "{\"driver\":\"$DRIVER\",\"spec\":{}}" | sed -E 's/.*"id":"([^"]+)".*/\1/')
  curl -fsS -X POST "$URL/v1/sandboxes/$id/exec" -d '{"cmd":["true"]}' >/dev/null
  t1=$(date +%s%N)
  ms=$(( (t1 - t0) / 1000000 ))
  total=$((total + ms))
  [ "$ms" -lt "$best" ] && best=$ms
  [ "$ms" -gt "$worst" ] && worst=$ms
  printf "  run %2d: %5d ms\n" "$i" "$ms"
done
echo "----"
printf "avg %d ms | best %d ms | worst %d ms\n" $((total / N)) "$best" "$worst"
