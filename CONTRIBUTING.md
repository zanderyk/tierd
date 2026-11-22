# Contributing to tierd

Thanks for helping. Issues and pull requests are welcome.

## Before you start

For anything bigger than a small fix, open an issue first so we can agree on
the approach. [docs/PLAN.md](docs/PLAN.md) has the design.

## Development

You need Go (see `go.mod`), Docker and kubectl. `make help` lists every
target.

```sh
make test       # unit tests and envtest
make lint       # golangci-lint
make test-e2e   # kind-based end-to-end suite, ~7 minutes
```

After changing `api/` types or kubebuilder markers, run
`make manifests generate` and commit the result; CI fails if they are out of
date. Don't edit generated files (`config/crd/bases`, `config/rbac/role.yaml`,
`zz_generated.*`) by hand.

To try a change on a cluster:

```sh
make docker-build docker-push deploy IMG=<registry>/tierd:<unique-tag>
```

Use a unique tag per build; nodes don't re-pull a tag they already have.

## Pull requests

- Keep each PR to one change, with tests.
- CI runs lint, unit tests, a generated-files check and the e2e suite; all
  must pass.
- By contributing you agree your work is licensed under the
  [Apache License 2.0](LICENSE).
