# qemu-lab-manager

## labview

A console wall for the QEMU lab. Developers see every machine at a glance,
open one to drive it, and attach to serial when a scenario needs it. One Go
binary, one inventory file, no database.

The design is [`docs/design/labview.md`](docs/design/labview.md) and it is
the authority on why things are the way they are. This file is how to build
and run it.

### Build

Nothing but a Go toolchain. The browser application, noVNC and xterm are all
embedded in the binary.

    go build -o labview ./cmd/labview
    go test -race ./...

There is a second layer of checks that drives labview in a real browser
against a fake lab -- a minimal RFB server, unix sockets printing a boot, and
a proxy asserting an identity:

    test/lab/run.sh

It needs Node and Python in addition to Go, and two one-time installs: `npm
install` in `test/lab`, then `npx playwright install chromium`.

Unit tests cannot reach the place where labview meets a browser, and that is
where several of the more interesting bugs were; see `test/lab/README.md`.
Both layers run in CI on every pull request.

### Run

    ./labview -inventory ./inventory.json -host-access local

    ./labview -help            # every setting, with its default

On a laptop with no lab attached, `-host-access fake` invents plausible
machine details so the UI can be worked on. It is never a fallback and has
to be asked for: a wall silently showing invented details would be worse
than one saying it cannot reach the host.

### The inventory

Written by something else -- a playbook, a script, a person with an editor
-- and re-read whenever its mtime changes. A malformed file is logged and
ignored, leaving the previous inventory live.

```json
[
  {
    "id": "el9-build",
    "name": "el9-build",
    "host": "kvm01",
    "vnc": "10.20.0.11:5901",
    "serial": "/run/qemu/el9-build-serial.sock",
    "unit": "qemu-el9-build.service",
    "notes": "AlmaLinux 9"
  }
]
```

`id` is the only name a client ever uses, and it becomes a path element and
a capture filename, so it is validated at load: letters, digits, dot,
underscore and hyphen, starting with a letter or digit.

`serial` is a unix socket path when labview runs on the hypervisor, or
`host:port` when it does not. Either is a dial.

`unit` is what makes power operations possible, and it is an allowlist
rather than a convenience: a machine with no unit has no power operations,
and labview will not guess a unit name from an id. It is the one field the
design's own example omits -- section 6 does not list it, but section 12
requires the inventory to be the id-to-unit mapping.

Unknown fields are kept and shown verbatim on the details tab, so a producer
newer than labview does not break the load.

### Deploying

`deploy/` holds the three pieces section 10 and section 12 describe:

| File | What it is |
|---|---|
| `labview.service` | labview as its own user, not root, hardened |
| `50-labview-units.rules` | polkit: unit management over just the lab's units |
| `nginx-labview.conf` | TLS, SSO, and the headers labview depends on |

Read the comments in `nginx-labview.conf` before writing your own proxy
configuration. Three details there are easy to get wrong and two of them
fail in ways that look like labview bugs:

- The original `Host` header must be forwarded, or the websocket origin
  check refuses every console.
- `X-Forwarded-User` must be set on the websocket upgrade as well as on
  ordinary requests. Set only on plain HTTP, every console and serial socket
  is unidentified, so no lease matches the person holding it and nobody can
  type.
- Any client-supplied `X-Forwarded-User` must be discarded. labview trusts
  it completely: it is the only thing naming the lease holder and the only
  thing in the audit trail.

### The container image

Each commit on `main` publishes an image to the registry of this repository:

    podman pull ghcr.io/maglo/qemu-lab-manager/labview:latest

It is the binary on `scratch`, about eight megabytes, running as uid 65532.
The image sets no listen address, so labview binds loopback as it always
does and the proxy goes in front of it. On a laptop, with no lab:

    podman run --rm --network host \
        -v ./inventory.json:/etc/labview/inventory.json:ro \
        ghcr.io/maglo/qemu-lab-manager/labview:latest \
        -host-access fake

Two things do not work in the container, because the image holds no host
tools: the logs tab needs `journalctl`, and the disk sizes on the details
tab need `qemu-img`. Both say what is missing. Everything else works, and
on a hypervisor the container also needs the QEMU socket directory, the
system bus and the process namespace of the host.

