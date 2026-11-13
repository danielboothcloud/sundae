# Sundae Helm chart

A cluster-neutral chart for Sundae: one StatefulSet with a data volume,
Services for SSH, HTTP and stats, and Sundae's whole configuration rendered
from `config` into a ConfigMap mounted read-only at `/etc/sundae/config.yaml`.
Secrets are never in the ConfigMap: the chart mounts them from existing
Secrets as files and points the config at them.

Agent sessions are optional (`config.openshell.enabled`) and need an
[NVIDIA OpenShell](https://github.com/NVIDIA/OpenShell) gateway, which this
chart does not install.

## 1. Build and push the images

On GitHub, CI builds both images for linux/amd64 on every push to `main` or
`develop` (`.github/workflows/images.yml`) and pushes them to the repository
owner's GHCR namespace, `ghcr.io/<owner>/sundae` and
`ghcr.io/<owner>/sundae-sandbox`, tagged with the short commit sha and the
branch name. Build by hand for another registry or architecture:

Run from the repository root. Build for the architecture of the cluster nodes.

### Sundae

```sh
IMAGE=<registry>/sundae              # e.g. ghcr.io/<owner>/sundae
TAG="$(git rev-parse --short HEAD)"

podman build -f Containerfile \
  --platform linux/amd64 \
  --build-arg VERSION="$(git describe --tags --always --dirty)" \
  --build-arg COMMIT_SHA="$(git rev-parse HEAD)" \
  --build-arg COMMIT_DATE="$(git log -1 --format=%cI)" \
  -t "$IMAGE:$TAG" .

podman login <registry host>
podman push "$IMAGE:$TAG"
```

`docker build` takes the same arguments. The repository's `Dockerfile` only
packages a prebuilt binary and is not used here. The image also carries the pinned osewa
release's Linux builds (amd64 and arm64, checksum-verified at build time) in
`/usr/local/share/sundae/osewa`, which Sundae uploads to sandboxes
whose image has no osewa (`sessions.osewa.binary_dir`).

### Sandbox

The image the OpenShell sandboxes run, built from
[`deploy/sandbox/Containerfile`](../../deploy/sandbox/Containerfile): Ubuntu 24.04 (the base
OpenShell uses for its own default sandbox image) with `git`, `jj` 0.47.1,
[osewa](https://github.com/rjyo/osewa) 0.1.4 (which keeps each session's
program running after the SSH client disconnects), `bash`,
`ca-certificates`, `curl`, `openssh-client`, `less`, `nano` and `vi`. Its
comments list what OpenShell expects of a sandbox image and what Sundae
keeps in it.

```sh
SANDBOX_IMAGE=<registry>/sundae-sandbox   # e.g. ghcr.io/<owner>/sundae-sandbox
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

Set `config.openshell.image` to `$SANDBOX_IMAGE:$TAG` in step 3.


## 2. Install OpenShell (only for agent sessions)

Skip this when `config.openshell.enabled` stays false.

OpenShell's Kubernetes driver runs every sandbox as a `Sandbox` resource of
[Agent Sandbox](https://github.com/kubernetes-sigs/agent-sandbox)
(`agents.x-k8s.io`), so install that first; the OpenShell chart does not, and
its preflight check refuses to install without it. The cluster CNI must
**enforce NetworkPolicy**; OpenShell relies on it to confine sandboxes.

```sh
# Agent Sandbox CRDs and controller (as the OpenShell chart's README says;
# pin a release, e.g. v1.0.10, instead of latest).
kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.10/sandbox.yaml

# The OpenShell gateway. Install it in the namespace Sundae will use, so
# Sundae can mount its client TLS Secret (openshell-client-tls).
helm install openshell oci://ghcr.io/nvidia/openshell/helm-chart \
  --version 0.1.8 -n <namespace> --create-namespace
```

With chart 0.1.8 defaults, the gateway is
`https://openshell.<namespace>.svc.cluster.local:8080` and its client
certificate is in the Secret `openshell-client-tls` (`ca.crt`, `tls.crt`,
`tls.key`), which is this chart's default `openshell.clientTLS.secretName`.
To use a gateway in another namespace, copy that Secret into Sundae's
namespace.

Sandboxes clone from Sundae's in-cluster HTTP Service. The sandbox network
policy must allow it (see "Sandbox egress" in the top-level README).

## 3. Set your values

Keep your cluster's values in your own file, e.g. `my-cluster.local.yaml`
(`*.local.yaml` is gitignored in this repository).

```yaml
image:
  repository: <registry>/sundae
  tag: <tag>

initialAdminKeys:
  - "ssh-ed25519 AAAA... you@example.com"

service:
  ssh:
    type: LoadBalancer      # or NodePort, or ClusterIP plus your own exposure
    annotations: {}         # e.g. for your load balancer or ingress operator

config:
  ssh:
    public_url: "ssh://<ssh-host>:23231"
  # Only for agent sessions (step 2):
  openshell:
    enabled: true
    gateway_address: "https://openshell.<namespace>.svc.cluster.local:8080"
    image: "<registry>/sundae-sandbox:<tag>"
```

| Value | Required | What it is |
| --- | --- | --- |
| `image.repository`, `image.tag` | from a checkout | The Sundae image pushed in step 1. The published chart sets both. |
| `initialAdminKeys` | in practice | Admin public keys. Without one nobody can administer the server. |
| `service.ssh.type`, `service.ssh.annotations` | in practice | How SSH is reached from outside the cluster. |
| `config.ssh.public_url` | in practice | The external SSH address, shown in clone URLs. |
| `config.openshell.gateway_address`, `config.openshell.image` | with sessions | The gateway and the sandbox image from steps 1 and 2. |
| `config.openshell.clone_url` | no | Defaults to this release's HTTP Service, `http://<fullname>-http.<namespace>.svc.cluster.local:23232`. |
| `openshell.clientTLS.secretName` | no | Defaults to `openshell-client-tls`; the TLS paths in `config.openshell` point at it. |
| `persistence.size`, `persistence.storageClass` | no | Data volume; empty StorageClass uses the cluster default. |
| `imagePullSecrets` | no | For a private image. |

Every option is documented in [`values.yaml`](values.yaml). The chart fails
to render with a message naming the value when a required one is missing or
a secret is put in the config.

## 4. Install

From the published chart (built by CI with the images; `image.repository`
and `image.tag` already point at that run's images):

```sh
helm upgrade --install sundae oci://ghcr.io/<owner>/charts/sundae \
  --version <version> -n <namespace> --create-namespace -f my-cluster.local.yaml
```

Versions: `<x.y.z>` for `v*` tags, `<x.y.z>-<branch>.<sha>` for pushes to
`main` and `develop`. Or from a checkout, with `image.*` set in your values:

```sh
helm upgrade --install sundae helm/sundae -n <namespace> --create-namespace \
  -f my-cluster.local.yaml
```

Afterwards SSH is the Service `<fullname>-ssh`, port 23231. Config changes
roll the pod.

## Check the config before deploying

`sundae config check` loads a config file the way `sundae serve` does (the file,
then any `SUNDAE_*` variables as overrides, then validation), prints
`config OK: <path>` and exits 0, or lists every problem and exits 1. Unknown
keys are errors, reported with their line, so a misspelt key cannot be
silently ignored. It never writes a file. This renders the chart's ConfigMap with
your values and checks it. Run it from the repository root; it needs no
cluster.

```sh
helm template sundae helm/sundae -n <namespace> -f my-cluster.local.yaml \
    --show-only templates/configmap.yaml \
  | yq '.data["config.yaml"]' > /tmp/sundae-config.yaml
SUNDAE_DATA_PATH=/sundae go run ./cmd/sundae config check --file /tmp/sundae-config.yaml
```

`SUNDAE_DATA_PATH` matches the chart's `dataPath`, which relative paths in
the config resolve against. Export any other `SUNDAE_*` variables the pod
gets through `extraEnv` too, since they override the file.

Files the config names (TLS files, SSH keys) need not exist where the check
runs, with one exception: when OpenShell is enabled and
`openshell.token_file` is set (as it is when
`openshell.gatewayToken.secretName` is), the check
reads that file, as `sundae serve` does at startup. Outside the pod it fails
with `openshell: read token_file: ... no such file or directory`; to check the
rest, point it at a local file with `--set
config.openshell.token_file=/tmp/token`. The same goes
for every `config.forges[].token_file`: the check reads each one. And for
`config.notifications.ntfy.token_file` when ntfy is enabled (set to
`/etc/sundae-ntfy/token` when
`notifications.ntfyToken.secretName` is set), and for
`config.snapshots.s3.credentials_file` when S3 snapshots are enabled (set to
`/etc/sundae-s3/<key>`, or the directory `/etc/sundae-s3` with an
empty key, when `snapshots.s3Credentials.secretName`
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
  `openshell.gatewayToken.secretName` (and `.key`,
  default `token`). The chart mounts that key at
  `/etc/openshell-token/<key>` and sets `openshell.token_file` to it. Sundae
  reads the file again for every `session` command, so a rotated
  Secret needs no restart once the kubelet has updated the mounted file.
- **Forges.** GitHub and GitLab accounts go under
  `config.forges` (example in `values.yaml`; semantics in the top-level README, "Forges").
  Each token is a key in an existing Secret, mounted as a directory through
  `secretMounts` and named by `token_file`; Sundae
  rejects an inline `token`. Sundae needs egress to each forge's HTTPS
  host. Adding or changing a forge rolls the pod like any config change.
- **Notifications (ntfy).** Off by default. Turn them on under
  `config.notifications` (example in `values.yaml`; semantics in the top-level README, "Notifications").
  Quote the `"on":` key: Helm reads a bare `on:` as `true`, and the chart
  refuses to render that. For an access-controlled topic, put the ntfy
  access token in a Secret in the namespace and set
  `notifications.ntfyToken.secretName` (and `.key`,
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
  `suspend_sandbox`), so with that driver the gateway refuses, Sundae
  logs that once and leaves the sandboxes running. Set
  `config.sessions.suspend.enabled: false` to skip the attempt.
- **S3 snapshots.** Off by default. Turn them on under
  `config.snapshots.s3` (example in `values.yaml`;
  semantics in the top-level README, "S3 snapshots"). The access key is a
  Secret in the namespace named by
  `snapshots.s3Credentials.secretName`: with `key` set
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
  sessions are parked to S3 instead of left running, since that driver
  cannot suspend.
- **The data volume outlives the release.** `helm uninstall` keeps the
  `data-<fullname>-0` claim. Its size cannot be changed by upgrading the
  release after the first install (StatefulSet volume claims are immutable).
- **The pod runs as 10001:10001** with a read-only root filesystem. `fsGroup`
  makes the volume writable.

## What has not been verified

Both images build in CI (linux/amd64). Checked without a cluster: the Go build the Containerfile runs (linux/amd64
and linux/arm64, Go 1.26.11), `helm lint`, `helm template`, and that the rendered config is
accepted by Sundae's own config parsing. A native build of Sundae also
started against the rendered config from a read-only file and answered
`/livez`, `/readyz` and `/metrics`.

Not checked, because it needs an image build or a live cluster:

- That `osewa server`, started by an exec, keeps running in a sandbox after
  the exec ends, and that the bridge's `osewa attach` over loopback works
  there. Both follow from the OpenShell 0.1.8 and osewa 0.1.4 sources, and
  the bootstrap sequence was run against a native osewa build outside a
  sandbox; neither has run under OpenShell.
- Whether a sandbox started from the sandbox image comes up under OpenShell:
  the workspace seeding, the UID 10001 identity, and the binary paths in the
  sandbox network policy.
- Nothing was installed on a cluster, so the chart itself and the OpenShell
  install order are untested.
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
  `securityContext.readOnlyRootFilesystem: false`.
- Client certificate rotation. Sundae is expected to need a restart to
  pick up a new certificate.
