# syntax=docker/dockerfile:1

# ---- build ----------------------------------------------------------------
# Pinned to the same major the project targets. The toolchain layer is the
# only thing that needs a full Go distribution; everything else is static.
FROM golang:1.25-alpine AS build

# git is only required when the build resolves VCS information for the
# version stamp, and it is removed from the final image anyway.
RUN apk add --no-cache git ca-certificates

WORKDIR /src

# Dependencies first: this layer is cached until go.mod or go.sum changes.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH=amd64

# CGO_ENABLED=0 produces a static binary that runs on scratch and on any
# libc, which matters because the image must reach the host's /etc/hosts.
# -trimpath keeps build paths out of the binary; -s -w drop the symbol table
# and DWARF data.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/docker-hoster-injector \
      ./cmd/docker-hoster-injector

# ---- runtime --------------------------------------------------------------
# scratch has no shell, so a crash is reported through the exit code and the
# container logs only, never through an interactive shell.
FROM scratch

# Root certificates are copied in even though the agent does not make TLS
# requests today: the Docker client may be pointed at a TLS endpoint via
# DOCKER_HOST, and a missing CA bundle would be a baffling failure.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

COPY --from=build /out/docker-hoster-injector /docker-hoster-injector

# The agent rewrites the host's hosts file, which requires root. It needs
# nothing else, so every Linux capability is dropped.
USER 0:0

ENV DNS_SUFFIX=docker.local \
    HOSTS_FILE=/etc/hosts \
    HOSTS_MOUNT_MODE=file \
    TARGET_MODE=both \
    LOG_FORMAT=json \
    LOG_LEVEL=info \
    WEB_ENABLED=true \
    WEB_ADDR=:8080

EXPOSE 8080

# SIGTERM is what "docker stop" sends, and the agent uses it for a graceful
# shutdown that flushes the pending hosts file update.
STOPSIGNAL SIGTERM

ENTRYPOINT ["/docker-hoster-injector"]
