# Contributing

Before opening a pull request run, from the repo root:

```sh
cd engine && pip install -e '.[dev]' && pytest
cd operator && make check        # gofmt, vet, unit tests, race tests, integration
cd operator && make manifests    # regenerate CRDs/RBAC/deepcopy; the diff must be empty (CI enforces it)
```

- `make envtest` runs against a real kube-apiserver and etcd (`KUBEBUILDER_ASSETS`); CI runs it too.
- The kind end-to-end scripts (`hack/e2e-kind.sh`, `hack/e2e-kca.sh`) need docker, kind and kubectl; CI runs them.
- Go tests use `-race -p 2` (more parallelism has run out of memory on small machines).
- Kubernetes (`k8s.io/*`) and controller-runtime minor upgrades are deliberate changes, not Dependabot's: they need
  regenerated code and a Go/kube-apiserver compatibility check.
- Claims in docs and PR descriptions must match what was actually run.
