# Builds Soft Serve from source into a small non-root runtime image.
#
# The Dockerfile next to this file is the goreleaser one: it copies a prebuilt
# binary and is not meant to be built by hand. Use this file instead:
#
#   podman build -f Containerfile \
#     --build-arg VERSION="$(git describe --tags --always --dirty)" \
#     --build-arg COMMIT_SHA="$(git rev-parse HEAD)" \
#     --build-arg COMMIT_DATE="$(git log -1 --format=%cI)" \
#     -t ghcr.io/danielboothcloud/soft-serve:<tag> .
#
# See deploy/README.md for the full build and push commands.

# Base images are pinned. go.mod requires go >= 1.26.6.
ARG GO_IMAGE=docker.io/library/golang:1.26.11-alpine3.24
ARG RUNTIME_IMAGE=docker.io/library/alpine:3.24.4

# --- build -------------------------------------------------------------------
# The build stage runs on the build host's architecture and cross-compiles.
# That is possible because the binary is pure Go (SQLite is modernc.org/sqlite).
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build

ARG TARGETOS
ARG TARGETARCH

# Stamped into the binary; see the Version/CommitSHA/CommitDate vars in
# cmd/soft/main.go. All three may be left empty.
ARG VERSION=""
ARG COMMIT_SHA=""
ARG COMMIT_DATE=""

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -trimpath -buildvcs=false \
      -ldflags "-s -w -X main.Version=${VERSION} -X main.CommitSHA=${COMMIT_SHA} -X main.CommitDate=${COMMIT_DATE}" \
      -o /out/soft ./cmd/soft

# --- runtime -----------------------------------------------------------------
FROM ${RUNTIME_IMAGE}

# git:     Soft Serve shells out to git for every repository operation.
# bash:    the git hooks Soft Serve writes into each repository are bash scripts.
# openssh: the ssh client, used by git for mirrors pulled over SSH.
RUN apk add --no-cache git bash openssh-client-default

# A fixed, numeric, non-root identity (10001:10001). The Helm chart's pod
# security context uses the same IDs, and its fsGroup makes the data volume
# writable.
RUN addgroup -g 10001 -S soft \
 && adduser -u 10001 -S -G soft -h /home/soft -s /bin/bash soft \
 && mkdir -p /soft-serve \
 && chown 10001:10001 /soft-serve

# The hooks call the binary through SOFT_SERVE_BIN_PATH, which Soft Serve sets
# to its own path. Keep it here so existing repositories keep working.
COPY --from=build /out/soft /usr/local/bin/soft

# All state lives here: repositories, the SQLite database, SSH host keys.
ENV SOFT_SERVE_DATA_PATH="/soft-serve"
# Workaround to prevent slowness when running with a tty (from the Dockerfile).
ENV CI="1"

WORKDIR /soft-serve
VOLUME /soft-serve

# SSH, HTTP, stats, git daemon.
EXPOSE 23231/tcp 23232/tcp 23233/tcp 9418/tcp

USER 10001:10001

ENTRYPOINT ["/usr/local/bin/soft", "serve"]
