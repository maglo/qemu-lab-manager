# labview — design

A console wall for the QEMU lab. Developers see every machine at a glance,
open one to drive it, and attach to serial when a scenario needs it. One Go
binary, one inventory directory, no database.

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
"automation mode". A harness arrives under a name of its own, one for each
run, so two runs of one harness never contend for one machine. Section 10
gives the form of that name and says who issues it.

---

## 5. HTTP surface

Everything the UI shows is also JSON. The UI is one consumer, a harness is
another, and neither gets a privileged path.

| Endpoint | Purpose |
|---|---|
| `GET /api/machines` | Inventory plus liveness and current lease holder |
| `GET /api/machines/{id}` | The whole record: details, disks, network, unit, recordings, activity |
| `GET /api/machines/{id}/recordings` | Past serial captures |
| `GET /api/machines/{id}/recordings/{name}` | One capture, asciicast v2 |
| `GET /api/machines/{id}/activity` | Who attached when, who holds control |
| `POST /api/machines/{id}/lease` | Acquire, renew or release |
| `POST /api/machines/{id}/power` | Start, stop or restart |
| `POST /api/machines/{id}/keys` | Send one chord over the control socket |
| `GET /api/machines/{id}/screenshot` | One frame of the screen, as PNG |
| `GET /api/activity` | The activity of every machine |
| `GET /api/config` | The settings the browser application needs |
| `GET /ws/console/{id}` | Binary RFB, proxied to the VNC port |
| `GET /ws/serial/{id}` | Serial stream from the broker, scrollback first |
| `GET /healthz` | Liveness, and the inventory files that did not load |
| `GET /` | The browser application |

This table is the list. A reader who wants to know what labview serves reads
it here and nowhere else.

The serial endpoint takes `?write=1` to request the lease at attach time,
which is what a harness wants. Without it, attach is read-only.

Design rule: **the server does no matching.** No server-side expect, no
regex-on-output, no "wait for prompt". A harness gets raw bytes and does its
own matching, in its own language, with its own timeouts. Push that logic
into a small client package rather than into the protocol.

---

## 6. Inventory

A directory. One machine is one YAML file in it, and the file name is the
machine `id`:

```yaml
# /etc/labview/inventory.d/el9-build.yaml
name: el9-build
host: kvm01
vnc: 10.20.0.11:5901
serial: /run/qemu/el9-build-serial.sock
control: /run/qemu/el9-build-qmp.sock
unit: qemu-el9-build.service
notes: AlmaLinux 9
```

One file per machine is what makes the inventory editable. A person changes
one machine by opening one short file, a playbook writes one file per
machine with no read-modify-write, and a diff of a change names the machine
in the path. The file name is the only source of the `id`, so two machines
cannot claim one id, and a file must not set `id` itself.

labview watches the directory. A new file, a changed file and a deleted file
all reach the wall at once. It also reads the directory again every
`-inventory-rescan`, as a backstop for a watch the filesystem does not
deliver.

`.yaml` and `.yml` both count. Anything else in the directory is ignored: a
README, an editor's backup, a dot file a writer has not finished with.

The unit of failure is the file. A file that does not parse is logged and
keeps the entry that last parsed, and the other machines are untouched. A
typo takes down one machine at most, and a half-written file takes down
nothing. `/healthz` names the files that did not load.

The first load is strict. labview does not start with a file it cannot read,
because a wall that is quietly missing a machine is worse than one that does
not come up.

The file name is the id, and nothing else is. It becomes a path element and a
capture filename, so it is validated at load: letters, digits, dot,
underscore and hyphen, starting with a letter or digit.

`unit` names the systemd unit of the machine. It is an allowlist rather than
a convenience: a machine with no `unit` has no power operations, and labview
does not guess a unit name from an id. Section 12 requires the inventory to
be the id-to-unit mapping.

Unknown settings are kept and shown verbatim on the details tab, so a
producer newer than labview does not break the load.

`serial` is a unix socket path when labview runs on the hypervisor, or
`host:port` when it doesn't. Either is a `net.Dial`, so the broker does not
care which. `control` is the QMP socket of the machine, and it is classified
the same way.

