#!/usr/bin/env bash
# One-shot real-KVM end-to-end validation for vessel. Run on Ubuntu with /dev/kvm.
#
#   ./scripts/kvm-e2e.sh          # full run: deps -> images -> e2e -> benchmark
#   ./scripts/kvm-e2e.sh -s       # skip image rebuild if vmlinux/rootfs.img exist
#
# Everything happens under ./e2e-work/. Paste the full output back if it fails.
set -uo pipefail

SKIP_IMAGES=0
[ "${1:-}" = "-s" ] && SKIP_IMAGES=1

REPO=$(cd "$(dirname "$0")/.." && pwd)
WORK="$REPO/e2e-work"
mkdir -p "$WORK"
cd "$WORK"

step() { echo; echo "===== $* ====="; }
die()  { echo "FAIL: $*" >&2; exit 1; }

step "0. environment"
[ -e /dev/kvm ] || die "/dev/kvm not found (enable KVM / check permissions: sudo usermod -aG kvm \$USER)"
[ -r /dev/kvm ] && [ -w /dev/kvm ] || die "no rw access to /dev/kvm (log out/in after usermod -aG kvm)"
command -v go >/dev/null || die "go toolchain not installed"
uname -a

step "1. cloud-hypervisor binary"
# Kill leftovers from previous runs first: a running cloud-hypervisor keeps
# its binary busy (ETXTBSY) and stale VMs hold /tmp/vessel-ch sockets.
pkill -f 'vessel serve' 2>/dev/null
pkill -f 'cloud-hypervisor --api-socket /tmp/vessel-ch' 2>/dev/null
sleep 0.5

if ! command -v cloud-hypervisor >/dev/null; then
  if [ ! -x "$WORK/cloud-hypervisor" ]; then
    ARCH=$(uname -m)
    case $ARCH in
      x86_64)  CH_ASSET=cloud-hypervisor-static ;;
      aarch64) CH_ASSET=cloud-hypervisor-static-aarch64 ;;
      *) die "unsupported arch $ARCH" ;;
    esac
    echo "downloading cloud-hypervisor v45.0..."
    curl -fsSL -o cloud-hypervisor.tmp \
      "https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/v45.0/${CH_ASSET}" \
      || die "download failed; install manually and put on PATH"
    chmod +x cloud-hypervisor.tmp
    mv -f cloud-hypervisor.tmp cloud-hypervisor   # atomic; never writes a busy binary
  else
    echo "reusing existing $WORK/cloud-hypervisor"
  fi
  export PATH="$WORK:$PATH"
fi
cloud-hypervisor --version

step "2. build vessel"
(cd "$REPO" && go build -o "$WORK/vessel" ./cmd/vessel) || die "go build"

step "3. guest images"
if [ "$SKIP_IMAGES" = 1 ] && [ -f vmlinux ]; then
  echo "reusing existing vmlinux"
else
  bash "$REPO/images/build-kernel.sh" -o vmlinux || die "kernel"
fi
# rootfs embeds vessel-agent, which changes with the source: always rebuild.
bash "$REPO/images/build-rootfs.sh" -o rootfs.img || die "rootfs"
ls -lh vmlinux rootfs.img

step "4. start daemon"
pkill -f 'vessel serve' 2>/dev/null
pkill -f 'cloud-hypervisor --api-socket /tmp/vessel-ch' 2>/dev/null
sleep 0.5
rm -rf /tmp/vessel-ch   # clear stale instance dirs so log dumps are current
VESSEL_KERNEL="$WORK/vmlinux" VESSEL_ROOTFS="$WORK/rootfs.img" \
  ./vessel serve -addr :7070 > daemon.log 2>&1 &
DAEMON=$!
trap 'kill $DAEMON 2>/dev/null' EXIT
sleep 1
curl -fsS localhost:7070/healthz >/dev/null || { cat daemon.log; die "daemon not healthy"; }

dump_logs() {
  echo "--- daemon.log ---"; cat daemon.log 2>/dev/null
  for d in /tmp/vessel-ch/*/; do
    echo "--- $d vmm.log ---";    tail -30 "$d/vmm.log" 2>/dev/null
    echo "--- $d serial.log ---"; tail -40 "$d/serial.log" 2>/dev/null
  done
}

step "5. e2e: create microVM + exec"
HTTP=$(curl -sS -w '%{http_code}' -o create.json -X POST localhost:7070/v1/sandboxes \
  -d '{"driver":"cloudhypervisor","spec":{}}')
echo "HTTP $HTTP: $(cat create.json)"
[ "$HTTP" = 200 ] || { dump_logs; die "create sandbox (HTTP $HTTP)"; }
ID=$(sed -E 's/.*"id":"([^"]+)".*/\1/' create.json)

HTTP=$(curl -sS -w '%{http_code}' -o exec.json -X POST "localhost:7070/v1/sandboxes/$ID/exec" \
  -d '{"cmd":["sh","-c","echo guest-ok $(uname -r) pid=$$"]}')
echo "exec HTTP $HTTP: $(cat exec.json)"
[ "$HTTP" = 200 ] && grep -q guest-ok exec.json || { dump_logs; die "exec (HTTP $HTTP)"; }

step "6. e2e: snapshot + fork"
curl -fsS -X POST "localhost:7070/v1/sandboxes/$ID/snapshot" \
  -d "{\"path\":\"$WORK/snap-test\"}" || die "snapshot"
FORK=$(curl -fsS -X POST "localhost:7070/v1/sandboxes/$ID/fork" \
  -d "{\"path\":\"$WORK/snap-fork\"}") || die "fork"
echo "fork: $FORK"
CLONE=$(echo "$FORK" | sed -E 's/.*"id":"([^"]+)".*/\1/')
curl -fsS -X POST "localhost:7070/v1/sandboxes/$CLONE/exec" \
  -d '{"cmd":["echo","clone-ok"]}' | grep -q clone-ok || die "exec in clone"
echo "clone exec OK"

step "7. cold-start benchmark"
bash "$REPO/bench/coldstart.sh" -u http://localhost:7070 -d cloudhypervisor -n 10

step "ALL PASSED"
