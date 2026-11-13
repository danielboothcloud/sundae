# Vendored charts

Third-party charts the Sundae chart (`helm/sundae`) depends on but cannot
pull from a chart repository. They are referenced from
`helm/sundae/Chart.yaml` with `file://../vendor/<name>`; `helm dependency
build helm/sundae` packages them into `helm/sundae/charts/`, and from there
into the published Sundae chart.

Do not edit them. To update one, replace the directory with the upstream
files of the new tag, then bump its version in `helm/sundae/Chart.yaml` and
run `helm dependency update helm/sundae` to refresh `Chart.lock`.

## agent-sandbox

- Upstream: <https://github.com/kubernetes-sigs/agent-sandbox>, directory
  `helm/` at tag `v1.0.10` (commit
  `0d94fc0bd96c8024d5e7845532d2d7e2a96604ef`), chart `agent-sandbox`
  version `0.1.4`.
- Copied unmodified: `Chart.yaml`, `values.yaml`, `README.md`, `crds/`,
  `templates/`. Left out: `helm/ci/` (upstream's lint test values).
- `LICENSE` is the upstream repository's root `LICENSE` (Apache License
  2.0), copyright the Kubernetes Authors.
- Why vendored: the project publishes no chart repository or OCI chart (only
  the source tree and a `sandbox.yaml` manifest per release).
- The controller image is `registry.k8s.io/agent-sandbox/agent-sandbox-controller`;
  the chart requires `image.tag`, which the Sundae chart sets to `v1.0.10`
  (`agentSandbox.image.tag`). Keep the chart directory and that tag on the
  same release.