None of `vnc`, `serial` and `control` reaches the browser. Clients name a
machine by `id` only, so a developer cannot dial a console the inventory did
not give them.

The details tab shows every other field verbatim, so a new address field
joins the withheld set in the same change that makes it a known field. Until
it does, the verbatim rule publishes it to every viewer.

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
one browser tab. At that point the wall switches to periodic screenshots and
opens RFB only on click. The tile is a component that renders either, so
`-tile-mode=screenshot` is a config change and not a rewrite. The frames
come from QMP `screendump`; section 12 says why.

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

There is no logs tab. labview concerns itself with the VMs. A host's journal
is host business, even when it is scoped to one unit. The serial transcript
carries what a reader needs, and labview records it and replays it.

The removal costs one case. A VM that fails to start never opens its serial
socket, so its reason lives in the journal alone. labview shows that machine
as inactive, and the developer reads the reason on the hypervisor. That is a
deliberate trade, not an oversight.

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
unit, its cgroup, its command line. That is all readable from the hypervisor
without libvirt and without an agent, which is the point.

labview runs no program at all. Every host interaction is a socket, a file
read or a D-Bus call. Keep it that way. A new facility that wants to run a
tool needs a better reason than convenience.

The two disk sizes come from the image file. labview reads the virtual size
from the qcow2 header, and it reads the actual size from the blocks the file
occupies. A file without the qcow2 magic is raw, and a raw image presents
its own length. Both numbers are the numbers `qemu-img` reports, so a reader
who checks by hand sees the same values.

The actual size counts one file. An overlay therefore reports what the
overlay holds, and it names its backing file, because that is where the rest
of the content lives.

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

`deploy/` holds the three pieces this section and section 12 describe.

| File | What it is |
|---|---|
| `labview.service` | labview as its own user, not root, hardened |
| `50-labview-units.rules` | polkit: unit management over just the lab's units |
| `nginx-labview.conf` | TLS, SSO, and the headers labview depends on |

Read the comments in `nginx-labview.conf` before writing another proxy
configuration. Three details are easy to get wrong, and two of them fail in
a way that looks like a labview bug.

- The original `Host` header must be forwarded, or the websocket origin
  check refuses every console.
- `X-Forwarded-User` must be set on the websocket upgrade as well as on an
  ordinary request. Set on plain HTTP only, every console and serial socket
  is unidentified, so no lease matches the person holding it and nobody can
  type.
- A client-supplied `X-Forwarded-User` must be discarded. labview trusts the
  header completely: it is the only thing that names the lease holder and the
  only thing in the audit trail. A proxy that forwards the header a caller
  sent lets that caller name themselves.

### What a harness presents

A harness needs a name for the same reason a human does: the lease names one
holder. The proxy issues it one in the form `harness/<name>/<run-id>`, so a
CI harness arrives as `harness/ci/4812`.

The run id is part of the name on purpose. With one name for every run of a
harness, two runs look like one holder. They contend for a single lease, and
each one takes the machine from the other mid-scenario. That failure is
silent, and it reads as a flaky test. A name for each run cannot collide.

The proxy issues the name from a token the harness presents, which is a
token the CI system already holds. It maps that token to a name exactly as
it maps a person's session to a user name. A caller never sets its own name,
because labview trusts the header completely.

labview needs no change for this. It takes the name the header carries, shows
that holder in the UI, and writes it to the activity trail of section 4. The
run id in the trail is the run in the CI system, which is what makes the
trail worth reading after a failure.

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

asciicast stores output as JSON strings, which must be UTF-8, while a serial
read can split a multi-byte character. The capture holds back a trailing
partial character to rejoin it with the next chunk, and it replaces only
genuinely invalid bytes. The websocket path does no such thing: it forwards
raw bytes and lets the browser decode incrementally.

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

### The deployment is a systemd unit

labview runs on the hypervisor as a systemd unit. There is no container
image.