The systemd unit above is still the full deployment.
[`docs/design/container.md`](docs/design/container.md) gives the reasons and
lists the mounts.

### The API

Everything the UI shows is also JSON. The UI is one consumer, a test harness
is another, and neither gets a privileged path.

| Endpoint | Purpose |
|---|---|
| `GET /api/machines` | inventory plus liveness and lease holder |
| `GET /api/machines/{id}` | the whole record: details, disks, network, unit, recordings, activity |
| `GET /api/machines/{id}/logs?lines=N` | recent journal lines |
| `GET /api/machines/{id}/recordings` | past serial captures |
| `GET /api/machines/{id}/recordings/{name}` | one capture, asciicast v2 |
| `GET /api/machines/{id}/activity` | who attached when, who holds control |
| `POST /api/machines/{id}/lease` | `{"action":"acquire"\|"renew"\|"release"}` |
| `POST /api/machines/{id}/power` | `{"op":"start"\|"stop"\|"restart"}` |
| `GET /ws/console/{id}` | binary RFB, proxied to the VNC port |
| `GET /ws/serial/{id}?write=1` | serial stream, scrollback first |
| `GET /ws/logs/{id}` | journal tail |

#### Driving a machine from a harness

Attach with `?write=1` to take the write lease at attach time. The socket
carries two message types and they must not be confused:

- **binary frames** are what the VM printed, raw bytes
- **text frames** are labview talking about itself: the machine came up, the
  machine went away, someone else has the keyboard, your control is about to
  expire

Mixed into one stream a status message would look like something the VM
printed, and would land in the recording and in anything grepping the
output. Keep them apart.

The server does no matching. There is no server-side expect, no
regex-on-output, no "wait for prompt": a harness gets raw bytes and does its
own matching, in its own language, with its own timeouts.

Opening a machine connects you to labview's viewer, not to the VM. The
viewer is already attached to the serial port and stays attached, so a
harness can open a machine that is switched off, start it, and watch it boot
from the first line without racing it.

### Recordings

Serial output is captured to asciicast v2, one file per VM run, rotated when
the broker reconnects because a reconnect is a VM lifecycle boundary.
Retention is by count, age and total size, with a per-file ceiling as a
backstop against a machine that spews.

Do not point logrotate at these files. labview owns the file descriptor and
rotates in process; copytruncate against a live writer loses data.

asciicast stores output as JSON strings, which must be UTF-8, while a serial
line can split a multi-byte character across reads. The capture holds back a
trailing partial character to rejoin it with the next chunk and replaces
only genuinely invalid bytes. The websocket path does no such thing: it
forwards raw bytes and lets the browser decode incrementally.

### What is not built

- **Screenshot tiles.** The tile is a component with a pluggable renderer
  and `-tile-mode=screenshot` selects the other one, as section 7 asks, but
  there is no capture source behind it yet. Getting a screenshot server-side
  means either QMP screendump, which section 12 ruled out in favour of
  systemd, or decoding RFB in the server. Neither is settled, so the
  renderer says what it is waiting for rather than inventing an endpoint.
- **Stealing a lease**, and **framebuffer recording**, both deferred by
  section 9.
- **Aggregating several hypervisors.** One labview per hypervisor is the v1
  shape. The host access interface is the seam a future aggregating labview
  would implement by speaking this same API to another labview.

### Layout

    cmd/labview/           the command
    internal/inventory/    the machine list, and reloading it
    internal/serial/       the broker: ring buffer, fan-out, transcripts
    internal/lease/        the write lease
    internal/host/         the hypervisor: systemd, /proc, journal
    internal/rfb/          just enough RFB to tell input from viewing traffic
    internal/api/          HTTP and websockets
    internal/web/          the browser application, embedded
    internal/activity/     who did what
    deploy/                unit file, polkit rule, proxy configuration
    test/lab/              a fake QEMU lab, and browser-driven checks
    Dockerfile             the container image
    .github/workflows/     CI: build, vet, race tests, browser checks,
                           and the image
