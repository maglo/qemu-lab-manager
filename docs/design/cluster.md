# Lab cluster — design

The QEMU lab is a cluster. A bootable container image stands a node up.
labview is the control plane on each node. The API creates and destroys
machines, shows every console, exports telemetry, and runs scenarios.

This document is the product shape. [`labview.md`](labview.md) stays the
document of the console wall, the lease, the serial broker and the HTTP
surface that already ships. Where the two disagree on scope or deploy,
this document wins.

Status: slices 1-10 ship. The epic is
https://github.com/maglo/qemu-lab-manager/issues/72.

---

## 1. Scope

In:

- Machine lifecycle over the API: create, configure, start, stop,
  destroy.
- A Fedora bootc image on GHCR. The image is the node OS.
- A cluster of nodes, one wall, one API.
- Nodes that join and leave. A new machine goes to a node that has
  capacity.
- Observation: live framebuffer, serial, machine details.
- Telemetry: events as NDJSON, numbers as Prometheus metrics.
- Scenario orchestration: declare machines, run steps, assert, tear
  down.

Out:

- Ansible as the deployer of this lab or of its VMs.
- A privileged container on a foreign host OS.
- A user database inside labview. Identity still arrives from a proxy.
- Creation of a host. An operator or other tooling creates the VM or
  installs the physical node. labview uses the nodes that join.
- More than a small team needs. This is a lab test tool.

---

## 2. Why this shape

The console wall is useful only when machines exist. Today something
else creates them. That split forces two tools, two inventories and a
deploy path the lab does not control.

The API owns the machine. The image owns the host. The cluster places
machines on the nodes it has. A scenario owns a run. One product.

---

## 3. The bootc image

The node boots from a Fedora bootc image. The image holds:

- A Linux host with QEMU and the units labview needs.
- The `labview` binary and its unit.
- The agent that joins the cluster (later slice).

labview runs as a systemd unit on that host. It is not a container
process on a foreign OS. The sources live under `image/`.

### Why not a container on the hypervisor

A container with `container_t` cannot reach QEMU sockets, the system
bus or unit control on Enterprise Linux. `spc_t` can, and it is
unconfined. That is a wider boundary, not a narrower one.
[`labview.md`](labview.md) section 11 records that rejection.

bootc changes the question. The image *is* the host. labview keeps a
real unit boundary. The image is still how we ship and upgrade.

### Ship target

| Item | Value |
|---|---|
| Registry | GitHub Container Registry (`ghcr.io`) |
| Image | `ghcr.io/maglo/qemu-lab-manager/labview-node` |
| Visibility | Public |
| Base | `quay.io/fedora/fedora-bootc:45` |
| Architectures | x86_64 first. aarch64 when a node needs it. |

GHCR stores and serves container images at no charge on the free tier
today. Public packages stay free if that policy narrows to public
only. Quay is not required while that holds.

A push to `main` publishes `:latest`. A `v*` tag publishes that
version. The Go binary release on GitHub stays for developers who run
labview on a laptop against a fake lab.

---

## 4. Machine lifecycle

The API is the source of machine truth. There is no inventory directory
that Ansible writes for this lab.

A machine has a desired state and a reported state. labview on the
owning node makes the host match the desired state: disk, unit, QEMU
flags, serial socket, QMP socket, VNC bind.

### API shape (first cut)

| Method | Path | Meaning |
|---|---|---|
| `POST` | `/api/machines` | Create a machine from a document. |
| `GET` | `/api/machines` | List machines across the cluster view. |
| `GET` | `/api/machines/{id}` | One machine, as today, plus LCM fields. |
| `PATCH` | `/api/machines/{id}` | Change configuration or desired state. |
| `DELETE` | `/api/machines/{id}` | Destroy the machine and its disks. |
| `POST` | `/api/machines/{id}/power` | Start, stop or restart (already ships). |

Create and destroy take the write lease for that id once the machine
exists. Create of a new id is free; the creator holds the first lease.
Section 5 says which node gets a new machine.

The console, serial, screenshot and key endpoints stay as
[`labview.md`](labview.md) section 5 names them.

### Create document

The create body is `lifecycle.Spec`. Scenario documents pass the same
shape under `spec.machines[].create`
([`scenarios.md`](scenarios.md)).

