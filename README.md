# tierd

[![Tests](https://github.com/zanderyk/tierd/actions/workflows/test.yml/badge.svg?branch=main)](https://github.com/zanderyk/tierd/actions/workflows/test.yml)
[![Lint](https://github.com/zanderyk/tierd/actions/workflows/lint.yml/badge.svg?branch=main)](https://github.com/zanderyk/tierd/actions/workflows/lint.yml)
[![E2E Tests](https://github.com/zanderyk/tierd/actions/workflows/test-e2e.yml/badge.svg?branch=main)](https://github.com/zanderyk/tierd/actions/workflows/test-e2e.yml)
[![Coverage](https://github.com/zanderyk/tierd/raw/badges/coverage.svg)](https://github.com/zanderyk/tierd/actions/workflows/test.yml)

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

Each release publishes a multi-arch image to `ghcr.io/zanderyk/tierd` and a Helm
chart.

With Helm:

```sh
helm install tierd oci://ghcr.io/zanderyk/charts/tierd --version <version> \
  --namespace tierd-system --create-namespace
```

[helm/chart/values.yaml](helm/chart/values.yaml) lists the settings; the
`agent` section sets the sampling intervals, tolerations and resources of
the node agent.


Then write a `VolumeTierPolicy`;

```yaml
# Moves PVCs labelled app=postgres between two VolumeAttributesClasses.
# Replace the tier names with VolumeAttributesClasses of your CSI driver,
# lowest first, and set each tier's ceilings to what it provisions.
apiVersion: tierd.zanderyk.github.io/v1
kind: VolumeTierPolicy
metadata:
  labels:
    app.kubernetes.io/name: tierd
    app.kubernetes.io/managed-by: kustomize
  name: volumetierpolicy-sample
spec:
  # Recommend only records the tier tierd would pick; Act also moves the PVC.
  mode: Recommend
  selector:
    matchLabels:
      app: postgres
  tiers:
  - name: standard
    ceilings:
      iops: 3000
      throughputMiBps: 125
  - name: fast
    ceilings:
      iops: 10000
      throughputMiBps: 500
  scaleUp:
    # Scale up when the volume uses more than 80% of its tier's ceilings
    # for 5 minutes.
    for: 5m
    utilizationAbove: 80
  scaleDown:
    # Scale down when it would use less than 30% of the tier below for an
    # hour.
    for: 1h
    utilizationBelow: 30
```


## KubeVirt virtual machines

VM disks are tiered like any other PVC, in place while the VM runs.

- I/O pressure stays near zero for VM disks, so use `utilizationAbove`,
  `utilizationBelow` and `latencyP95AboveMs` rather than `ioPressure*`.
- Set a DataVolume's starting tier in `spec.pvc.volumeAttributesClassName`;
  CDI rejects it in `spec.storage`. DataVolume labels are copied to the PVC.
- tierd changes the PVC, not the DataVolume, so the DataVolume keeps showing
  the starting tier.

## GitOps

Exclude `volumeAttributesClassName` from drift detection on PVCs you apply
directly, or Argo CD or Flux will revert tierd's changes. For Argo CD:

```yaml
spec:
  ignoreDifferences:
  - kind: PersistentVolumeClaim
    jsonPointers:
    - /spec/volumeAttributesClassName
```

For Flux, annotate the PVC `kustomize.toolkit.fluxcd.io/ssa: IfNotPresent`.

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

**Delete the CRDs from the cluster:**

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


`helm/chart` is generated from `config/` by kubebuilder's helm plugin:

```sh
make helm-chart
```

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

