#!/usr/bin/env bash
# Obtain a guest kernel (vmlinux) for vessel microVMs.
#
# Two modes:
#   download (default) -- fetch a prebuilt PVH-bootable vmlinux from the
#     Firecracker CI artifact bucket. Works for Cloud Hypervisor too (both
#     boot uncompressed vmlinux with virtio). Fastest way to get running.
#   build -- clone Cloud Hypervisor's maintained kernel branch and compile
#     with their recommended config. Use this for production images.
#
# Usage:
#   ./build-kernel.sh [-a x86_64|aarch64] [-o vmlinux]           # download
#   ./build-kernel.sh -b [-a x86_64|aarch64] [-j N] [-o vmlinux] # build
set -euo pipefail

ARCH=$(uname -m)
OUT=vmlinux
MODE=download
JOBS=$(nproc 2>/dev/null || echo 4)
# Firecracker CI kernel line known to work with CH; bump as needed.
FC_KERNEL_VER=${FC_KERNEL_VER:-6.1.102}
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
  # Firecracker CI publishes vmlinux images per kernel line and arch.
  MAJOR_MINOR=$(echo "$FC_KERNEL_VER" | cut -d. -f1-2)
  URL="https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.10/${ARCH}/vmlinux-${FC_KERNEL_VER}"
  echo ">> downloading prebuilt vmlinux ${FC_KERNEL_VER} (${ARCH})"
  echo "   $URL"
  curl -fSL --progress-bar "$URL" -o "$OUT"
  echo "done: $OUT ($(du -h "$OUT" | cut -f1))"
  echo "note: prebuilt kernel line ${MAJOR_MINOR} is for quick starts;"
  echo "      use '-b' to build Cloud Hypervisor's ${CH_KERNEL_BRANCH} for production."
  exit 0
fi

# ---- build mode -----------------------------------------------------------
command -v make >/dev/null && command -v gcc >/dev/null || {
  echo "build mode needs make/gcc/flex/bison/libelf-dev/libssl-dev" >&2; exit 1; }

WORK=${KERNEL_WORKDIR:-"$PWD/linux-cloud-hypervisor"}
if [ ! -d "$WORK" ]; then
  echo ">> cloning cloud-hypervisor kernel branch $CH_KERNEL_BRANCH (shallow)"
  git clone --depth 1 -b "$CH_KERNEL_BRANCH" \
    https://github.com/cloud-hypervisor/linux.git "$WORK"
fi
cd "$WORK"

echo ">> applying cloud-hypervisor guest config"
if [ "$ARCH" = x86_64 ]; then
  CONF_URL="https://raw.githubusercontent.com/cloud-hypervisor/cloud-hypervisor/main/resources/linux-config-x86_64"
  KIMG=vmlinux
else
  CONF_URL="https://raw.githubusercontent.com/cloud-hypervisor/cloud-hypervisor/main/resources/linux-config-aarch64"
  KIMG=arch/arm64/boot/Image
fi
curl -fsSL "$CONF_URL" -o .config
make olddefconfig

echo ">> building (-j$JOBS)"
make -j"$JOBS" ${ARCH:+ARCH=$([ "$ARCH" = aarch64 ] && echo arm64 || echo x86_64)}
cp "$KIMG" "$OUT" 2>/dev/null || cp vmlinux "$OUT"
echo "done: $OUT"
