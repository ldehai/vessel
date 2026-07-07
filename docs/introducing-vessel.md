# Introducing vessel: an agent-native sandbox runtime

*July 2026 · draft*

Every AI agent needs a machine. Not a thread, not a container sharing your kernel — a machine: its own kernel, its own filesystem, its own blast radius. The industry figured this out the hard way. By early 2026 the consensus was blunt: shared-kernel container isolation isn't good enough for executing code that an LLM just wrote.

vessel is an open-source sandbox runtime built for exactly this world. It gives every agent session a hardware-isolated microVM, and it treats the one operation agent workloads do constantly — spinning up a fresh, identical environment — as its core primitive.

## The gap we're filling

The isolation landscape today is strong at the bottom and fragmented in the middle. At the primitive layer, Firecracker, Cloud Hypervisor and gVisor are mature and battle-tested. At the platform layer, managed services will happily run your agent code for a fee. The middle — a runtime you can self-host, that speaks both Kubernetes and agent SDKs — is where things fall apart:

E2B is excellent but designed around its own cloud; self-hosting means adopting a full infrastructure stack. microsandbox proved that libkrun microVMs can boot in under 100ms, but it's local-first with no containerd or Kubernetes story. Kata Containers integrates beautifully with Kubernetes, but it targets traditional container workloads: no session semantics, no snapshot-and-clone API, and cold starts measured in hundreds of milliseconds.

Nobody offers all four of: OCI/containerd compatibility, microVM isolation, sub-100ms session creation via snapshots, and a self-hostable agent API. That combination is vessel.

## Design

vessel deliberately does not implement a VMM. Writing another virtual machine monitor in 2026 is re-plowing plowed ground — Cloud Hypervisor and Firecracker are excellent. vessel is the layer above: a Go daemon that manages sandbox lifecycles through a pluggable driver interface.

```
K8s / containerd ──► containerd shim ─┐
                                       ├─► sandboxd ──► driver (CH / FC / process)
AI apps ──► REST/gRPC + SDKs ─────────┘        │
                                        vsock + JSON protocol
                                               │
                                     guest: kernel + vessel-agent (PID 1)
```

Three decisions define the project:

**Pluggable VMM drivers.** The `Driver` interface is small: create, exec, snapshot, stop, plus an optional `Restorer` for drivers that can resurrect a VM from disk. Today there's a Cloud Hypervisor driver (production) and a Linux-namespaces driver (development — API-compatible, boots nothing, great for CI). Firecracker and libkrun drivers slot in without touching the core.

**Two front doors.** The same daemon will serve Kubernetes through a containerd shim (`runtimeClassName: vessel`, one line in a pod spec) and serve AI applications directly through a REST/gRPC API with session semantics: create, exec, upload files, snapshot, fork. Existing projects pick one door. Agent infrastructure needs both, because the same sandbox image your agents use in production should be schedulable by your cluster.

**Snapshots as the first-class primitive.** The agent pattern is always the same: prepare an environment once (install Python, warm the interpreter, load the model client), then clone it for every conversation. vessel's `fork` does exactly this — pause, snapshot, restore into a new VM — skipping kernel boot entirely. This is the mechanism behind every fast sandbox platform's cold-start numbers, and it's the part existing open-source runtimes treat as an afterthought.

Inside the guest, a single static Go binary (`vessel-agent`, ~2.6MB) runs as PID 1, listening on vsock. The host reaches it through Cloud Hypervisor's hybrid vsock handshake. The protocol is newline-delimited JSON — deliberately boring, because the interesting work happens at the VMM layer.

## Where it stands

vessel is young and we'd rather undersell it. What exists and is tested today: the core domain model with fork semantics, the Cloud Hypervisor driver (VMM interactions covered by mocks — real-KVM end-to-end is the current milestone), the guest agent and vsock transport with end-to-end tests over real sockets, a REST API, a zero-dependency Python SDK, and reproducible guest image build scripts (Alpine rootfs + agent, prebuilt or source-built kernel).

What does not exist yet: published cold-start benchmarks. Sub-100ms via snapshot restore is the design target, not a measured claim — the benchmark harness is in the repo, and numbers against Kata, E2B and microsandbox will accompany the first release. Also pending: the containerd shim, erofs image layering, and vsock socket remapping for many-clones-from-one-snapshot.

If the roadmap survives contact with reality, v0.1 ships with: measured cold starts, Kubernetes RuntimeClass support, and Python/TypeScript SDKs.

## Why Go

The layer vessel occupies — orchestration above the VMM, integration with containerd and Kubernetes — is Go's home field. containerd is Go, the Kubernetes ecosystem is Go, gVisor is Go. The VMM itself stays in Rust, where it belongs, behind a process boundary and a REST API. Right tool, right layer.

## Try it, break it

```bash
git clone https://github.com/andyliu/vessel && cd vessel
go build ./cmd/vessel
./vessel run -- sh -c 'echo hello from PID $$'   # namespace sandbox, no KVM needed
```

With KVM and cloud-hypervisor installed, `images/build-kernel.sh` and `images/build-rootfs.sh` produce a bootable guest in about a minute, and `bench/coldstart.sh` tells you what your hardware can do.

The most valuable contribution right now is adversarial: boot it on real KVM, find where the driver's assumptions break, and file the issue. Every great runtime got there by being broken early and often.
