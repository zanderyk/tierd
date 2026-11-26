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

### CSI drivers

Each CSI driver implements `ModifyVolume` differently, which affects how
quickly a tier change takes effect and how often tierd can change tiers.

| Driver | Tested | Behavior |
|---|---|---|
| `disk.csi.azure.com` | Yes (AKS) | Applies live. At least 6 hours between tier changes on a volume; tierd rolls back a rejected change and retries with backoff. |
| `file.csi.azure.com` | Yes (AKS) | Applies live. At least 6 hours between tier changes on a share. |
| `rbd.csi.ceph.com` (`krbd`, default) | Yes (Rook) | The change is accepted and the PVC shows the new class, but the limits reach the application pod only when it restarts. |
| `rbd.csi.ceph.com` (`rbd-nbd`) | Yes (Rook) | Applies live, but much slower than krbd. |
| `hostpath.csi.k8s.io` | Yes (kind, e2e) | Records the change but enforces no limits. For testing only. |
| `nfs.csi.k8s.io`, `smb.csi.k8s.io` | Yes (kind, e2e) | Measured, but can't modify volumes. Use `Recommend` mode. |
| `ebs.csi.aws.com` | Not yet | Supports type, IOPS and throughput changes. AWS requires at least 6 hours between modifications of a volume. |
| `csi.trident.netapp.io` (NetApp Trident) | No | Can't modify volumes: `ControllerModifyVolume` is unimplemented (as of 26.10.1). Use `Recommend` mode. |
| `pd.csi.storage.gke.io` (Hyperdisk) | Yes (GKE) | Applies live, but takes about 5 minutes after the PVC shows the new class. Rate-limited: a second change soon after is rejected as `Infeasible`. |
| `pxd.portworx.com` | Not yet | Supports modifying volume attributes. |

Reports for other drivers are welcome; open an issue with what you saw.

On krbd, a running pod keeps its old limits until it restarts, so use
`Recommend` mode or expect changes to land on the next rollout. For Ceph,
set the `iops` ceiling to read + write, since its limits apply to each
direction.

On GCP Hyperdisk, set `scaleUp.for` and `scaleDown.for` longer than about
5 minutes, so tierd doesn't judge a change before it takes effect.

## Install

Requires Kubernetes v1.34+ with cgroup v2 nodes and a CSI driver that
supports `ControllerModifyVolume` (VolumeAttributesClass changes).

With Helm:

```sh
helm install tierd oci://ghcr.io/zanderyk/charts/tierd --version <version> \
  --namespace tierd-system --create-namespace
```

[helm/chart/values.yaml](helm/chart/values.yaml) lists the settings; the
`agent` section sets the sampling intervals, tolerations and resources of
the node agent.

Then write a `VolumeTierPolicy`:

```yaml
# Moves PVCs labelled app=postgres between two VolumeAttributesClasses.
# Replace the tier names with VolumeAttributesClasses of your CSI driver,
# lowest first, and set each tier's ceilings to what it provisions.
apiVersion: tierd.zanderyk.github.io/v1alpha1
kind: VolumeTierPolicy
metadata:
  name: postgres
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

To uninstall, `helm uninstall tierd --namespace tierd-system`.

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

## Development

Requires Go 1.27+ and Docker. Build and deploy from source (the controller
and node agent share one image):

```sh
make docker-build docker-push IMG=<registry>/tierd:<tag>
make install                         # CRDs
make deploy IMG=<registry>/tierd:<tag>
make undeploy uninstall              # remove
```

Tests:

```sh
make test       # unit tests + envtest (real API server and etcd, no controllers)
make lint
make test-e2e   # kind cluster with CSI hostpath, NFS and SMB drivers; ~7 minutes
```

`make test-e2e` deletes its kind cluster when it passes; after a failure, run
`make cleanup-test-e2e`. `helm/chart` is generated from `config/` with
`make helm-chart`. `make help` lists all targets.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Report security issues privately as
described in [SECURITY.md](SECURITY.md).

## License

Apache 2.0; see [LICENSE](LICENSE).