Host access is local. labview reads the QEMU socket directory, the process
list and the system bus, and it asks systemd to start and stop units. The
targeted SELinux policy of Enterprise Linux denies `container_t` each one,
and no boolean opens them. A container reaches them through `spc_t` and the
process namespace of the host, and `spc_t` carries
`unconfined_domain_type`. That is a wider boundary than a unit, not a
narrower one.

The unit gives the boundary instead. `ProtectSystem=strict`, `PrivateTmp`,
`NoNewPrivileges`, a `CapabilityBoundingSet` and a scoped `ReadWritePaths`
bound what labview reaches. The polkit rule of section 12 bounds the units
it manages, and polkit matches the user name that the host sees.

Packaging is the other half of the reason. The binary is static and the
browser application is embedded, so an image carries one file and isolates
no dependency. A release attaches that binary for Linux and macOS on both
architectures.

---

## 12. Power, input and capture

### Power goes through systemd

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

That rule is `deploy/50-labview-units.rules`, and its default pattern accepts
`qemu-<machine>.service` and the instance form `qemu-vm@<machine>.service`.
The instance form is what `maglo.qemu` writes, so the shipped rule matches a
lab that the collection built. A default that matches nothing is worse than a
wide one here: polkit returns `NOT_HANDLED` for a unit the rule skips,
`NOT_HANDLED` denies, and every power click then fails in a way that reads as
a labview fault. A deployer narrows the pattern to the lab's own naming.

Restart is the only destructive thing in an otherwise read-mostly tool, so
it confirms in the UI and is logged with the identity from the proxy.

### Input and capture go through QMP

The choice above is about power alone. Input and capture are a different
question, and the answer is the QMP socket that the `control` field of
section 6 names. labview sends a key chord with `send-key` and takes a frame
with `screendump`. It runs no other QMP command, and power stays with
systemd.

`POST /api/machines/{id}/keys` sends one chord: QEMU presses every key of
one call together and releases them together. It takes the write lease, the
same one that gates the framebuffer and the serial line, because the
keyboard is the keyboard whichever way it is reached. A chord is input, so
it also pushes the expiry of that lease out.

`GET /api/machines/{id}/screenshot` needs no lease. The wall is view-only
for everyone and shows every machine's screen already, so a capture of that
screen is a read, and read is free. The screenshot wall of section 7 is the
case that decides it: nobody holds a lease on forty machines.

`screendump` writes PNG only. The same frame measures 10,790 bytes as PNG
and 864,015 bytes as PPM, and QEMU defaults to PPM: the filename extension
is not consulted. QEMU writes the file itself, as its own user and in its
own filesystem namespace, so the path is absolute and the directory belongs
to QEMU as well as to labview. `-screenshot-dir` names it, and the unit
gives QEMU write access to it. It cannot be labview's own `/tmp`, because
the unit sets `PrivateTmp`. labview reads the file and removes it: a frame
of somebody's screen does not stay on disk.

A QMP client must skip asynchronous `event` lines before it matches a reply.
A client that takes the next line for its reply reads the wrong one for
every command after it, and the symptom looks exactly like `screendump`
returning before the file exists.

A QMP socket takes one client at a time, like a serial chardev. The serial
line needs a broker because output arrives whether or not anybody listens.
QMP does not: every message answers a request. So labview dials for one
exchange and closes, and it lets one exchange at a time onto a machine.

An error on the control socket names the socket, so the client is told that
the machine did not answer and the text stays in labview's log. QEMU's own
refusal is different: it names the command and the parameter, never the
socket, so a harness reads it.

---

## 13. Boundaries

labview is a standalone service. It does not assume Ansible, and nothing in
it should. Whether the collection deploys it and writes its inventory is a
deployment question to answer later, and the design stays usable either way.

Three interfaces, and they are the whole contract:

**Inventory in.** A directory of YAML files in the shape of §6, one for each
machine. labview watches the directory and follows what it finds. Any
producer will do — a playbook, a script, a person with an editor.

**Host access out.** An internal interface, because the channels split two
ways. Framebuffer and serial are dialable over the network. The QEMU command
line, the unit state and power operations are not — they are local to the
hypervisor.

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
