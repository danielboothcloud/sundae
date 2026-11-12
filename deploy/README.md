# Deploying Soft Serve with OpenShell on Kubernetes

One helmfile installs three releases, in this order:

| Release | Chart | Namespace |
| --- | --- | --- |
| `agent-sandbox` | `kubernetes-sigs/agent-sandbox` Helm chart at tag `v1.0.10` | `agent-sandbox-system` |
| `openshell` | `oci://ghcr.io/nvidia/openshell/helm-chart` `0.1.8` | `namespace` (default `openshell`) |
| `soft-serve` | [`helm/soft-serve`](helm/soft-serve) in this repository | the same namespace |

Soft Serve and the OpenShell gateway share a namespace so Soft Serve can mount
the gateway's client certificate (`openshell-client-tls`). Soft Serve's whole
configuration is rendered into a ConfigMap and mounted read-only at
`/etc/soft-serve/config.yaml`.

Nothing here has been deployed to a cluster yet. See
[What has not been verified](#what-has-not-been-verified).

## Prerequisites

- A cluster whose CNI **enforces NetworkPolicy** (Cilium does). OpenShell
  relies on it to confine sandboxes.
- A default StorageClass, or the StorageClass values set (see below).
- `helm` 4, `helmfile` 1.8 (`mise x helmfile@1.8.2 -- helmfile ...`), `git`
  (helmfile fetches the Agent Sandbox chart from GitHub), and the `helm-diff`
  plugin if you want `helmfile diff` or `apply`.
- `podman` or `docker` to build the image, and push access to the registry.
- A sandbox image that provides `git` and `jj`.

## 1. Build and push the image

Run from the repository root. Build for the architecture of the cluster nodes.

```sh
IMAGE=ghcr.io/danielboothcloud/soft-serve   # placeholder: confirm the registry path
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

`docker build` takes the same arguments. The repository's `Dockerfile` is the
goreleaser one and is not used here.

## 2. Set the values

Edit [`environment.yaml`](environment.yaml), or pass the same keys with
`--state-values-set`.

| Value | Required | What it is |
| --- | --- | --- |
| `openshell.sandboxImage` | yes | Default image for the sandboxes Soft Serve starts. |
| `softServe.image.tag` | yes | The tag pushed in step 1. |
| `softServe.image.repository` | check | Defaults to the placeholder `ghcr.io/danielboothcloud/soft-serve`. |
| `softServe.initialAdminKeys` | in practice | Admin public keys. Without one nobody can administer the server. |
| `softServe.ssh.serviceType` | in practice | `LoadBalancer` or `NodePort` to reach SSH from outside the cluster. |
| `softServe.ssh.publicURL` | in practice | The external SSH address, shown in clone URLs. |
| `namespace` | no | Defaults to `openshell`. |
| `agentSandbox.enabled` | no | Set `false` when the cluster already has Agent Sandbox. |
| `softServe.storage.*`, `*.chartValues` | no | Volume size and StorageClass; free-form chart values. |

helmfile stops with a message naming the value when a required one is empty.
If the image is private, add a pull secret through
`softServe.chartValues.imagePullSecrets`.

## 3. Install

Run from this directory. `hf` stands for `mise x helmfile@1.8.2 -- helmfile`.

```sh
# Render everything without touching the cluster. The --api-versions flag
# stands in for the Agent Sandbox API, which the OpenShell chart checks for.
hf template --args="--api-versions=agents.x-k8s.io/v1beta1"

# 1. Agent Sandbox CRDs and controller. Skip when agentSandbox.enabled is false.
hf -l name=agent-sandbox sync

# 2. OpenShell, then Soft Serve.
hf sync
```

Install Agent Sandbox first: the OpenShell chart refuses to render until the
cluster serves the Sandbox API, and `hf diff` and `hf apply` render every
release before installing any. Once everything is installed, use `hf diff` and
`hf apply` for changes.

Afterwards:

- The gateway address Soft Serve uses is
  `https://openshell.<namespace>.svc.cluster.local:8080`.
- Sandboxes clone from
  `http://soft-serve-http.<namespace>.svc.cluster.local:23232`. The sandbox
  network policy must allow that host and port (see "Sandbox egress" in the
  top-level README; the host name there is an example and differs from this
  one).
- SSH is the Service `soft-serve-ssh`, port 23231.

## Things to know

- **One pod.** Soft Serve is a single-replica StatefulSet on one
  ReadWriteOnce volume (SQLite and the repositories). The chart has no replica
  setting.
- **Config changes roll the pod.** The ConfigMap's checksum is a pod
  annotation. SSH sessions are dropped when the pod restarts.
- **No secrets in the ConfigMap.** The chart refuses `config.openshell.token`.
  Secret files come from existing Secrets (`openshell.clientTLS`,
  `secretMounts`); a secret value can be passed as a `SOFT_SERVE_*` variable
  through `extraEnv`.
- **The data volume outlives the release.** `hf destroy` keeps the
  `data-soft-serve-0` claim and the Agent Sandbox CRDs. Its size cannot be
  changed through helmfile after the first install.
- **The pod runs as 10001:10001** with a read-only root filesystem. `fsGroup`
  makes the volume writable.

## What has not been verified

Checked without a cluster: the Go build the Containerfile runs (linux/amd64
and linux/arm64, Go 1.26.11), `helm lint`, `helm template`, `helmfile
template` and `lint` for all three releases, and that the rendered config is
accepted by Soft Serve's own config parsing. A native build of Soft Serve also
started against the rendered config from a read-only file and answered
`/livez`, `/readyz` and `/metrics`.

Not checked, because it needs an image build or a live cluster:

- The image itself was never built: no container engine was running. The
  `apk` package names and base image tags were checked against the registries
  only.
- Nothing was installed: no `helmfile sync`, so the release order, the hooks
  and the waits are untested.
- Whether the gateway accepts Soft Serve's requests with only the client
  certificate from `openshell-client-tls`, and whether the Go SDK in `go.mod`
  (0.1.6) works against a 0.1.8 gateway.
- Whether sandboxes can reach Soft Serve's HTTP Service, and how the sandbox
  egress policy is supplied.
- Whether the cluster's StorageClass honours `fsGroup`. If the pod cannot
  write `/soft-serve`, that is the first thing to look at.
- Running with a read-only root filesystem. If something needs to write
  outside `/soft-serve` and `/tmp`, set
  `softServe.chartValues.securityContext.readOnlyRootFilesystem: false`.
- Client certificate rotation. Soft Serve is expected to need a restart to
  pick up a new certificate.
