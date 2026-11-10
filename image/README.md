# labview-node image

A Fedora bootc image that boots as a lab hypervisor node. The image is
the host OS. labview runs as a unit on it. See
[`docs/design/cluster.md`](../docs/design/cluster.md) section 3.

## Registry

| Item | Value |
|---|---|
| Image | `ghcr.io/maglo/qemu-lab-manager/labview-node` |
| Base | `quay.io/fedora/fedora-bootc:45` |
| Visibility | Public |
| Cost | GHCR container storage and bandwidth are free today |

A push to `main` publishes `:latest`. A `v*` tag publishes that version.

## Build

From the repository root:

```sh
podman build -t ghcr.io/maglo/qemu-lab-manager/labview-node:latest \
  -f image/Containerfile .
```

Docker Buildx works the same way with `docker build`.

## Boot a disk (local)

Turn the container into a qcow2 with bootc-image-builder, then start it
in QEMU. The base image has no login by default; pass a config that
adds a user when you need a shell.

```sh
mkdir -p output
sudo podman pull ghcr.io/maglo/qemu-lab-manager/labview-node:latest
sudo podman run --rm --privileged \
  --security-opt label=type:unconfined_t \
  -v "$PWD/output:/output" \
  -v /var/lib/containers/storage:/var/lib/containers/storage \
  quay.io/centos-bootc/bootc-image-builder:latest \
  --type qcow2 --rootfs xfs \
  ghcr.io/maglo/qemu-lab-manager/labview-node:latest
```

labview listens on `127.0.0.1:8080` inside the guest. Forward a host
port or put a proxy in front, as `deploy/nginx-labview.conf` shows.

## Machine create and destroy

`POST /api/machines` creates a machine. `PATCH` updates desired
hardware. `DELETE /api/machines/{id}` destroys one the API created.
The image ships `qemu-vm@.service` and swtpm helpers for TPM machines.
See `docs/design/cluster.md` section 4.

## What this image does not do yet

- Read cloud-init user data.
- Join a cluster at boot.
