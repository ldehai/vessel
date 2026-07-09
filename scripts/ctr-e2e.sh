#!/usr/bin/env bash
# Real-containerd handshake e2e: containerd itself launches the vessel shim
# via the Runtime v2 start/delete handshake and drives the task lifecycle.
# Run on a host with containerd installed (sudo apt install containerd).
#
#   sudo ./scripts/ctr-e2e.sh
#
# Honest scope note: with no template annotation the shim creates a
# process-driver sandbox but does NOT yet execute the container's OCI
# command (that lands with rootfs conversion + exec support). What this
# validates is the part no unit test can: real containerd spawning our shim
# binary, reading its address from stdout, driving Create/Start/State/Kill/
# Delete over the handshake socket, and receiving our TaskExit events.
set -euo pipefail

RUNTIME=io.containerd.vessel.v1
IMAGE=docker.io/library/busybox:latest
TASK=vessel-ctr-e2e

die() { echo "FAIL: $*" >&2; exit 1; }
step() { echo; echo "===== $* ====="; }

step "0. environment"
[ "$(id -u)" = 0 ] || die "run as root (containerd socket + shim install)"
command -v ctr >/dev/null || die "ctr not found (sudo apt install containerd)"
systemctl is-active --quiet containerd || die "containerd is not running"

REPO=$(cd "$(dirname "$0")/.." && pwd)

step "1. build + install shim"
# Running under sudo, root's PATH usually lacks the user's Go toolchain.
# Prefer a prebuilt binary in the repo; otherwise locate go (including via
# the invoking user's shell).
BIN="$REPO/containerd-shim-vessel-v1"
if [ ! -x "$BIN" ] || [ -n "${REBUILD:-}" ]; then
  GO=$(command -v go || true)
  if [ -z "$GO" ]; then
    for c in /usr/local/go/bin/go /snap/bin/go /snap/go/current/bin/go; do
      [ -x "$c" ] && GO=$c && break
    done
  fi
  if [ -z "$GO" ] && [ -n "${SUDO_USER:-}" ]; then
    GO=$(sudo -u "$SUDO_USER" bash -lc 'command -v go' 2>/dev/null || true)
  fi
  [ -n "$GO" ] || die "go not found under sudo; pre-build as your user first:
  CGO_ENABLED=0 go build -o containerd-shim-vessel-v1 ./cmd/containerd-shim-vessel-v1"
  (cd "$REPO" && CGO_ENABLED=0 "$GO" build -o "$BIN" ./cmd/containerd-shim-vessel-v1) || die "build"
fi
install -m 0755 "$BIN" /usr/local/bin/containerd-shim-vessel-v1
echo "installed /usr/local/bin/containerd-shim-vessel-v1"

step "2. pull image"
ctr image pull "$IMAGE" >/dev/null || die "image pull"

step "3. run task via the vessel runtime (detached)"
# no --rm: it conflicts with -d on some ctr versions; cleanup is explicit below
ctr run -d --runtime "$RUNTIME" "$IMAGE" "$TASK" || die "ctr run"
trap 'ctr task kill -s KILL "$TASK" 2>/dev/null; ctr container rm "$TASK" 2>/dev/null' EXIT

step "4. containerd sees the task RUNNING"
sleep 0.5
ctr task ls | tee /dev/stderr | grep -E "^$TASK .*RUNNING" >/dev/null \
  || die "task not RUNNING in ctr task ls"

step "4b. ctr task exec runs a command in the sandbox"
EXEC_OUT=$(ctr task exec --exec-id smoke1 "$TASK" /bin/echo vessel-exec-ok 2>&1)
echo "$EXEC_OUT"
echo "$EXEC_OUT" | grep -q vessel-exec-ok || die "ctr task exec did not return command output"

step "5. kill -> containerd receives exit via our TaskExit event"
ctr task kill -s TERM "$TASK" || die "task kill"
for i in $(seq 1 50); do
  STATE=$(ctr task ls | awk -v t="$TASK" '$1==t {print $3}')
  [ "$STATE" = "STOPPED" ] && break
  sleep 0.1
done
[ "${STATE:-}" = "STOPPED" ] || die "task never reached STOPPED (state=${STATE:-gone})"

step "6. delete"
ctr task rm "$TASK" 2>/dev/null || true
ctr container rm "$TASK" || die "container rm"
trap - EXIT

step "ALL PASSED — containerd drove the vessel shim end to end"
