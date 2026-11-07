# Lab cluster — design

The QEMU lab is a cluster. A bootable container image stands a node up.
labview is the control plane on each node. The API creates and destroys
machines, shows every console, exports telemetry, and runs scenarios.

This document is the product shape. [`labview.md`](labview.md) stays the
document of the console wall, the lease, the serial broker and the HTTP
surface that already ships. Where the two disagree on scope or deploy,
this document wins.

Status: slices 1-5 ship. Scenarios and auto-extend remain under
https://github.com/maglo/qemu-lab-manager/issues/72.

---

## 1. Scope

In:

- Machine lifecycle over the API: create, configure, start, stop,
  destroy.
- A Fedora bootc image on GHCR. The image is the node OS.
- A cluster of nodes, one wall, one API.
- Nodes that join and leave. Capacity that extends when the lab needs
  more hosts.
- Observation: live framebuffer, serial, machine details.
- Telemetry: metrics and events about nodes, machines and scenarios.
- Scenario orchestration: declare machines, run steps, assert, tear
  down.

Out:

- Ansible as the deployer of this lab or of its VMs.
- A privileged container on a foreign host OS.
- A user database inside labview. Identity still arrives from a proxy.

---

## 2. Why this shape

The console wall is useful only when machines exist. Today something
else creates them. That split forces two tools, two inventories and a
deploy path the lab does not control.

The API owns the machine. The image owns the host. The cluster owns
capacity. A scenario owns a run. One product.

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

The console, serial, screenshot and key endpoints stay as
[`labview.md`](labview.md) section 5 names them.

### Create document

```json
{
  "id": "el9-build",
  "name": "el9-build",
  "memoryMb": 2048,
  "vcpus": 2,
  "diskGb": 20,
  "notes": "",
  "image": ""
}
```

`image` empty means a blank raw disk under
`/var/lib/labview/machines/{id}/`. A backing image path is a later
slice. Create writes the inventory entry, a `qemu.argv` file and a
sparse disk, and names the unit `qemu-vm@{id}.service`. The node image
ships that template unit. Destroy requires the write lease, stops the
unit, and removes the files.

`-machines-dir` empty disables create and destroy. The bootc node
image sets it to `/var/lib/labview/machines`.

---

## 5. Cluster

One node runs alone and is a valid lab. Several nodes join one cluster.

- Each node runs labview and owns the machines scheduled onto it.
- One node serves the wall and the API that clients use. Other nodes
  are peers it reaches over the same section-5 API, on a private
  network the image configures.
- Auto-extend adds a node when the scheduler cannot place a machine.
  How a node is provisioned (cloud API, bare metal, nested VM) is an
  open decision on issue #72.

### Join (this slice)

`-peer name=url` names each peer (repeatable). Peer machines appear on
`GET /api/machines` as `peer:local-id`. Machine-scoped HTTP forwards to
the peer that owns the machine. Create stays local: a client that wants
a machine on a peer calls that peer, or a later scheduler does.

`-cluster-token` is sent as `X-Labview-Cluster-Token` on peer calls.
Empty sends nothing. Enforcing the token on inbound requests is a
later hardening slice.

Websocket attach to a remote machine answers 501 until forwarding
lands. A peer that does not answer is named in `peerFailures` on the
machines list; the rest of the wall still loads.

Failure rule: a node that disappears keeps its machines marked
unreachable. The control plane does not silently recreate them on
another node. A scenario that needs replace-on-failure says so.

---

## 6. Telemetry

Every node emits:

- Node: up (machine count, disk free, version, peer count), peer-fail.
- Machine: created, destroyed, power, lease, denied, attach, detach.
- Scenario: run id, step, result (kinds in
  [`scenarios.md`](scenarios.md) section 8).

`GET /api/telemetry` writes one JSON object per line (NDJSON). The
handler dumps the recent ring first. It then follows live events until
the client closes. `?follow=0` dumps the ring and closes. The same
events are one JSON object per line on the unit's stdout. Activity
actions feed the machine events. A peer listing failure emits
`peer-fail` once per distinct error text.

OTLP or a remote sink is a later slice if a lab asks for it. Scenario
events land with the scenario runner.

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

Node-to-node calls use a cluster credential the image provisions at
join time. Clients do not see it.

---

## 9. Slices

Work lands in this order. Each slice has its own issue under #72.

1. This document and the scope fix in `labview.md`. Done.
2. Fedora bootc image on GHCR; one node; wall for local machines. Done.
3. Machine create and destroy on one node. Done.
4. Cluster join; one pane across nodes. Done.
5. Telemetry stream. Done.
6. Scenario object and runner. Document locked (#82). Runner v1
   (https://github.com/maglo/qemu-lab-manager/issues/88).
7. Auto-extend of capacity.

---

## 10. Open decisions

Tracked on https://github.com/maglo/qemu-lab-manager/issues/72:

1. How a node auto-extends.
2. Telemetry sink beyond JSON/stdout.

Locked: registry is GHCR, base is Fedora bootc, binary name is
`labview`. Scenario document format is locked in
[`scenarios.md`](scenarios.md).
