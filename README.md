# tierd
A vendor-agnostic Kubernetes controller that automatically moves PVCs between performance tiers via `VolumeAttributesClasses` based on observed I/O behavior.

## Description

tierd has two parts:

- **Controller** (Deployment): reconciles `VolumeTierPolicy` objects. Each
  policy selects PVCs by label and lists the VolumeAttributesClasses (tiers) they
  may move between. For every matched PVC the controller keeps a
  `VolumeTierState` recording which policy manages it. A PVC matched by more
  than one policy is left unmanaged, and both policies report a `Conflict`
  condition.
- **Node agent** (DaemonSet): finds CSI volumes mounted on its node and
  publishes a `VolumeIOSample` per managed volume (IOPS, throughput, and where
  the volume type reports them, p50/p95 latency, utilization, queue depth),
  plus the pods' cgroup v2 I/O pressure.

The controller compares each sample with the policy's triggers. When a
trigger holds for its `for` window it recommends the next tier up or down;
in `Act` mode it also sets the PVC's `volumeAttributesClassName`. A tier the
driver rejects is rolled back and retried with backoff, and a tier changed by
hand pauses tierd for that PVC until the
`tierd.zanderyk.github.io/manual-override` annotation is removed.

```sh
kubectl get volumetierpolicies,volumetierstates,volumeiosamples -A
```

**Status:** early development; APIs may change. See
[docs/PLAN.md](docs/PLAN.md) for the design and roadmap.

What the agent can measure:

| Volume | Source | Reports |
|---|---|---|
| Block devices (filesystem or `volumeMode: Block`) | `/proc/diskstats` | everything |
| NFS | `/proc/1/mountstats` | IOPS, throughput, latency |
| SMB/cifs | `/proc/fs/cifs/Stats` | IOPS, throughput |

A sample's `Measured` condition says which figures are missing.

## Install

Each release publishes a multi-arch image to `ghcr.io/zanderyk/tierd`, a Helm
chart, and an `install.yaml` with the CRDs, controller and agent.

With Helm:

```sh
helm install tierd oci://ghcr.io/zanderyk/charts/tierd --version <version> \
  --namespace tierd-system --create-namespace
```

[helm/chart/values.yaml](helm/chart/values.yaml) lists the settings; the
`agent` section sets the sampling intervals, tolerations and resources of
the node agent.

Or with kubectl:

```sh
kubectl apply -f https://github.com/zanderyk/tierd/releases/latest/download/install.yaml
```

Then write a `VolumeTierPolicy`; [config/samples](config/samples) has an
example.

## Getting Started

### Prerequisites
- go version v1.27+
- docker version 17.03+.
- kubectl version v1.34+.
- A Kubernetes v1.34+ cluster (VolumeAttributesClass GA) with cgroup v2 nodes
  and a CSI driver that supports `ControllerModifyVolume`.

### To Deploy on the cluster
**Build and push your image to the location specified by `IMG`:**

```sh
make docker-build docker-push IMG=<some-registry>/tierd:tag
```

**NOTE:** This image ought to be published in the personal registry you specified.
And it is required to have access to pull the image from the working environment.
Make sure you have the proper permission to the registry if the above commands don’t work.

**Install the CRDs into the cluster:**

```sh
make install
```

**Deploy the controller and node agent with the image specified by `IMG`:**

```sh
make deploy IMG=<some-registry>/tierd:tag
```

Both use the same image (`/manager` and `/agent`). Use a unique tag or a
digest per build: nodes that already have a tag cached will not pull it again.

> **NOTE**: If you encounter RBAC errors, you may need to grant yourself cluster-admin
privileges or be logged in as admin.

**Create a policy**

[config/samples](config/samples) has an example policy; set its tiers to
VolumeAttributesClasses of your driver first:

```sh
kubectl apply -k config/samples/
```

### To Uninstall
**Delete the instances (CRs) from the cluster:**

```sh
kubectl delete -k config/samples/
```

**Delete the APIs(CRDs) from the cluster:**

```sh
make uninstall
```

**UnDeploy the controller from the cluster:**

```sh
make undeploy
```

## Testing

```sh
make test       # unit tests + envtest (real API server and etcd, no controllers)
make lint
make test-e2e   # kind cluster with CSI hostpath, NFS and SMB drivers; ~7 minutes
```

`make test-e2e` creates a kind cluster, runs the suite in `test/e2e` and
deletes the cluster. A failed run leaves the cluster up for debugging; run
`make cleanup-test-e2e` before the next run.

### Smoke test on AKS

`hack/aks-smoke` creates two Premium SSD v2 tiers (3000 and 5000 IOPS), a
10Gi PVC, an fio pod and a Recommend-mode policy in namespace `tierd-test`:

```sh
kubectl apply -k hack/aks-smoke
kubectl get volumeiosample -n tierd-test -w
```

Premium SSD v2 needs a zonal node pool, so every node the fio pod can land on
must be in a zone. The VM size must allow more uncached disk IOPS than the
highest tier, or the VM, not the disk, becomes the limit. fio runs at
`iodepth=32`, which trips node-exporter's `NodeDiskIOSaturation` alert after
30 minutes; lower it if that alert pages someone.

## Releases

Pushing a `v*` tag builds and publishes the image, chart and `install.yaml`
(`.github/workflows/release.yml`).

### Helm chart

`helm/chart` is generated from `config/` by kubebuilder's helm plugin:

```sh
make helm-chart
```

It keeps `values.yaml`, `Chart.yaml` and `templates/agent/` (the node
agent, which the plugin doesn't template), and regenerates the rest. CI fails
if the chart is out of date.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Report security issues privately as
described in [SECURITY.md](SECURITY.md).

**NOTE:** Run `make help` for more information on all potential `make` targets

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

## License

Copyright 2026 The tierd Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

