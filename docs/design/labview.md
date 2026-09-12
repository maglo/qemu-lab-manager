# labview — design

A console wall for the QEMU lab. Developers see every machine at a glance,
open one to drive it, and attach to serial when a scenario needs it. One Go
binary, one inventory file, no database.

Status: design only. Implementation happens elsewhere.

---

## 1. Scope

In:

- A wall of live, view-only framebuffers, one per machine.
- Click a machine to expand it to the viewport and take control.
- Attach to the graphical console or the serial line over a websocket,
  from a browser or from a test harness.

Out, for now:

- Creating, destroying or reconfiguring machines. That stays in Ansible.
- Power operations. Deliberately deferred — see §8.
- Anything resembling a user database.

---

## 2. Machines and channels

A machine has up to three channels. Their properties differ enough that they
need different plumbing, which is most of this document.

| Channel | Source | Concurrency | Nature |
|---|---|---|---|
| Framebuffer | QEMU VNC port | QEMU shares it across clients | Binary RFB, stateful handshake |
| Serial | QEMU chardev socket | **One client only** | Byte stream, no framing |
| Control | QMP socket | One client only | JSON request/response |

The framebuffer is the easy one: one browser websocket, one TCP dial to the
VNC port, copy bytes both ways. No state on the server.

Serial cannot work that way, and that asymmetry drives §3.

---

## 3. The serial broker

### The problem

A QEMU `-chardev socket` accepts a single connection. Two developers
attaching means the second is refused. A developer attaching while a
scenario script is driving the machine means one of them silently loses
keystrokes. And a client that attaches at 14:02 sees nothing that happened
at 14:01, which is exactly the boot output they wanted.

### The shape

labview holds **one** upstream connection per serial socket, opened when the
machine first appears in the inventory and re-established with backoff
whenever it drops. Clients subscribe to the broker, not to QEMU.

    QEMU chardev ──► broker ──┬──► browser pane
                              ├──► second browser (read-only)
                              ├──► scenario harness (holds the lease)
                              └──► transcript file

Each broker keeps:

- **A ring buffer**, last ~256 KB of output. Sent to every subscriber on
  attach, before live bytes. This is the scrollback, and it is the reason
  the broker earns its complexity.
- **A subscriber set.** All subscribers read. Writes are gated by the lease.
- **A transcript**, optional per machine, appended with timestamps. Makes a
  failed scenario run readable after the fact.

### Consequences worth accepting

- labview holds the serial socket even when nobody is watching. That is the
  point — output is captured regardless of who is attached.
- Restarting labview drops the ring buffer. Acceptable; the transcript on
  disk survives.
- Serial output is not UTF-8 safe mid-stream. Frames are binary, and the
  browser decodes incrementally. Do not decode on the server.

---

## 4. The control lease

Read is free. Write is leased.

- The wall is view-only for everyone, always.
- Expanding a machine requests the lease. Granted if free.
- If held, the developer sees who holds it and can watch, not type.
- A lease expires after a few minutes of no input, or on release, or when
  the websocket closes. No manual stealing in v1; expiry handles the
  developer who closed their laptop.

The lease covers framebuffer input and serial input together. A machine has
one driver at a time regardless of which channel they are driving it
through, which is the only model that makes sense when a scenario is
half-serial and half-GUI.

A scenario harness takes the same lease a human does. That is what stops a
developer from typing into a VM mid-test, and it needs no special
"automation mode".

---

## 5. HTTP surface

| Endpoint | Purpose |
|---|---|
| `GET /api/machines` | Inventory plus liveness and current lease holder |
| `GET /ws/console/{id}` | Binary RFB, proxied to the VNC port |
| `GET /ws/serial/{id}` | Serial stream from the broker, scrollback first |
| `POST /api/machines/{id}/lease` | Acquire, renew or release |

The serial endpoint takes `?write=1` to request the lease at attach time,
which is what a harness wants. Without it, attach is read-only.

Design rule: **the server does no matching.** No server-side expect, no
regex-on-output, no "wait for prompt". A harness gets raw bytes and does its
own matching, in its own language, with its own timeouts. Push that logic
into a small client package rather than into the protocol.

---

## 6. Inventory

Written by the playbook, read by labview on mtime change. Extends the
existing shape with a serial address:

```json
[
  {
    "id": "el9-build",
    "name": "el9-build",
    "host": "kvm01",
    "vnc": "10.20.0.11:5901",
    "serial": "/run/qemu/el9-build-serial.sock",
    "notes": "AlmaLinux 9"
  }
]
```

`serial` is a unix socket path when labview runs on the hypervisor, or
`host:port` when it doesn't. Either is a `net.Dial`, so the broker does not
care which.

Neither `vnc` nor `serial` is ever sent to the browser. Clients name a
machine by `id` only, so a developer cannot dial a console the inventory did
not give them.

---

## 7. UI states

Three, no more.

**Wall.** Grid of live framebuffers, view-only. Machine name, hypervisor, a
liveness dot. Machines without a console show why.

**Expanded.** One machine fills the viewport, tabbed — see §8 for what the
tabs hold. Console is the default tab and shows the same RFB session as the
tile, with `viewOnly` flipped off once the lease is held, so there is no
reconnect and no second handshake. A control row sits above the tabs: send
Ctrl+Alt+Del, full screen, take or release control, back to wall.

**Serial-only.** Some machines have no framebuffer worth showing. Their
tile is a serial tail, and expanding opens on the serial tab instead of
console. Same view, different default.

Beyond roughly eight tiles, every live RFB session is a decoder running in
one browser tab. At that point the wall should switch to periodic
screenshots and open RFB only on click. Build the tile as a component that
can render either, so this is a config change and not a rewrite.

---

## 8. What a developer can inspect

This is a debugging tool, so the bias is toward showing everything the host
already knows about a machine. The constraint is not what to show, it is
keeping it organised enough to be worth reading.

The expanded view is tabbed. Console is the default and the other tabs are
one click away, never a different page.

**Console** — the framebuffer, as today.

**Serial** — live serial, scrollback first.

**Details** — everything static or slow-moving about the machine, in one
scrollable page:

- The full QEMU command line, as invoked. This is the single most useful
  thing on the page and the thing that is most annoying to dig out by hand.
- The systemd unit name, its state, and when it last changed state.
- Disk images: path, format, virtual and actual size, backing file.
- Network: taps, bridges, MAC addresses, guest addresses if known.
- Memory, vCPUs, machine type, firmware.
- The machine's entry in the Ansible inventory, verbatim.

**Logs** — recent journal lines for the unit, tailing live. A VM that failed
to start has its reason here and nowhere else, which is the case where a
developer currently has to SSH to the hypervisor.

**Recordings** — past serial captures for this machine, one per run, with
timestamps and sizes. Play in the browser or download.

**Activity** — who attached when, who holds control, who held it before.
Useful for "why did the VM reboot", less so day to day.

Two rules that matter more than the list itself:

- **Everything is copyable.** Every block has a copy button, and the
  underlying text is plain, unwrapped and unstyled when copied. A command
  line that has been visually wrapped and copied with the wrapping is worse
  than useless.
- **Everything is reachable as JSON.** `GET /api/machines/{id}` returns the
  whole record. The UI is one consumer of it; a script is another. Nothing
  appears in the UI that a harness cannot also fetch.

Gathering this needs a small amount of host-side introspection — reading the
unit, its cgroup, its command line, the journal. That is all readable from
the hypervisor without libvirt and without an agent, which is the point.

---

## 9. Deferred, deliberately

**Stealing a lease.** Expiry covers the common case. Add stealing only if
expiry turns out to be too slow in practice.

**Recording and replay of framebuffer sessions.** Serial transcripts give
most of the forensic value at a fraction of the cost.

---

## 10. Deployment and trust

labview binds loopback. A reverse proxy terminates TLS and does SSO, and
forwards the original `Host` header so the websocket origin check keeps
working. The asserted identity arrives in `X-Forwarded-User` and is used for
two things: naming the lease holder in the UI, and logging who attached to
what. It is not an authorisation input — everyone who gets past the proxy
can see every machine.

VNC and serial endpoints are bound to the hypervisor's management address,
or to a unix socket with labview running alongside. They are not reachable
from the developer network. labview is the only path in, which is what makes
the lease meaningful.

---

## 11. Settled decisions

### Brokers start eagerly

Every machine in the inventory gets a broker at process start, with a
reconnect loop and backoff. Not lazily on first subscriber.

