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
- **Node agent** (DaemonSet): finds CSI volumes mounted on its node, reads the
  block device's counters from `/proc/diskstats` and the pods' cgroup v2
  `io.pressure`, and publishes a `VolumeIOSample` per managed volume (IOPS,
  throughput, p50/p95 latency, utilization, queue depth, PSI).

```sh
kubectl get volumetierpolicies,volumetierstates,volumeiosamples -A
```

**Status:** early development. Membership and metrics collection work;
tier recommendations and Act mode are not implemented yet. See
[docs/PLAN.md](docs/PLAN.md) for the design and roadmap.

Limitations today: filesystem-mode block volumes only (no `volumeMode: Block`,
NFS or Azure Files metrics).

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

**Create instances of your solution**
You can apply the samples (examples) from the config/sample:

```sh
kubectl apply -k config/samples/
```

>**NOTE**: Ensure that the samples has default values to test it out.

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
make test   # unit tests + envtest (real API server and etcd, no controllers)
make lint
```

### Smoke test on AKS

`hack/aks-smoke` creates two Premium SSD v2 tiers (3000 and 5000 IOPS), a
10Gi PVC, an fio pod and a Recommend-mode policy in namespace `tierd-test`:

```sh
kubectl apply -k hack/aks-smoke
kubectl get volumeiosample -n tierd-test -w
```

Premium SSD v2 needs a zonal node pool; edit the fio pod's `nodeSelector`
for your cluster. The VM size must allow more uncached disk IOPS than the
highest tier, or the VM, not the disk, becomes the limit. fio runs at
`iodepth=32`, which trips node-exporter's `NodeDiskIOSaturation` alert after
30 minutes; lower it if that alert pages someone.

## Project Distribution

Following the options to release and provide this solution to the users.

### By providing a bundle with all YAML files

1. Build the installer for the image built and published in the registry:

```sh
make build-installer IMG=<some-registry>/tierd:tag
```

**NOTE:** The makefile target mentioned above generates an 'install.yaml'
file in the dist directory. This file contains all the resources built
with Kustomize, which are necessary to install this project without its
dependencies.

2. Using the installer

Users can just run 'kubectl apply -f <URL for YAML BUNDLE>' to install
the project, i.e.:

```sh
kubectl apply -f https://raw.githubusercontent.com/<org>/tierd/<tag or branch>/dist/install.yaml
```

### By providing a Helm Chart

1. Build the chart using the optional helm plugin

```sh
kubebuilder edit --plugins=helm/v2-alpha
```

2. See that a chart was generated under 'dist/chart', and users
can obtain this solution from there.

**NOTE:** If you change the project, you need to update the Helm Chart
using the same command above to sync the latest changes. Furthermore,
if you create webhooks, you need to use the above command with
the '--force' flag and manually ensure that any custom configuration
previously added to 'dist/chart/values.yaml' or 'dist/chart/manager/manager.yaml'
is manually re-applied afterwards.

## Contributing
// TODO(user): Add detailed information on how you would like others to contribute to this project

**NOTE:** Run `make help` for more information on all potential `make` targets

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

## License

Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

