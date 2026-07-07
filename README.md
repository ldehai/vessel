# vessel（暂定名）

Agent 原生沙箱运行时：底层复用成熟 VMM（Cloud Hypervisor / Firecracker，可插拔），
上层同时暴露 containerd shim（进 K8s）和 REST/gRPC Agent API，主打快照恢复带来的
<100ms 冷启动。定位与路线图见仓库外的《sandbox-runtime-分析报告.md》。

## 当前状态：M1 骨架

已有：核心域模型（Spec / Instance / Driver / Manager）、process 驱动
（Linux user+PID+mount+UTS+IPC namespace，开发与测试用，非生产隔离）、
REST API 雏形、CLI。

## 使用

```bash
go build ./cmd/vessel

# 在命名空间沙箱里跑命令（Linux）
./vessel run -- sh -c 'echo hello from PID $$; hostname'

# 启动 API 守护进程
./vessel serve -addr :7070
curl -X POST localhost:7070/v1/sandboxes -d '{"driver":"process","spec":{}}'
curl -X POST localhost:7070/v1/sandboxes/<id>/exec -d '{"cmd":["uname","-a"]}'
```

## 目录结构

```
cmd/vessel/          CLI 入口（run / serve / info）
pkg/sandbox/         核心域：Spec、状态机、Driver 接口、Manager
pkg/driver/process/  开发驱动：Linux namespaces（M2 将新增 cloudhypervisor/）
pkg/api/             REST API（M3 加 gRPC + 快照/fork + SDK）
```

## 路线图

M1 骨架（当前）→ M2 Cloud Hypervisor 驱动 + vsock guest-agent →
M3 snapshot/restore/fork、冷启动 <100ms、Python/TS SDK →
M4 containerd shim v2 + K8s RuntimeClass，发布 v0.1。
