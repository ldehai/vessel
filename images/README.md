# guest 镜像构建

vessel 的 microVM 模式需要两个产物：guest 内核（vmlinux）和内置 vessel-agent 的 rootfs。
两个脚本都不需要 root 权限。

```bash
# 1. 内核：先用预编译的快速起步（生产用 -b 从 CH 官方分支编译）
./build-kernel.sh -o vmlinux

# 2. rootfs：Alpine + 静态 vessel-agent 作为 init
./build-rootfs.sh -o rootfs.img            # ext4
./build-rootfs.sh -f erofs -o rootfs.img   # erofs（需 erofs-utils，生产推荐）

# 3. 启动
VESSEL_KERNEL=$PWD/vmlinux VESSEL_ROOTFS=$PWD/rootfs.img vessel serve

# 4. 跑冷启动 benchmark
../bench/coldstart.sh -d cloudhypervisor -n 10
```

前置条件：Linux + /dev/kvm、cloud-hypervisor 二进制在 PATH、curl/tar/mkfs.ext4、Go 工具链。

guest 启动流程：内核 cmdline `init=/sbin/init` → init 挂载 proc/sys/dev →
`exec /usr/bin/vessel-agent`（PID 1，vsock 5000 端口监听）→ host 侧 hybrid vsock 握手接入。
