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

# osewa release and the SHA-256 of its gzipped Linux builds, from SHA256SUMS of
# https://github.com/rjyo/osewa/releases/tag/v0.1.4. Must match OsewaVersion
# and the checksums in pkg/openshell/osewa.go and the OSEWA_* arguments of
# deploy/sandbox/Containerfile (TestOsewaPinMatchesContainerfiles checks).
ARG OSEWA_VERSION=0.1.4
ARG OSEWA_SHA256_AMD64=160667bfd60e999b4059b30dfad74fd38c8ab0da413668ec176678a4a0d4bb58
ARG OSEWA_SHA256_ARM64=5a08537f6ebd2b2b6870a54c2c6a78cb26a16d1426e261ab5b8b896d24967325

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

# --- osewa -------------------------------------------------------------------
# Both Linux builds of the pinned osewa, whatever the target: Soft Serve
# uploads the one matching a sandbox's architecture when the sandbox image
# lacks osewa (sessions.osewa.binary_dir). Kept gzipped as released, so Soft
# Serve and the sandbox can check them against the published checksums.
FROM --platform=$BUILDPLATFORM ${RUNTIME_IMAGE} AS osewa

ARG OSEWA_VERSION
ARG OSEWA_SHA256_AMD64
ARG OSEWA_SHA256_ARM64

RUN apk add --no-cache curl

RUN set -eu; \
    mkdir -p /out/osewa; \
    for arch in amd64 arm64; do \
      case "${arch}" in \
        amd64) sum="${OSEWA_SHA256_AMD64}" ;; \
        arm64) sum="${OSEWA_SHA256_ARM64}" ;; \
      esac; \
      gz="osewa-linux-${arch}.gz"; \
      curl -fsSL --proto '=https' --tlsv1.2 -o "/out/osewa/${gz}" \
        "https://github.com/rjyo/osewa/releases/download/v${OSEWA_VERSION}/${gz}"; \
      echo "${sum}  /out/osewa/${gz}" | sha256sum -c -; \
    done

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

# The osewa builds Soft Serve uploads to sandboxes (sessions.osewa.binary_dir).
COPY --from=osewa /out/osewa/ /usr/local/share/soft-serve/osewa/

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
