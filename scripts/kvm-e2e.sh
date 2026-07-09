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
# Under sudo, root's PATH usually lacks the user's Go toolchain. Locate it,
# including via common install dirs and the invoking user's login shell.
GO=$(command -v go || true)
if [ -z "$GO" ]; then
  for c in /usr/local/go/bin/go /snap/bin/go /snap/go/current/bin/go "$HOME/go-toolchain/go/bin/go"; do
    [ -x "$c" ] && GO=$c && break
  done
fi
if [ -z "$GO" ] && [ -n "${SUDO_USER:-}" ]; then
  GO=$(sudo -u "$SUDO_USER" bash -lc 'command -v go' 2>/dev/null || true)
fi
[ -n "$GO" ] || die "go toolchain not found (install go, or run without sudo where go is on PATH)"
echo "go: $GO"
uname -a

step "1. cloud-hypervisor binary"
# Kill leftovers from previous runs first: a running cloud-hypervisor keeps
# its binary busy (ETXTBSY) and stale VMs hold /tmp/vessel-ch sockets.
pkill -f 'vessel serve' 2>/dev/null
pkill -f 'cloud-hypervisor --api-socket /tmp/vessel-ch' 2>/dev/null
sleep 0.5

CH_VERSION=${CH_VERSION:-v52.0}
# Drop a cached binary that doesn't match the pinned version.
if [ -x "$WORK/cloud-hypervisor" ] && ! "$WORK/cloud-hypervisor" --version 2>/dev/null | grep -q "${CH_VERSION#v}"; then
  echo "cached cloud-hypervisor is not $CH_VERSION; refreshing"
  rm -f "$WORK/cloud-hypervisor"
fi

if ! command -v cloud-hypervisor >/dev/null; then
  if [ ! -x "$WORK/cloud-hypervisor" ]; then
    ARCH=$(uname -m)
    case $ARCH in
      x86_64)  CH_ASSET=cloud-hypervisor-static ;;
      aarch64) CH_ASSET=cloud-hypervisor-static-aarch64 ;;
      *) die "unsupported arch $ARCH" ;;
    esac
    echo "downloading cloud-hypervisor ${CH_VERSION}..."   # v52+ needed for OnDemand restore
    curl -fsSL -o cloud-hypervisor.tmp \
      "https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/${CH_VERSION}/${CH_ASSET}" \
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
(cd "$REPO" && "$GO" build -o "$WORK/vessel" ./cmd/vessel) || die "go build"

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
HTTP=$(curl -sS -w '%{http_code}' -o snap.json -X POST "localhost:7070/v1/sandboxes/$ID/snapshot" \
  -d "{\"path\":\"$WORK/snap-test\"}")
echo "snapshot HTTP $HTTP: $(cat snap.json)"
[ "$HTTP" = 200 ] || { dump_logs; die "snapshot (HTTP $HTTP)"; }

HTTP=$(curl -sS -w '%{http_code}' -o fork.json -X POST "localhost:7070/v1/sandboxes/$ID/fork" \
  -d "{\"path\":\"$WORK/snap-fork\"}")
echo "fork HTTP $HTTP: $(cat fork.json)"
[ "$HTTP" = 200 ] || { dump_logs; die "fork (HTTP $HTTP)"; }
CLONE=$(sed -E 's/.*"id":"([^"]+)".*/\1/' fork.json)

HTTP=$(curl -sS -w '%{http_code}' -o clone-exec.json -X POST "localhost:7070/v1/sandboxes/$CLONE/exec" \
  -d '{"cmd":["echo","clone-ok"]}')
echo "clone exec HTTP $HTTP: $(cat clone-exec.json)"
[ "$HTTP" = 200 ] && grep -q clone-ok clone-exec.json || { dump_logs; die "exec in clone (HTTP $HTTP)"; }

step "7. cold-start benchmark"
bash "$REPO/bench/coldstart.sh" -u http://localhost:7070 -d cloudhypervisor -n 10

step "8. shim test suite under race detector"
(cd "$REPO" && "$GO" test -race -count=1 ./pkg/shim/) || die "shim tests"

step "9. rootfs->block-image microVM (a directory rootfs booted as a VM)"
# Unpack the prebuilt rootfs image into a directory, then create a sandbox
# with that DIRECTORY as rootfs. The CH driver must pack it into a block
# image and boot it — the real path a containerd bundle rootfs takes.
ROOTDIR="$WORK/bundle-rootfs"
rm -rf "$ROOTDIR"; mkdir -p "$ROOTDIR"
# rootfs.img was built in step 3; mount read-only to copy its tree out.
MNT=$(mktemp -d)
if mount -o loop,ro "$WORK/rootfs.img" "$MNT" 2>/dev/null; then
  cp -a "$MNT"/. "$ROOTDIR"/ 2>/dev/null || true
  umount "$MNT"
  rmdir "$MNT"
  HTTP=$(curl -sS -w '%{http_code}' -o dir.json -X POST localhost:7070/v1/sandboxes \
    -d "{\"driver\":\"cloudhypervisor\",\"spec\":{\"Rootfs\":\"$ROOTDIR\"}}")
  echo "dir-rootfs create HTTP $HTTP: $(cat dir.json)"
  [ "$HTTP" = 200 ] || { dump_logs; die "directory-rootfs microVM (HTTP $HTTP)"; }
  DID=$(sed -E 's/.*"id":"([^"]+)".*/\1/' dir.json)
  curl -fsS -X POST "localhost:7070/v1/sandboxes/$DID/exec" \
    -d '{"cmd":["sh","-c","echo packed-rootfs-ok"]}' | grep -q packed-rootfs-ok \
    || { dump_logs; die "exec in directory-rootfs microVM"; }
  echo "directory rootfs -> block image -> booted microVM -> exec OK"
else
  echo "(skip: loop mount unavailable; pkg/image unit tests still cover packing)"
  rmdir "$MNT" 2>/dev/null || true
fi

step "ALL PASSED"
