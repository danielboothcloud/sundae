# Deploying Sundae with OpenShell on Kubernetes

One helmfile installs three releases, in this order:

| Release | Chart | Namespace |
| --- | --- | --- |
| `agent-sandbox` | `kubernetes-sigs/agent-sandbox` Helm chart at tag `v1.0.10` | `agent-sandbox-system` |
| `openshell` | `oci://ghcr.io/nvidia/openshell/helm-chart` `0.1.8` | `namespace` (default `sundae`) |
| `sundae` | [`helm/sundae`](helm/sundae) in this repository | the same namespace |

Sundae and the OpenShell gateway share a namespace so Sundae can mount
the gateway's client certificate (`openshell-client-tls`). Sundae's whole
configuration is rendered into a ConfigMap and mounted read-only at
`/etc/sundae/config.yaml`.

Nothing here has been deployed to a cluster yet. See
[What has not been verified](#what-has-not-been-verified).

## Prerequisites

- A cluster whose CNI **enforces NetworkPolicy** (Cilium does). OpenShell
  relies on it to confine sandboxes.
- A default StorageClass, or the StorageClass values set (see below).
- `helm` 4, `helmfile` 1.8 (`mise x helmfile@1.8.2 -- helmfile ...`), `git`
  (helmfile fetches the Agent Sandbox chart from GitHub), and the `helm-diff`
  plugin if you want `helmfile diff` or `apply`.
- `podman` or `docker` to build the images, and push access to the registry.

## 1. Build and push the images

CI builds both images for linux/amd64 on every push to `main` or `develop`
(`.github/workflows/images.yml`) and pushes them to
`ghcr.io/danielboothcloud/sundae` and `ghcr.io/danielboothcloud/sundae-sandbox`,
tagged with the short commit sha and the branch name. Build by hand only when
you need another registry or architecture:

Run from the repository root. Build for the architecture of the cluster nodes.

### Sundae

```sh
IMAGE=ghcr.io/danielboothcloud/sundae   # placeholder: confirm the registry path
TAG="$(git rev-parse --short HEAD)"

podman build -f Containerfile \
  --platform linux/amd64 \
  --build-arg VERSION="$(git describe --tags --always --dirty)" \
  --build-arg COMMIT_SHA="$(git rev-parse HEAD)" \
  --build-arg COMMIT_DATE="$(git log -1 --format=%cI)" \
  -t "$IMAGE:$TAG" .

podman login ghcr.io
podman push "$IMAGE:$TAG"
```

`docker build` takes the same arguments. The repository's `Dockerfile` only
packages a prebuilt binary and is not used here. The image also carries the pinned osewa
release's Linux builds (amd64 and arm64, checksum-verified at build time) in
`/usr/local/share/sundae/osewa`, which Sundae uploads to sandboxes
whose image has no osewa (`sessions.osewa.binary_dir`).

### Sandbox

The image the OpenShell sandboxes run, built from
[`sandbox/Containerfile`](sandbox/Containerfile): Ubuntu 24.04 (the base
OpenShell uses for its own default sandbox image) with `git`, `jj` 0.47.1,
[osewa](https://github.com/rjyo/osewa) 0.1.4 (which keeps each session's
program running after the SSH client disconnects), `bash`,
`ca-certificates`, `curl`, `openssh-client`, `less`, `nano` and `vi`. Its
comments list what OpenShell expects of a sandbox image and what Sundae
keeps in it.

```sh
SANDBOX_IMAGE=ghcr.io/danielboothcloud/sundae-sandbox   # placeholder: confirm the registry path
TAG="$(git rev-parse --short HEAD)"

podman build -f deploy/sandbox/Containerfile \
  --platform linux/amd64 \
  -t "$SANDBOX_IMAGE:$TAG" deploy/sandbox

podman push "$SANDBOX_IMAGE:$TAG"
```

Use `--platform linux/arm64` for arm64 nodes. For both at once, build a
manifest list with `--platform linux/amd64,linux/arm64 --manifest
"$SANDBOX_IMAGE:$TAG"` instead of `-t`, then `podman manifest push --all
"$SANDBOX_IMAGE:$TAG"`.

The image declares the user `sandbox`, UID and GID 10001, with `/sandbox` as
its home. On Kubernetes OpenShell runs sandboxes as its own sandbox UID/GID
(10001 unless the OpenShell chart's `server.sandboxUid` / `server.sandboxGid`
are set), not as the image's `USER`. If you set those, rebuild with
`--build-arg SANDBOX_UID=<uid> --build-arg SANDBOX_GID=<gid>` so the passwd
entry matches.

Set `openshell.sandboxImage` to `$SANDBOX_IMAGE:$TAG` in step 2.

## 2. Set the values

Edit [`environment.yaml`](environment.yaml), or pass the same keys with
`--state-values-set`.

| Value | Required | What it is |
| --- | --- | --- |
| `openshell.sandboxImage` | yes | Default image for the sandboxes Sundae starts, built in step 1. |
| `sundae.image.tag` | yes | The Sundae tag pushed in step 1. |
| `sundae.image.repository` | check | Defaults to the placeholder `ghcr.io/danielboothcloud/sundae`. |
| `sundae.initialAdminKeys` | in practice | Admin public keys. Without one nobody can administer the server. |
| `sundae.ssh.serviceType` | in practice | `LoadBalancer` or `NodePort` to reach SSH from outside the cluster. |
| `sundae.ssh.publicURL` | in practice | The external SSH address, shown in clone URLs. |
| `namespace` | no | Defaults to `sundae`. |
| `agentSandbox.enabled` | no | Set `false` when the cluster already has Agent Sandbox. |
| `sundae.storage.*`, `*.chartValues` | no | Volume size and StorageClass; free-form chart values. |

helmfile stops with a message naming the value when a required one is empty.
If the image is private, add a pull secret through
`sundae.chartValues.imagePullSecrets`.

## 3. Install

Run from this directory. `hf` stands for `mise x helmfile@1.8.2 -- helmfile`.

```sh
# Render everything without touching the cluster. The --api-versions flag
# stands in for the Agent Sandbox API, which the OpenShell chart checks for.
hf template --args="--api-versions=agents.x-k8s.io/v1beta1"

# 1. Agent Sandbox CRDs and controller. Skip when agentSandbox.enabled is false.
hf -l name=agent-sandbox sync

# 2. OpenShell, then Sundae.
hf sync
```

Install Agent Sandbox first: the OpenShell chart refuses to render until the
cluster serves the Sandbox API, and `hf diff` and `hf apply` render every
release before installing any. Once everything is installed, use `hf diff` and
`hf apply` for changes.

Afterwards:

- The gateway address Sundae uses is
  `https://openshell.<namespace>.svc.cluster.local:8080`.
- Sandboxes clone from
  `http://sundae-http.<namespace>.svc.cluster.local:23232`. The sandbox
  network policy must allow that host and port (see "Sandbox egress" in the
  top-level README, whose example uses this host for the default namespace
  `sundae`).
- SSH is the Service `sundae-ssh`, port 23231.

## Check the config before deploying

`sundae config check` loads a config file the way `sundae serve` does (the file,
then any `SUNDAE_*` variables as overrides, then validation), prints
`config OK: <path>` and exits 0, or lists every problem and exits 1. Unknown
keys are errors, reported with their line, so a misspelt key cannot be
silently ignored. It never writes a file. This renders the chart's ConfigMap,
with the same values helmfile uses, and checks it. Run it from the repository
root; it needs no cluster.

```sh
mise x helmfile@1.8.2 -- helmfile -f deploy/helmfile.yaml -l name=sundae \
    template --skip-deps \
    --state-values-set openshell.sandboxImage=ghcr.io/danielboothcloud/sundae-sandbox:abc1234 \
    --state-values-set sundae.image.tag=abc1234 \
  | mise x yq@4 -- yq 'select(.kind == "ConfigMap") | .data["config.yaml"]' \
  > /tmp/sundae-config.yaml
SUNDAE_DATA_PATH=/sundae go run ./cmd/sundae config check --file /tmp/sundae-config.yaml
```

Drop the `--state-values-set` flags if `environment.yaml` already has the
required values. To check the chart on its own, render it with `helm template
sundae deploy/helm/sundae -f <values> --show-only
templates/configmap.yaml` and pipe it through the same `yq` expression.
`SUNDAE_DATA_PATH` matches the chart's `dataPath`, which relative paths in
the config resolve against. Export any other `SUNDAE_*` variables the pod
gets through `extraEnv` too, since they override the file.

Files the config names (TLS files, SSH keys) need not exist where the check
runs, with one exception: when OpenShell is enabled and
`openshell.token_file` is set (as it is when
`sundae.chartValues.openshell.gatewayToken.secretName` is), the check
reads that file, as `sundae serve` does at startup. Outside the pod it fails
with `openshell: read token_file: ... no such file or directory`; to check the
rest, point it at a local file with `--state-values-set
sundae.chartValues.config.openshell.token_file=/tmp/token`. The same goes
for every `config.forges[].token_file`: the check reads each one. And for
`config.notifications.ntfy.token_file` when ntfy is enabled (set to
`/etc/sundae-ntfy/token` when
`sundae.chartValues.notifications.ntfyToken.secretName` is set), and for
`config.snapshots.s3.credentials_file` when S3 snapshots are enabled (set to
`/etc/sundae-s3/<key>`, or the directory `/etc/sundae-s3` with an
empty key, when `sundae.chartValues.snapshots.s3Credentials.secretName`
is set).

## Things to know

- **One pod.** Sundae is a single-replica StatefulSet on one
  ReadWriteOnce volume (SQLite and the repositories). The chart has no replica
  setting.
- **Config changes roll the pod.** The ConfigMap's checksum is a pod
  annotation. SSH sessions are dropped when the pod restarts.
- **No secrets in the ConfigMap.** The chart refuses `config.openshell.token`.
  Secret files come from existing Secrets (`openshell.clientTLS`,
  `openshell.gatewayToken`, `secretMounts`); a secret value with no file
  setting can be passed as a `SUNDAE_*` variable through `extraEnv`.
- **No gateway token by default.** The gateway authenticates Sundae by
  its client certificate. If it also needs a bearer token, put the token in a
  Secret in the namespace and set
  `sundae.chartValues.openshell.gatewayToken.secretName` (and `.key`,
  default `token`). The chart mounts that key at
  `/etc/openshell-token/<key>` and sets `openshell.token_file` to it. Sundae
  reads the file again for every `session` command, so a rotated
  Secret needs no restart once the kubelet has updated the mounted file.
- **Forges.** GitHub and GitLab accounts go under
  `sundae.chartValues.config.forges` (example in `environment.yaml` and
  the chart's `values.yaml`; semantics in the top-level README, "Forges").
  Each token is a key in an existing Secret, mounted as a directory through
  `sundae.chartValues.secretMounts` and named by `token_file`; Sundae
  rejects an inline `token`. Sundae needs egress to each forge's HTTPS
  host. Adding or changing a forge rolls the pod like any config change.
- **Notifications (ntfy).** Off by default. Turn them on under
  `sundae.chartValues.config.notifications` (example in
  `environment.yaml`; semantics in the top-level README, "Notifications").
  Quote the `"on":` key: Helm reads a bare `on:` as `true`, and the chart
  refuses to render that. For an access-controlled topic, put the ntfy
  access token in a Secret in the namespace and set
  `sundae.chartValues.notifications.ntfyToken.secretName` (and `.key`,
  default `token`). The chart mounts that key at `/etc/sundae-ntfy/<key>`
  and sets `notifications.ntfy.token_file` to it; Sundae reads the file
  for every message, so a rotated Secret needs no restart. The chart refuses
  an inline `notifications.ntfy.token`. Sundae needs egress to the ntfy
  server.
- **Session status and parking.** Sundae polls every running session's
  sandbox (`osewa local ls`, one exec each, every
  `sessions.status.poll_interval`) and parks idle sessions after
  `sessions.suspend.after` (semantics in the top-level README, "Session
  status" and "Parking idle sessions"). Parking needs a compute driver with
  OpenShell's suspend-resume capability. The OpenShell 0.1.8 Kubernetes driver
  does not have it (`openshell-driver-kubernetes/src/grpc.rs`,
  `suspend_sandbox`), so with this deployment the gateway refuses, Sundae
  logs that once and leaves the sandboxes running. Set
  `config.sessions.suspend.enabled: false` to skip the attempt.
- **S3 snapshots.** Off by default. Turn them on under
  `sundae.chartValues.config.snapshots.s3` (example in `environment.yaml`;
  semantics in the top-level README, "S3 snapshots"). The access key is a
  Secret in the namespace named by
  `sundae.chartValues.snapshots.s3Credentials.secretName`: with `key` set
  (default `credentials`) that key is an AWS shared-credentials file mounted
  at `/etc/sundae-s3/<key>`; with `key: ""` the whole Secret is mounted
  at `/etc/sundae-s3` and must hold `AWS_ACCESS_KEY_ID` and
  `AWS_SECRET_ACCESS_KEY`, as the Secret of a Rook ObjectBucketClaim does.
  The chart points `snapshots.s3.credentials_file` at it, refuses inline keys
  and a bare `on:` key, and requires a bucket. Sundae needs egress to the
  endpoint (HEAD, small GET and PUT, LIST, DELETE), and so do the sandboxes:
  they upload and download the archives themselves with `curl`, so add the
  endpoint to the sandbox network policy (example in the top-level README,
  "Sandbox egress"). With `config.sessions.suspend.fallback: snapshot`, idle
  sessions are parked to S3 instead of left running, since this deployment's
  driver cannot suspend.
- **The data volume outlives the release.** `hf destroy` keeps the
  `data-sundae-0` claim and the Agent Sandbox CRDs. Its size cannot be
  changed through helmfile after the first install.
- **The pod runs as 10001:10001** with a read-only root filesystem. `fsGroup`
  makes the volume writable.

## What has not been verified

Checked without a cluster: the Go build the Containerfile runs (linux/amd64
and linux/arm64, Go 1.26.11), `helm lint`, `helm template`, `helmfile
template` and `lint` for all three releases, and that the rendered config is
accepted by Sundae's own config parsing. A native build of Sundae also
started against the rendered config from a read-only file and answered
`/livez`, `/readyz` and `/metrics`.

Not checked, because it needs an image build or a live cluster:

- The images were never built: no container engine was running. For the
  Sundae image, the `apk` package names and base image tags were checked
  against the registries only. For the sandbox image, the base image tag and
  digest (amd64 and arm64) were checked against nvcr.io, the `apt` package
  names against the Ubuntu 24.04 (noble) `main` indexes for amd64 and arm64,
  and the jj 0.47.1 release tarballs were downloaded and their SHA-256
  checksums compared with the ones in the Containerfile. The osewa 0.1.4
  Linux builds both Containerfiles download were fetched the same way and
  checked against the release's `SHA256SUMS`.
- That `osewa server`, started by an exec, keeps running in a sandbox after
  the exec ends, and that the bridge's `osewa attach` over loopback works
  there. Both follow from the OpenShell 0.1.8 and osewa 0.1.4 sources, and
  the bootstrap sequence was run against a native osewa build outside a
  sandbox; neither has run under OpenShell.
- Whether a sandbox started from the sandbox image comes up under OpenShell:
  the workspace seeding, the UID 10001 identity, and the binary paths in the
  sandbox network policy.
- Nothing was installed: no `helmfile sync`, so the release order, the hooks
  and the waits are untested.
- Whether the gateway accepts Sundae's requests with only the client
  certificate from `openshell-client-tls`, and whether the Go SDK in `go.mod`
  (0.1.6) works against a 0.1.8 gateway.
- Whether sandboxes can reach Sundae's HTTP Service, and how the sandbox
  egress policy is supplied.
- Session status polling and parking against a gateway: that `osewa local
  ls` by exec answers within the poll timeout in a sandbox, what the gateway
  answers to a suspend (expected: FailedPrecondition, "compute driver does
  not support sandbox suspend", from the 0.1.8 source), and, with a driver
  that can suspend, a resume that keeps the osewa session or a clean restart
  that loses it. The poll was run against a native osewa 0.1.4 build.
- S3 snapshots against the cluster's Ceph RGW or AWS: the sandbox's egress
  to the endpoint, RGW's handling of the presigned PUT with `Content-MD5` and
  of `sse` (RGW refuses SSE headers without a configured KMS, hence the
  default `sse: ""`), and how long large archives take. The pipeline ran
  against a local MinIO (with SSE-S3) and a signature-checking fake; whether
  OpenShell's Kubernetes driver keeps a sandbox's `/sandbox` volume after the
  sandbox is deleted (which is when a restore asks before replacing files)
  is not known.
- Notifications against a real ntfy server from the cluster (egress, and an
  access-controlled topic with a token). The publish format was checked
  against an `httptest` server.
- Whether the cluster's StorageClass honours `fsGroup`. If the pod cannot
  write `/sundae`, that is the first thing to look at.
- Running with a read-only root filesystem. If something needs to write
  outside `/sundae` and `/tmp`, set
  `sundae.chartValues.securityContext.readOnlyRootFilesystem: false`.
- Client certificate rotation. Sundae is expected to need a restart to
  pick up a new certificate.
