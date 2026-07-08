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

```bash
go build ./cmd/vessel

# 开发模式：namespace 沙箱（Linux，无需 KVM）
./vessel run -- sh -c 'echo hello from PID $$'

# API 守护进程
./vessel serve -addr :7070
```

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
- [x] M3.5a guest 镜像构建脚本（内核 + Alpine/vessel-agent rootfs）、冷启动 benchmark 脚本
- [x] M3.5b KVM 真机 e2e 全链路通过（create/exec/snapshot/fork/clone-exec）
- [ ] M3.5c fork 路径冷启动优化与对比数据（vs Kata/E2B/microsandbox）
- [ ] M4 containerd shim v2 + K8s RuntimeClass，多 fork 的 vsock socket 重映射，发布 v0.1
