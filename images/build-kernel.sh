#!/usr/bin/env bash
# Obtain a guest kernel for vessel microVMs (Cloud Hypervisor driver).
#
# IMPORTANT: Cloud Hypervisor only supports virtio-PCI. Kernels built for
# Firecracker (virtio-MMIO) will panic with "Cannot open root device vda".
# Both modes below produce CH-compatible kernels with virtio-PCI built in.
#
# Modes:
#   download (default) -- fetch the official prebuilt kernel from the
#     cloud-hypervisor/linux GitHub releases (built with ch_defconfig).
#   build (-b) -- clone the cloud-hypervisor/linux fork and compile with
#     `make ch_defconfig`. Needs: build-essential flex bison libssl-dev
#     libelf-dev bc.
#
# Usage:
#   ./build-kernel.sh [-a x86_64|aarch64] [-o vmlinux]           # download
#   ./build-kernel.sh -b [-a x86_64|aarch64] [-j N] [-o vmlinux] # build
set -euo pipefail

ARCH=$(uname -m)
OUT=vmlinux
MODE=download
JOBS=$(nproc 2>/dev/null || echo 4)
CH_RELEASE=${CH_RELEASE:-ch-release-v6.12.8-20250613}
CH_KERNEL_BRANCH=${CH_KERNEL_BRANCH:-ch-6.12.8}

while getopts "a:o:bj:h" opt; do
  case $opt in
    a) ARCH=$OPTARG ;;
    o) OUT=$OPTARG ;;
    b) MODE=build ;;
    j) JOBS=$OPTARG ;;
    h) grep '^#' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) exit 2 ;;
  esac
done
case $ARCH in
  x86_64|aarch64) ;;
  arm64) ARCH=aarch64 ;;
  *) echo "unsupported arch: $ARCH" >&2; exit 1 ;;
esac

if [ "$MODE" = download ]; then
  case $ARCH in
    x86_64)  ASSET=vmlinux-x86_64 ;;
    aarch64) ASSET=Image-arm64 ;;
  esac
  URL="https://github.com/cloud-hypervisor/linux/releases/download/${CH_RELEASE}/${ASSET}"
  echo ">> downloading official Cloud Hypervisor kernel (${CH_RELEASE}, ${ARCH})"
  echo "   $URL"
  curl -fSL --progress-bar "$URL" -o "$OUT"
  echo "done: $OUT ($(du -h "$OUT" | cut -f1))"
  exit 0
fi

# ---- build mode -----------------------------------------------------------
command -v make >/dev/null && command -v gcc >/dev/null || {
  echo "build mode needs: sudo apt install build-essential flex bison libssl-dev libelf-dev bc" >&2
  exit 1
}

WORK=${KERNEL_WORKDIR:-"$PWD/linux-cloud-hypervisor"}
if [ ! -d "$WORK" ]; then
  echo ">> cloning cloud-hypervisor/linux branch $CH_KERNEL_BRANCH (shallow)"
  git clone --depth 1 -b "$CH_KERNEL_BRANCH" \
    https://github.com/cloud-hypervisor/linux.git "$WORK"
fi
cd "$WORK"

if [ "$ARCH" = x86_64 ]; then
  echo ">> make ch_defconfig && make vmlinux -j$JOBS"
  make ch_defconfig
  CFLAGS="-Wa,-mx86-used-note=no" make vmlinux -j"$JOBS"
  cp vmlinux "$OUT"
else
  echo ">> ARCH=arm64 make ch_defconfig && make Image -j$JOBS"
  ARCH=arm64 make ch_defconfig
  ARCH=arm64 make Image -j"$JOBS"
  cp arch/arm64/boot/Image "$OUT"
fi
echo "done: $OUT"
