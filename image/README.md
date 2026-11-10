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

## Join a cluster at boot

The image has cloud-init. The labview unit reads `LABVIEW_ARGS` from
`/etc/labview/node.env`. A flag in `LABVIEW_ARGS` replaces the same
flag of the unit. See `docs/design/cluster.md` section 5.

Give each node this user data. Change the addresses, the name and the
token.

```yaml
#cloud-config
write_files:
  - path: /etc/labview/cluster-token
    owner: root:labview
    permissions: '0640'
    content: |
      change-this-token
  - path: /etc/labview/node.env
    permissions: '0644'
    content: |
      LABVIEW_ARGS=-listen 0.0.0.0:8080 -cluster-token-file /etc/labview/cluster-token -join http://10.0.0.1:8080 -node-name node2 -advertise-url http://10.0.0.2:8080
```

The API node gets the same token file. Its `node.env` has `-listen`
and `-cluster-token-file`, and no `-join`.

`-listen 0.0.0.0:8080` opens labview on each network of the node.
Limit port 8080 to the cluster network with a security group.

A node needs KVM. A VM node needs a flavor with nested virtualization.
A node with no KVM joins, and it shows as not ready.

A physical node has no user data. Write the two files by hand, and
restart `labview.service`.
