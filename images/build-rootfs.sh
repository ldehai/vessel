#!/usr/bin/env bash
# Build a guest rootfs image for vessel microVMs.
#
# Contents: Alpine minirootfs + statically-linked vessel-agent + /sbin/init
# wrapper that mounts proc/sys/dev and execs the agent (PID 1 semantics).
#
# Output formats:
#   ext4  (default; mkfs.ext4 -d, no root privileges needed)
#   erofs (if mkfs.erofs is installed; read-only, dedup, page-cache shared
#          across VMs -- the production choice per the vessel design doc)
#
# Usage:
#   ./build-rootfs.sh [-a x86_64|aarch64] [-v 3.20.3] [-f ext4|erofs] [-o rootfs.img]
#
# No root required. Needs: curl, tar, mkfs.ext4 (e2fsprogs >= 1.43) or mkfs.erofs, go.
set -euo pipefail

ARCH=$(uname -m)
ALPINE_VER=3.20.3
FORMAT=ext4
OUT=""
SIZE_MB=128

while getopts "a:v:f:o:s:h" opt; do
  case $opt in
    a) ARCH=$OPTARG ;;
    v) ALPINE_VER=$OPTARG ;;
    f) FORMAT=$OPTARG ;;
    o) OUT=$OPTARG ;;
    s) SIZE_MB=$OPTARG ;;
    h) grep '^#' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) exit 2 ;;
  esac
done

case $ARCH in
  x86_64|aarch64) ;;
  arm64) ARCH=aarch64 ;;
  *) echo "unsupported arch: $ARCH" >&2; exit 1 ;;
esac
[ -n "$OUT" ] || OUT="rootfs-${ARCH}.${FORMAT}.img"

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
REPO_ROOT=$(cd "$SCRIPT_DIR/.." && pwd)
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
ROOT="$WORK/root"
mkdir -p "$ROOT"

echo ">> [1/4] fetching Alpine minirootfs $ALPINE_VER ($ARCH)"
MAJOR_MINOR=${ALPINE_VER%.*}
URL="https://dl-cdn.alpinelinux.org/alpine/v${MAJOR_MINOR}/releases/${ARCH}/alpine-minirootfs-${ALPINE_VER}-${ARCH}.tar.gz"
curl -fsSL "$URL" -o "$WORK/alpine.tar.gz"
tar -xzf "$WORK/alpine.tar.gz" -C "$ROOT"

echo ">> [2/4] building static vessel-agent"
GOARCH_MAP_x86_64=amd64 GOARCH_MAP_aarch64=arm64
eval "GOARCH=\$GOARCH_MAP_${ARCH}"
(cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=$GOARCH \
  go build -ldflags='-s -w' -o "$ROOT/usr/bin/vessel-agent" ./cmd/vessel-agent)

echo ">> [3/4] installing /sbin/init"
# Alpine ships /sbin/init as an absolute symlink to /bin/busybox; writing
# through it would follow the link outside the rootfs. Remove it first.
rm -f "$ROOT/sbin/init"
cat > "$ROOT/sbin/init" <<'EOF'
#!/bin/sh
# vessel guest init: minimal PID-1 duties, then become the agent.
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
mount -t proc     proc     /proc  2>/dev/null
mount -t sysfs    sysfs    /sys   2>/dev/null
mount -t devtmpfs devtmpfs /dev   2>/dev/null
mount -t tmpfs    tmpfs    /tmp   2>/dev/null
hostname vessel-guest 2>/dev/null
exec /usr/bin/vessel-agent
EOF
chmod +x "$ROOT/sbin/init"

echo ">> [4/4] packing $FORMAT image -> $OUT"
case $FORMAT in
  ext4)
    rm -f "$OUT"
    truncate -s "${SIZE_MB}M" "$OUT"
    mkfs.ext4 -q -F -d "$ROOT" "$OUT"
    ;;
  erofs)
    command -v mkfs.erofs >/dev/null || { echo "mkfs.erofs not found (apt install erofs-utils)" >&2; exit 1; }
    rm -f "$OUT"
    mkfs.erofs -zlz4 "$OUT" "$ROOT"
    ;;
  *) echo "unknown format: $FORMAT" >&2; exit 1 ;;
esac

OUT_ABS=$(readlink -f "$OUT")
echo "done: $OUT_ABS ($(du -h "$OUT" | cut -f1))"
echo "boot it with: VESSEL_ROOTFS=$OUT_ABS VESSEL_KERNEL=<vmlinux> vessel serve"
