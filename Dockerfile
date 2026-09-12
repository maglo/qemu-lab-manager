# syntax=docker/dockerfile:1

# labview is one static binary with the browser application embedded, so the
# image carries the binary and nothing else. See docs/design/container.md.

# The build stage runs on the architecture of the runner and cross-compiles.
# Go cross-compiles in seconds, and emulation of the build takes minutes.
#
# The tag is a floor, and it must not fall behind the go line of go.mod. A
# newer toolchain in go.mod makes the build fetch that toolchain.
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
	--mount=type=cache,target=/root/.cache/go-build \
	CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
	go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
	-o /out/labview ./cmd/labview

# The final image has no shell, so the build stage makes the mount points and
# the account. labview runs as this user and writes only the recordings.
RUN mkdir -p /rootfs/etc/labview /rootfs/var/lib/labview/recordings \
	&& printf 'labview:x:65532:65532:labview:/var/lib/labview:/sbin/nologin\n' \
	>/rootfs/etc/passwd \
	&& printf 'labview:x:65532:\n' >/rootfs/etc/group

FROM scratch

COPY --from=build /rootfs/etc /etc
COPY --from=build --chown=65532:65532 /rootfs/var /var
COPY --from=build /out/labview /usr/local/bin/labview

USER 65532:65532

# No default -listen. labview binds loopback, and design section 10 puts TLS
# and SSO in a proxy in front of it. An image that bound every interface would
# put the wall on the network with no identity in front of it.
ENTRYPOINT ["/usr/local/bin/labview"]
