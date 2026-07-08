# vessel（暂定名）

Agent 原生沙箱运行时：底层复用成熟 VMM（Cloud Hypervisor，驱动可插拔），上层同时面向
K8s（后续 containerd shim）和 AI 应用（REST API + SDK），核心卖点是快照 fork——
一个预热好的模板沙箱克隆出 N 个会话，跳过内核启动，冷启动目标 <100ms。
定位与路线图见仓库外的《sandbox-runtime-分析报告.md》。

## 当前状态

M1~M3 已实现：

- **核心域**（`pkg/sandbox`）：Spec / Instance / Driver / Restorer 接口，Manager 生命周期与 fork 语义
- **process 驱动**（`pkg/driver/process`）：Linux user+pid+mnt+uts+ipc namespace，开发测试用，非生产隔离
- **Cloud Hypervisor 驱动**（`pkg/driver/cloudhypervisor`）：REST API 客户端、microVM 启动、
  hybrid vsock（CONNECT/OK 握手）连接 guest agent、pause+snapshot、restore+resume
- **guest agent**（`pkg/agent` + `cmd/vessel-agent`）：JSON-over-vsock 协议，exec / 文件读写，
  作为 guest init 运行
- **REST API**（`pkg/api`）：create / list / exec / snapshot / fork
- **Python SDK**（`sdk/python/vessel.py`）：零依赖客户端

## 快速开始

一条命令，从裸机到可用的沙箱 daemon（对比某些竞品的 Docker + MySQL + Redis + 七组件）：

```bash
CGO_ENABLED=0 go build ./cmd/vessel && ./vessel up
```

`vessel up` 会：检测 KVM 和 CPU 架构（x86_64/arm64 都支持）→ 自动下载
cloud-hypervisor 和官方 guest 内核 → 用 Alpine + 自身二进制构建 rootfs
（vessel 二进制同时就是 guest agent，`vessel agent` 子命令）→ 启动 API
并打印可直接复制的示例。资产缓存在 `~/.vessel`，第二次启动瞬时完成。
没有 KVM 的机器自动降级到 process 驱动，API 照常可用。

其他命令：

```bash
./vessel run -- sh -c 'echo hello from PID $$'   # 一次性沙箱执行
./vessel serve -addr :7070                        # 只起 daemon（资产路径用环境变量）
```

### E2B SDK 直接迁移

daemon 同时提供 E2B 兼容的控制面 API（`/sandboxes`）。E2B SDK 用户改两个
环境变量就能指向自托管的 vessel，业务代码零改动：

```bash
export E2B_API_URL="http://localhost:7070"
export E2B_API_KEY="local"
```

templateID 映射：注册过的模板（`RegisterTemplate`）走 vessel 的快速恢复路径
（<100ms）；`"base"` 或未知 templateID 则新建沙箱。当前覆盖 E2B **控制面**
（创建/列表/kill，字段与状态码对齐 E2B OpenAPI）；数据面（envd 的文件/进程
gRPC）用 vessel 原生 `/v1/sandboxes/{id}/exec`，envd gRPC 兼容为后续项。

```python
import sys; sys.path.insert(0, "sdk/python")
from vessel import VesselClient

v = VesselClient("http://localhost:7070")
sb = v.create(driver="process")            # 或 "cloudhypervisor"
print(sb.exec(["python3", "-c", "print(42)"]).stdout)
clone = sb.fork("/var/lib/vessel/snap-1")  # VM 驱动限定
```

microVM 模式需要 Linux + KVM。guest 内核和 rootfs 用 `images/` 下的脚本构建（见
`images/README.md`）：

```bash
cd images && ./build-kernel.sh -o vmlinux && ./build-rootfs.sh -o rootfs.img
VESSEL_KERNEL=$PWD/vmlinux VESSEL_ROOTFS=$PWD/rootfs.img ../vessel serve
../bench/coldstart.sh   # 冷启动 benchmark
```

## 目录结构

```
cmd/vessel/                  CLI + API daemon
cmd/vessel-agent/            guest init 二进制（vsock listener）
pkg/sandbox/                 核心域模型与 Manager
pkg/agent/                   host<->guest 协议（client/server）
pkg/vsock/                   AF_VSOCK dial/listen（Linux）
pkg/driver/process/          开发驱动（namespaces）
pkg/driver/cloudhypervisor/  生产驱动（microVM）
pkg/api/                     REST API
sdk/python/                  Python SDK
```

## 测试

```bash
go test ./...
```

单元测试用 mock 覆盖 VMM 交互；真机验证用 `scripts/kvm-e2e.sh`（Linux + KVM）。

## 实测数据（2026-07，Ubuntu 24.04 x86_64 / KVM / CH v45）

256MiB 模板，n=10，avg（CH v52，OnDemand restore）：

| 路径 | v0.1 (Copy) | **v0.2 (OnDemand)** | 说明 |
|---|---|---|---|
| 完整启动（boot + 握手 + exec） | 529ms | 521ms | 与 Kata 同量级 |
| fork（snapshot+restore + exec） | 224ms | **86ms** | OnDemand + 自动 resume |
| restore-only（会话主路径） | 137ms | **70ms（best 58ms）** | 与模板内存解耦 |
| **并发 10 clone 全部就绪** | 不支持 | **173ms（17ms/clone）** | per-clone 快照覆盖层 |

v0.1 时 restore 延迟随模板内存线性增长（纯内存文件读取）；v0.2 用
CH v52 的 userfaultfd 按需缺页解耦了两者——256MiB 模板与 128MiB 同样快，
页面在首次访问时才载入。并发 clone 靠硬链接快照覆盖层 + per-clone vsock
路径实现，10 路并发摊薄到 17ms/clone。

## 路线图

- [x] M1 核心域 + process 驱动 + REST + CLI
- [x] M2 guest agent（vsock）+ Cloud Hypervisor 驱动
- [x] M3 snapshot / restore / fork + Python SDK
- [x] M3.5 guest 镜像脚本 + KVM 真机 e2e 全链路（create/exec/snapshot/fork）
- [x] v0.2 按需缺页恢复（内存解耦）+ 并发 clone + VMM 预启动池
- [x] vessel up 一键引导（双架构、无 KVM 降级、二进制自嵌为 agent）
- [x] E2B 兼容控制面（SDK drop-in 迁移）
- [~] v0.3 containerd shim v2 + K8s RuntimeClass（进行中）：Task service 映射、
  pod 注解 `vessel.dev/template` 走恢复路径（未注册模板 fail-fast）、
  **containerd 启停握手 + TaskExit 事件发布已完成并通过真机验收**
  （2026-07 Ubuntu 24.04：`scripts/ctr-e2e.sh` 中真 containerd 拉起 shim、
  ctr run→RUNNING→kill→STOPPED→rm 全链路通过）。**OCI rootfs→块镜像转换
  已完成**（`pkg/image`：erofs 优先、ext4 兜底，CH 驱动 boot 时自动打包）。
  剩：CNI 桥接、真集群 e2e（见 docs/kubernetes.md）
- [ ] E2B envd 数据面 gRPC 兼容、erofs 镜像分层
