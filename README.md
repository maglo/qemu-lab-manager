# qemu-lab-manager

The tooling for a QEMU lab. It builds one Go binary, `labview`.

## labview

You run QEMU virtual machines, and each machine has its own VNC port. To see
four machines you open four noVNC tabs. You also track which port belongs to
which machine.

labview replaces those tabs with one wall. The wall shows a live screen for
every machine at once. You click a machine to expand it and to take control.
You attach to the serial line when a scenario needs it. One Go binary, one
inventory directory, no database.

This file tells you how to build labview and how to run it. It does not
describe how labview works. Each document below is the one place that
describes its subject.

## Build

Nothing but a Go toolchain. The browser application, noVNC and xterm are all
embedded in the binary.

    go build -o labview ./cmd/labview
    go test -race ./...

A second layer of checks drives labview in a real browser against a fake lab:

    test/lab/run.sh

It needs Node and Python in addition to Go, and two one-time installs.
[`test/lab/README.md`](test/lab/README.md) says how to set it up, why the
layer exists, and how to add a check.

## Run

    ./labview -inventory /etc/labview/inventory.d -host-access local

    ./labview -help            # every setting, with its default

`-help` is the list of settings. On a laptop with no lab attached,
`-host-access fake` invents plausible machine details so the UI can be worked
on. It is never a fallback and has to be asked for.

Each commit on `main` also publishes an image:

    podman pull ghcr.io/maglo/qemu-lab-manager/labview:latest

## Read next

| Document | What it covers |
|---|---|
| [`docs/design/labview.md`](docs/design/labview.md) | What labview is and why. The inventory format, the HTTP and websocket surface, the serial broker, the write lease, power operations, deployment and trust. |
| [`docs/design/container.md`](docs/design/container.md) | The image: what it holds, what it cannot do, and what the host must lend it. |
| [`docs/design/changelog.md`](docs/design/changelog.md) | How a change reaches the changelog, and how a release is cut. |
| [`docs/design/checks.md`](docs/design/checks.md) | Every workflow and every job, and the commands to run before a push. |
| [`test/lab/README.md`](test/lab/README.md) | The fake lab, and how to add a browser check. |
| [`CHANGELOG.md`](CHANGELOG.md) | What changed in each release. |
| [`CLAUDE.md`](CLAUDE.md) | The rules for anyone who works in this repository. |

A design document gives the reason for a decision. `CLAUDE.md` gives the
layout of the repository and names the document of each part.