```json
{
  "id": "el9-build",
  "name": "el9-build",
  "memoryMb": 2048,
  "vcpus": 2,
  "diskGb": 20,
  "notes": "",
  "image": "",
  "firmware": "uefi",
  "disks": [
    {"name": "root", "sizeGb": 20},
    {"name": "data", "sizeGb": 40, "bus": "scsi"}
  ],
  "tpm": {"version": "2.0", "model": "tpm-tis"},
  "usb": {"controllers": 1, "devices": [{"id": "tablet0", "kind": "tablet"}]},
  "ccid": [{"id": "reader0"}]
}
```

| Field | Meaning |
|---|---|
| `diskGb` / `image` | Shorthand primary disk when `disks` is empty. `image` is an existing file; empty creates a blank sparse raw disk. |
| `firmware` | `bios` (default) or `uefi` (OVMF on the node image). |
| `disks` | One or more disks (`virtio`, `scsi`, or `ide`). |
| `tpm` | Emulator-backed TPM; the unit starts `swtpm` when `tpm/` exists. |
| `usb` | XHCI controllers and boot-time USB devices. |
| `ccid` | Smart-card readers at boot (`usb-ccid`). |

Create writes the inventory entry, `qemu.argv`, disks and `spec.json`,
and names the unit `qemu-vm@{id}.service`. Destroy requires the write
lease, stops the unit, and removes the files.

`PATCH /api/machines/{id}` updates desired state, rewrites `qemu.argv`,
and returns `needsRestart` when the running QEMU must be restarted.
`GET /api/machines/{id}` includes `desired` for managed machines.
Reported hardware stays in `details` from host inspect.

Hotplug (write lease required):

| Method | Path | Meaning |
|---|---|---|
| `POST` | `/api/machines/{id}/devices` | Insert USB tablet/storage/CCID, or inject a CCID card. |
| `DELETE` | `/api/machines/{id}/devices/{deviceId}` | Remove a hotplugged device. |

Timed actions, asserts and evidence stay in the scenario runner. When
hotplug exists, scenarios add a step that posts these paths.

`-machines-dir` empty disables create, destroy and patch. The bootc
node image sets it to `/var/lib/labview/machines`.

---

## 5. Cluster

One node runs alone and is a valid lab. Several nodes join one cluster.

- Each node runs labview and owns the machines scheduled onto it.
- One node serves the wall and the API that clients use. Other nodes
  are peers it reaches over the same section-5 API, on a private
  network the image configures.

### Nodes

A node is a cloud VM, for example in OpenStack, or a physical host.
labview does not create a node.

A node needs KVM. A VM node needs a flavor with nested
virtualization. A node without KVM shows as not ready, and it gets no
machines. QEMU without KVM is too slow for a lab.

A node has KVM when `/dev/kvm`, `/sys/module/kvm_intel` or
`/sys/module/kvm_amd` exists. The module directories count because the
labview unit has a private `/dev`.

cloud-init configures a node at first boot. The user data writes
`/etc/labview/node.env` and the token file.
[`image/README.md`](../../image/README.md) has an example.

### Join with the peer flag

`-peer name=url` names each peer (repeatable). Peer machines appear on
`GET /api/machines` as `peer:local-id`. Machine-scoped HTTP forwards to
the peer that owns the machine. Create stays local: a client that wants
a machine on a peer calls that peer, or a later scheduler does.

### Cluster token

`-cluster-token-file` names a file that holds the cluster token.
`-cluster-token` gives the token on the command line, where other
users of the host can read it.

A node sends the token as `X-Labview-Cluster-Token` on each call to
another node. A node answers 401 to a request that carries a wrong
token. A node with no token answers 401 to each request that carries
one.

A request with no token passes. A browser sends none, and the proxy in
front of the node is the gate for a person. The token keeps the nodes
of one cluster apart from the nodes of another. It does not hide a
node from the network.

A peer that does not answer is named in `peerFailures` on the
machines list; the rest of the wall still loads.

### Join at boot

A node registers with the API node when it starts, and again at each
`-join-interval` (30 seconds). The API node does not restart.