The deciding case is not first attach, it is a VM that reboots while nobody
is watching. An eager broker reattaches as soon as QEMU recreates the
socket, so the next person to open the machine sees the boot they missed. A
lazy broker cannot, because there was no subscriber to trigger it.

Cost is one goroutine and one ring buffer per machine. At lab scale that is
a few megabytes and some idle sockets.

### Transcripts are on, one file per upstream connection

Format is **asciicast v2**: a JSON header line, then `[elapsed, "o", data]`
lines. Chosen over a raw byte dump because timing is what makes a failed
scenario readable afterwards, and over inventing a framing because players
for this format already exist.

Do not interleave timestamps into the byte stream. It corrupts replay and
makes the file useless as a serial capture.

Rotation is by **broker reconnect**, not by clock or size — a reconnect is a
VM lifecycle boundary, so one file is one VM run. Named by machine id and
start time. Retain by count and age, both configurable, with a size ceiling
as a backstop against a machine that spews.

The broker owns the fd, so rotation happens in-process. Do not point
logrotate at these files; copytruncate against a live writer loses data.

### You can attach to a machine that is not running yet

A developer who wants to watch a VM boot has to connect before it boots.
Connecting afterwards means the boot messages have already scrolled past.

So: opening a machine connects you to the viewer, not to the VM. The viewer
is already connected to the VM's serial port and stays that way. You can
open a machine that is switched off, wait there, start it, and watch it come
up from the first line. A test harness does the same thing, which is what
stops it from racing the boot it wants to observe.

The connection carries two different kinds of thing:

- What the VM prints.
- Status from the viewer: the machine came up, the machine went away,
  someone else has the keyboard, your control is about to expire.

These must not share one stream. Mixed together, a status message looks like
something the VM printed — it lands in the recording and in anything
grepping the output. Websockets already carry two message types, so VM
output goes in binary frames and status in text frames. Nothing to invent
and nothing to escape.

The graphical console stays pure binary. Its protocol has its own handshake
and nothing may be injected into it.

---

## 12. Power operations

Start, stop and restart are in v1, and they go through **systemd, not QMP**.

The VMs are systemd units. Using the unit matches how they are actually
managed, reports a crashed machine as failed rather than merely absent, and
needs no second control socket held open per machine.

Four rules:

- Talk to systemd over its D-Bus API, not by shelling out to `systemctl`.
  Shelling out means building a unit name into a command string from a
  request, and one parsing mistake there is command execution on the
  hypervisor.
- A client never names a unit. It names a machine id, and labview maps that
  to a unit through the inventory. The inventory is therefore also the
  allowlist: labview can touch exactly the units it was told about and
  nothing else on the host.
- Power operations require the write lease, the same one that gates the
  keyboard. Restarting a machine somebody else is driving should be as
  impossible as typing into it.
- labview runs as its own user, not root, with a polkit rule granting unit
  management over just those units.

Restart is the only destructive thing in an otherwise read-mostly tool, so
it confirms in the UI and is logged with the identity from the proxy.

---

## 13. Boundaries

labview is a standalone service. It does not assume Ansible, and nothing in
it should. Whether the collection deploys it and writes its inventory is a
deployment question to answer later, and the design stays usable either way.

Three interfaces, and they are the whole contract:

**Inventory in.** A JSON file in the shape of §6. labview re-reads it when
the mtime changes. Any producer will do — a playbook, a script, a person
with an editor.

**Host access out.** An internal interface, because the channels split two
ways. Framebuffer and serial are dialable over the network. The QEMU command
line, the unit state, the journal and power operations are not — they are
local to the hypervisor.

**API out.** HTTP and websockets as in §5, with everything the UI displays
also available as JSON. The UI is one consumer, a test harness is another,
and neither gets a privileged path.

### What that means for multiple hypervisors

One labview per hypervisor, with the local implementation of host access.
This is the v1 shape, and it is why §6's inventory can list a `host` field
without labview needing to reach it.

Aggregation, if it is wanted later, is a labview configured with remote
hosts that are other labviews. The remote implementation of host access
speaks the API from §5, which already exists and is already authenticated at
the proxy. No agent, no second protocol, and a single-host install is not
paying for a feature it does not use.
