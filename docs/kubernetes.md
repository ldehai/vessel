# Kubernetes integration (v0.3, in progress)

vessel's differentiator in Kubernetes: a pod can be **restored from a
cached template snapshot** instead of booted from scratch. Annotate a pod
with `vessel.dev/template=<id>` and the shim creates it through vessel's
restore path — session-ready in tens of milliseconds (benchmarks in the
README). No other open-source runtime maps a Kubernetes pod onto a snapshot
restore.

## Architecture

```
kubelet ──CRI──► containerd ──shim v2 (ttrpc)──► containerd-shim-vessel-v1
                                                        │
                                                  pkg/shim.Service
                                                        │ Task RPCs -> Manager
                                                        ▼
                                            vessel drivers (CH microVM / process)
```

The shim is a thin ttrpc adapter translating the containerd Task service
onto the same `sandbox.Manager` that backs vessel's REST and E2B APIs. One
engine, three front doors (K8s, native REST, E2B).

## Semantics

- **Template annotation is a contract.** `vessel.dev/template=<id>` naming a
  registered template restores from its snapshot. Naming an *unregistered*
  template fails `Create` with NotFound — the shim never silently serves a
  cold boot where a warm restore was requested. No annotation = fresh
  sandbox from the bundle rootfs.
- **Template registry** is a JSON file passed via `-templates`:

  ```json
  {
    "python-3.12": {"driver": "cloudhypervisor", "path": "/var/lib/vessel/tpl/py312"},
    "node-22":     {"path": "/var/lib/vessel/tpl/node22"}
  }
  ```

- **Pids.** Tasks live inside a microVM with no host-visible init, so (as
  with other VM runtimes) the shim reports its own pid to containerd,
  consistently across Create/Start/State/Pids/Connect.
- **Signals.** The shim cannot yet deliver signals inside the guest, so any
  `Kill` tears the sandbox down; the exit status is the conventional
  128+signal (SIGTERM→143, SIGKILL→137) so callers can distinguish them.
- **Unimplemented RPCs are explicit.** Exec/ResizePty/CloseIO (land with the
  guest data-plane work), Pause/Resume (will map to CH vm.pause/resume) and
  Checkpoint/Update return codes.Unimplemented instead of fake success — a
  silent no-op Exec would make `kubectl exec` appear to work while doing
  nothing.

## What works now

- `pkg/shim.Service`: the Task RPC surface mapped onto the Manager, with
  the semantics above. Covered by unit tests and race-detector runs.
- `pkg/shim.Serve`: ttrpc transport. The round-trip test drives the full
  lifecycle — including a template restore and a NotFound-across-the-wire
  case — with the real containerd Task client over a unix socket.
- `containerd-shim-vessel-v1 -standalone -templates <file>`: serves the
  Task service on a fixed socket with a template registry for local
  validation.
- **The containerd handshake** (`pkg/shim/bootstrap.go`): the `start`
  subcommand listens on a derived socket, re-execs itself as a daemon
  (listener inherited as fd 3), records `bundle/address` + `bundle/
  shim.pid`, and prints the address containerd connects to; `delete`
  kills the daemon's process group and cleans up; `Shutdown` exits an
  idle daemon. Covered by a full-contract test (start → connect →
  lifecycle → Shutdown → delete, plus stale-socket recovery) that
  re-execs the test binary as the daemon.
- **Event publishing** (`pkg/shim/events.go`): TaskCreate/Start/Exit/
  Delete forwarded to containerd's ttrpc events endpoint
  (`TTRPC_ADDRESS`). TaskExit is what makes kubelet notice pod death.
  Best-effort with logging — event failures never fail the triggering RPC.
- **Node config** (`/etc/vessel/shim.json`, `pkg/shim/config.go`): VM
  assets, pool size and the template registry. Missing file = defaults
  (process driver); malformed file = hard error, because silently
  ignoring an admin's template registry means cold boots where warm
  restores were configured.
- **Real-containerd e2e**: `sudo ./scripts/ctr-e2e.sh` has containerd
  itself spawn the shim and drive run/kill/rm via `ctr`.
- **OCI rootfs → block image** (`pkg/image`): the CH driver packs the
  bundle's rootfs directory into a virtio-blk image on boot — mkfs.erofs
  when available (read-only, dedup, page-cache shared across VMs), else
  mkfs.ext4 -d (no root). A rootfs that is already a file (the configured
  default image, or a restored template) is used as-is. This is what lets
  a containerd task actually become a microVM rather than a process
  sandbox.

## What's next

- Pod networking: bridge the CNI netns into the VM (Kata's tc-mirror /
  vhost-net approach).
- Real-cluster e2e: `kubectl run` a template-annotated pod, `kubectl exec`
  (needs Exec), `kubectl delete`.

## Usage

Install the shim on each node's `$PATH` and register a RuntimeClass:

```yaml
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: vessel
handler: vessel        # containerd resolves containerd-shim-vessel-v1
```

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: agent-session
  annotations:
    vessel.dev/template: "python-3.12"   # restore from this snapshot
spec:
  runtimeClassName: vessel
  containers:
    - name: sandbox
      image: registry/agent-sandbox:latest
```

## Local validation today

```bash
CGO_ENABLED=0 go build -o containerd-shim-vessel-v1 ./cmd/containerd-shim-vessel-v1
./containerd-shim-vessel-v1 -standalone \
    -socket /tmp/vessel-shim.sock -templates /etc/vessel/templates.json
# drive it with the containerd Task ttrpc client (see pkg/shim/serve_test.go);
# scripts/kvm-e2e.sh runs the shim test suite under -race on real hardware
```