| Flag | On | Meaning |
|---|---|---|
| `-join` | node | URL of the API node. |
| `-node-name` | node | The name of the node. It is the left side of `name:local-id`. |
| `-advertise-url` | node | Where the API node reaches the node. |
| `-join-interval` | node | Time between two registrations. |
| `-node-ttl` | API node | How long a node stays with no registration (90 seconds). |

The node sends `POST /api/cluster/nodes` with its name, its URL and if
it has KVM. The call needs the cluster token. An API node with no
token answers 403, so a lab with no token accepts no node.

A registered node is a peer. Its machines appear on the wall, and
machine calls forward to it, as for a `-peer` node. A node that sends
no registration for `-node-ttl` leaves the list. A node that fails to
register tries again at the next interval, so the API node can start
last.

A registration cannot take the name of a `-peer` node. A registration
with a known name and a new URL replaces the URL, because a cloud VM
can come back with a new address. The token is the only check on that.

`GET /api/cluster/nodes` lists the nodes: name, source (`flag` or
`join`), KVM, ready and the time of the last registration. It shows no
URL. A `-peer` node does not report KVM, and it shows as ready.

`-peer` stays for a fixed set of nodes.

### Placement

`POST /api/machines` on a node that has peers picks the node for the
new machine. A node with no peer creates the machine on itself, with
no check.

Each node reports its capacity on `GET /api/cluster/capacity`:

| Field | Meaning |
|---|---|
| `lifecycle` | The node can create a machine. |
| `kvm` | The node has KVM. |
| `cpus` | The CPU count of the node. |
| `memoryFreeMb` | `MemAvailable` of the kernel. |
| `diskFreeGb` | Free space under the machines directory. |

The API node asks each ready node at the time of the create, so the
numbers are current. A machine fits a node when all of these hold:

- The node can create a machine, and it has KVM.
- `memoryMb` is not more than `memoryFreeMb`.
- `vcpus` is not more than `cpus`. The CPU count is not divided
  between machines.
- The sum of the blank disks is not more than `diskFreeGb`. A blank
  disk is sparse, and it counts in full.

The node with the most free memory takes the machine. This node wins
a tie. The API node forwards the create document to a peer with the
cluster token and the identity of the user. The answer carries the id
as `name:local-id`. A node does not place a create that carries the
cluster token, so two nodes cannot send a create to each other.

When no node fits, create answers 409. The error names each node and
the resource that is short on it. The operator then adds a node.

A `node` field next to the create document names the node. Its value
is a node name, or `local` for the node that gets the request. A named
node gets the machine with no capacity check.

The numbers are a view of one moment. Two creates at the same time
can pick the same node. A stopped machine uses no memory, so a node
can accept more machines than it can run at the same time.

### Websocket forward

`/ws/console/{id}` and `/ws/serial/{id}` forward to the node that owns
the machine, as the machine calls do. The API node copies the bytes in
both directions. It reads no frame. A close on one side closes the
other side.

The lease, the serial broker and the recording stay on the node that
owns the machine. The forward carries the identity of the user, so
that node names the right holder. A close of the socket on the API
node releases the lease on that node.

A browser sends the origin of the page with a websocket. A node
refuses an origin that is not its own host. The page comes from the
API node, so the node that owns the machine does not know its origin.
The API node does the origin check, and it forwards the socket with
no origin. A refusal from the peer, such as a machine with no console,
reaches the browser with its status.

A node that does not answer gives 502.

### Failure

Failure rule: a node that disappears keeps its machines marked
unreachable. The control plane does not silently recreate them on
another node. A scenario that needs replace-on-failure says so.

---

## 6. Telemetry

Telemetry has two forms. Events leave as NDJSON. Numbers leave as
Prometheus metrics. There is no OTLP export.

### Events

Every node emits:

- Node: up (machine count, disk free, version, peer count), join,
  peer-fail.
- Machine: created, destroyed, power, lease, denied, attach, detach.
- Scenario: run id, step, result (kinds in
  [`scenarios.md`](scenarios.md) section 8).

`GET /api/telemetry` writes one JSON object per line (NDJSON). The
handler dumps the recent ring first. It then follows live events until
the client closes. `?follow=0` dumps the ring and closes. The same
events are one JSON object per line on the unit's stdout. Activity
actions feed the machine events. A peer listing failure emits
`peer-fail` once per distinct error text.

Scenario events land with the scenario runner.

### Metrics

