# vessel（暂定名）

Agent 原生沙箱运行时：底层复用成熟 VMM（Cloud Hypervisor，驱动可插拔），上层三面朝外——
**Kubernetes**（containerd shim v2 + RuntimeClass）、**AI 应用**（原生 REST + Python SDK）、
**E2B SDK**（drop-in 迁移）。核心卖点是快照恢复：一个预热好的模板沙箱恢复出 N 个会话、
跳过内核启动。定位与竞品分析（含腾讯 CubeSandbox 对比）见仓库外的《sandbox-runtime-分析报告.md》。

## 差异化定位

快启动如今是赛道标配（CubeSandbox 等都做到了），vessel 竞争在"**在哪跑、怎么跑**"：

- **Kubernetes 原生**：一个 containerd shim，`runtimeClassName: vessel` 一行进集群——
  而非另起一套平行编排栈。pod 注解 `vessel.dev/template=<id>` 让 pod 从缓存快照**恢复**
  （数十毫秒）而非启动，这是目前没有任何开源运行时提供的能力。
- **活体 fork**：对一个正在运行、已加载会话状态的沙箱做分叉（Agent 多分支探索），
  不只是从干净模板克隆。
- **单静态二进制自托管**：`vessel up` 一条命令从裸机到可用，无 Docker / 数据库依赖；
  x86_64 + arm64 双架构。

## 实测性能（Ubuntu 24.04 / KVM / CH v52，n=10）

| 路径 | 128MiB | 256MiB | 说明 |
|---|---|---|---|
| 完整启动（boot + exec） | 524ms | 529ms | 与 Kata 同量级 |
| **restore-only（会话主路径）** | **79ms** | 137ms | OnDemand 缺页 + VMM 池 |
| 并发 10 clone 全部就绪 | — | 173ms（17ms/clone） | per-clone 快照覆盖层 |

restore 延迟与模板内存解耦（userfaultfd 按需缺页），并发靠硬链接快照覆盖层 + per-clone vsock。

## 组件

- **核心域**（`pkg/sandbox`）：Spec / Instance / Driver / Restorer 接口，Manager 生命周期
  （create / delete / snapshot / fork / restore）与优雅 shutdown（reap 所有 VMM）
- **Cloud Hypervisor 驱动**（`pkg/driver/cloudhypervisor`）：microVM 生命周期、hybrid vsock、
  OnDemand restore、VMM 预启动池、OCI rootfs→块镜像自动打包、pod 网络
- **process 驱动**（`pkg/driver/process`）：namespace 隔离，开发/无 KVM 降级用
- **guest agent**（`pkg/agent`，即 vessel 二进制的 `agent` 子命令）：vsock 上的 exec / 文件 / 配网
- **containerd shim v2**（`pkg/shim` + `cmd/containerd-shim-vessel-v1`）：Task service、
  start/delete 握手、TaskExit 事件、模板注解恢复、非交互 exec
- **网络**（`pkg/vmnet`）：CNI netns 内 tc-mirror TAP↔veth，guest 采用 pod IP
- **镜像**（`pkg/image`）：目录 rootfs → erofs/ext4 块镜像
- **REST API**（`pkg/api`）：create / list / exec / delete / snapshot / fork / restore
- **E2B 兼容层**（`pkg/e2b`）：E2B 控制面 API（drop-in）
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

单元测试用 mock 覆盖 VMM 交互（含 SCM_RIGHTS fd 传递等）；真机验证用
`scripts/kvm-e2e.sh`（KVM，10 步含联网 pod）和 `scripts/ctr-e2e.sh`（真 containerd）。

## 路线图

- [x] **v0.1** — sub-100ms 会话恢复：核心域 + CH 驱动 + vsock agent +
  snapshot/restore/fork + REST + Python SDK；KVM 真机验证 79ms 恢复
- [x] **v0.2** — 内存解耦恢复（userfaultfd 按需缺页）+ 并发 clone（17ms/clone）+
  VMM 预启动池 + `vessel up` 一键引导（双架构、无 KVM 降级）
- [x] **v0.2.x** — E2B 兼容控制面（SDK drop-in 迁移）
- [x] **v0.3.0** — containerd shim v2 + K8s RuntimeClass：Task service、
  start/delete 握手 + TaskExit 事件（真 containerd 验收）、OCI rootfs→块镜像、
  非交互 exec（`kubectl exec`）、CNI pod 网络（tc-mirror）、干净的 VMM 生命周期
  （DELETE 端点 + 优雅 shutdown + Pdeathsig 兜底）
- [x] **v0.3.1** — 联网 pod 走池化 restore（方案 B）：模板 pod 从池取通用 VMM +
  restore，恢复后在 pod netns 内打开 TAP fd（fd 超越 namespace）并经 SCM_RIGHTS
  用 `vm.add-net` 热插 NIC，guest 采用 pod IP。对无网卡模板也成立，联网 pod
  同享 <100ms 恢复。（非模板 pod 仍走 create+boot 的 spawn-in-netns，正确但非快路径）
- [ ] 真集群 `kubectl run/exec/delete` e2e、E2B envd 数据面 gRPC 兼容、erofs 镜像分层

## AI 参与说明

本项目由 [Claude](https://claude.com)（Claude Fable 5）在人类主导下结对开发：
架构决策、真机验证与最终审核由维护者完成，代码实现与测试由 AI 编写。
相关提交带有 `Co-Authored-By: Claude Fable 5` trailer。
