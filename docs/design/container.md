# container image — design

labview is also a container image. The image holds one static binary and
nothing else. `docs/design/labview.md` stays the authority on the service
itself, and this document covers only the image and the registry.

---

## 1. Why an image

The systemd unit in `deploy/` is the full deployment on a hypervisor. The
image serves the cases around it:

- A demo or UI work with `-host-access fake`, on a laptop with no lab.
- A test harness that wants labview next to it, for one run.
- A host that runs its services with podman.

The image adds a way to ship the binary. It does not change section 10: a
proxy still terminates TLS, does SSO and sets the identity header.

---

## 2. What the image holds

The base is `scratch`. The binary is static and the browser application is
embedded, so there is nothing else to install.

| Path | Content |
|---|---|
| `/usr/local/bin/labview` | The binary, with the UI inside it. |
| `/etc/labview` | The mount point for the inventory directory. |
| `/var/lib/labview/recordings` | The recordings directory. |
| `/etc/passwd` | One account, `labview`, uid 65532. |

The image runs as uid 65532, not root. The two directories belong to that
uid, because a mount point that the process cannot write is a startup
failure that reads like a labview bug.

`scratch` has no shell and no package manager, so the image is about eight
megabytes and has no third party code in it. The cost is section 3.

---

## 3. What the container cannot do

Local host access runs two programs of the host, and the image has neither.

| Feature | Program | Result in the container |
|---|---|---|
| The logs tab | `journalctl` | The tab reports `journalctl not found`. |
| Disk sizes on the details tab | `qemu-img` | The tab warns that sizes are unavailable. |

labview already treats both as optional and says why the data is missing, so
the container degrades in the open. Everything else works: the wall, the
console, the serial line, the recordings, the leases and power operations.

A deployment that needs the logs tab uses the systemd unit. Adding systemd
and QEMU to the image to get one tab back costs more than the tab is worth.

---

## 4. The image sets no listen address

labview binds loopback by default, and the image keeps that default. An
image that bound every interface would put the wall on the network with no
proxy and no identity in front of it, which section 10 forbids.

So the container runs with the network of the host, and the proxy reaches it
on loopback exactly as it reaches the unit. A deployment that publishes a
port instead asks for it with `-listen 0.0.0.0:8080`, and puts the proxy in
front of that port.

---

## 5. What the host must lend the container

Local host access is local. The container needs these from the host, and the
lab decides which of them it wants.

| Need | Mount or option |
|---|---|
| The serial sockets | The QEMU socket directory, read and write. |
| Power operations | The system bus socket, plus the polkit rule. |
| The QEMU command lines | The process namespace of the host. |
| The inventory | The inventory directory, read only. |
| The recordings | A writable directory. |

The polkit rule matches a user name. A container user is not a host user, so
the rule needs the identity that the host sees for the container process.
This is the reason the unit stays the simpler deployment on a hypervisor.

---

## 6. Registry and tags

The image is `ghcr.io/maglo/qemu-lab-manager/labview`. The registry of the
repository needs no second account and no second secret.

| Tag | Meaning |
|---|---|
| `latest` | The head of `main`. |
| `main` | The same image, named by branch. |
| `sha-<short>` | One commit, for a deployment that pins. |

`labview -version` prints the full commit of the image, because a tag can
move and a commit cannot.

A pull request builds the image and pushes nothing. The build is the check:
a broken Dockerfile fails the pull request, and only `main` publishes.

---

## 7. The build

The build stage runs on the architecture of the runner and cross-compiles.
Go cross-compiles in seconds, and emulation of the same build takes minutes.
The image is built for `linux/amd64` and `linux/arm64`.

The job lives in `ci.yml` and the `ci passed` gate depends on it. A separate
workflow would need a second gate job in the ruleset of `main`, and the
ruleset names two gate jobs today.