`GET /metrics` serves the Prometheus text format.

| Metric | Type | Meaning |
|---|---|---|
| `labview_build_info{version}` | gauge | The version of labview. The value is 1. |
| `labview_node_kvm` | gauge | 1 when the node has KVM. |
| `labview_node_lifecycle` | gauge | 1 when the node can create a machine. |
| `labview_node_cpus` | gauge | The CPU count of the node. |
| `labview_node_memory_free_bytes` | gauge | The memory that a new machine can use. |
| `labview_node_disk_free_bytes` | gauge | The free space under the machines directory. |
| `labview_node_peers` | gauge | The peers of this node. |
| `labview_node_machines` | gauge | The machines in the inventory of this node. |
| `labview_machine_live{machine}` | gauge | 1 when the machine runs. |
| `labview_machine_lease_held{machine}` | gauge | 1 when a user holds the write lease. |
| `labview_lease_grants_total` | counter | The write leases that this node gave. |
| `labview_lease_denials_total` | counter | The requests refused because the user did not hold the lease. |
| `labview_peer_failures_total` | counter | The machine list calls to a peer that failed. |

The node gauges are the numbers of `GET /api/cluster/capacity`, so a
graph shows what placement sees. `labview_node_disk_free_bytes` is 0
on a node that cannot create a machine.

A counter starts at 0 when labview starts. `labview_machine_live` is
the `live` field of `GET /api/machines`. No metric has a user name as
a label, so the number of series follows the number of machines.

Each node serves its own metrics, and Prometheus scrapes each node.
The API node does not collect the metrics of its peers. The image
ships no Prometheus.

labview writes the text format itself. It has few metrics, and the
format is small, so it takes no client library.

Live consoles and serial are observation, not telemetry. They stay on
their websockets.

---

## 7. Scenarios

A scenario is a versioned document and a run of that document. It
names machines to create or reuse, ordered steps, asserts, cleanup
and evidence. [`scenarios.md`](scenarios.md) is the document of that
interface.

The server still does no expect-regex on serial bytes for interactive
attach. The scenario runner is the one place that matches output, with
timeouts the document sets. That keeps
[`labview.md`](labview.md) section 5's rule for harnesses that are not
scenarios.

Locked for scenarios (see [`scenarios.md`](scenarios.md)):

- `apiVersion: labview.dev/vN` and `kind` on every document.
- YAML in-repo; JSON on the wire; the API accepts an uploaded run.
- `Lab` / `Machine` / `Scenario` share the version line.
- Desired hardware stays on the machine document. Steps only call
  LCM and machine verbs.
- Evidence reuses screenshots and serial captures.

Issue: https://github.com/maglo/qemu-lab-manager/issues/82.

---

## 8. Trust

Identity still arrives in `X-Forwarded-User` from a proxy in front of
the API node. It names lease holders and audit events. It is not an
authorisation table inside labview.

Node-to-node calls carry the cluster token. cloud-init writes the
token file at first boot. Clients do not see the token. Section 5 says
what the token does and does not protect.

---

## 9. Slices

Work lands in this order. Each slice has its own issue under #72.

1. This document and the scope fix in `labview.md`. Done.
2. Fedora bootc image on GHCR; one node; wall for local machines. Done.
3. Machine create and destroy on one node. Done.
4. Cluster join; one pane across nodes. Done.
5. Telemetry stream. Done.
6. Scenario object and runner. Done (#82 document, #88 runner).
   Follow-ons: hotplug step (#100, merged), labviewctl (#101).
7. Join at boot with cloud-init and a checked cluster token. Done.
8. Placement of a new machine. Done.
9. Prometheus metrics. Done.
10. Websocket forward to peer machines. Done.

---

## 10. Decisions

No decision is open.

| Topic | Decision |
|---|---|
| Registry | GHCR, public. |
| Base image | Fedora bootc. |
| Binary name | `labview`. |
| Scenario format | See [`scenarios.md`](scenarios.md). |
| Nodes | Cloud VMs or physical hosts. cloud-init configures them. |
| KVM | A node needs KVM. A VM node needs nested virtualization. |
| Capacity | Join and placement. labview does not create a host. |
| Telemetry | NDJSON events and Prometheus metrics. No OTLP. |
| Size | A small team. |
