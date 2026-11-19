# Volume Tier Autoscaler — Project Plan

A vendor-agnostic Kubernetes controller that automatically moves PVCs between
performance tiers (VolumeAttributesClasses) based on observed I/O behavior.
Think "VPA for disks."

## Status (2026-11-17)

| Area | State |
|---|---|
| CRDs (`VolumeTierPolicy`, `VolumeTierState`, `VolumeIOSample`) | Done |
| Policy reconciler (§7.1) | Done: tier validation (`Ready`), `VolumeTierState` per matched PVC, `status.policyRef`, `Conflict` condition, 5-minute resync. Not yet: deleting states of PVCs that stop matching; PVC/neighbour-policy watches (the resync covers them for now). |
| Node agent (§5.1) | Done for filesystem-mode block volumes: diskstats + pod PSI, `VolumeIOSample` per managed PV. Not yet: `/metrics`, block-mode volumes, NFS. |
| State reconciler (§7.2) | Not started (scaffold only). Next up. |
| Act mode, cooldowns, infeasible handling | Not started |
| Tests | envtest for the policy reconciler; unit tests for procfs parsing, windowing, volume discovery and cgroup lookup |
| Cloud validation | AKS smoke test in `hack/aks-smoke` (Premium SSD v2, fio) |

Next, in order: state reconciler in Recommend mode → Act mode → delete
states of unmatched PVCs → replace the resync with watches → agent `/metrics`,
block-mode and an explicit NFS skip → kind e2e.

---

## 1. Problem

Teams provision storage for peak load and pay for it 24/7. Since Kubernetes
v1.34, `VolumeAttributesClass` (VAC) is GA, so a live PVC's performance tier
can be changed online with no pod restart. Nothing in the ecosystem uses that
API to change tiers automatically, and Kubernetes has no per-volume I/O signal
to base such decisions on.

## 2. Goals and non-goals

**Goals**
- Scale PVCs up or down between admin-defined VAC tiers using observed I/O.
- Work with any CSI driver that implements `ControllerModifyVolume`, with no vendor-specific code.
- Treat driver feedback (`status.modifyVolumeStatus`) as the source of truth.
- Offer a recommend-only mode for cautious adoption.

**Non-goals (v1)**
- Capacity resizing. The controller changes tiers only, never size.
- Creating or managing VACs. Admins define tiers; the controller only selects among them.
- Interpreting VAC parameters. They stay opaque.
- Multi-cluster coordination.

## 3. Target users

- Platform teams running many stateful workloads (for example, one database per tenant).
- Bursty workloads: month-end reporting, batch ETL, ML data loading.
- Dev and staging clusters over-provisioned on premium tiers (scale-down only).
- On-prem storage teams reclaiming array QoS headroom from idle volumes.

## 4. Background: how VAC modification works

The controller is **not** a provisioner or CSI sidecar. It patches one PVC
field. The existing sidecars do the rest.

1. Admin creates a StorageClass plus several VACs with the same `driverName`
   (for example `standard`, `fast`, `ultra`). VAC parameters are immutable.
2. User creates a PVC with `volumeAttributesClassName: standard`.
3. **external-provisioner** calls `CreateVolume` with StorageClass params in
   `parameters` and VAC params in `mutable_parameters`.
4. PV is created with `spec.volumeAttributesClassName: standard`; the PVC's
   `status.currentVolumeAttributesClassName` becomes `standard`.
5. Attach and mount proceed as normal (`ControllerPublishVolume`,
   `NodeStageVolume`, `NodePublishVolume`).
6. **Our controller** patches `pvc.spec.volumeAttributesClassName: fast`.
7. **external-resizer** sees spec differs from current, sets
   `status.modifyVolumeStatus = {target: fast, status: InProgress}`, and calls
   `ControllerModifyVolume` with `fast`'s parameters.
8. Outcome:
   - **Success:** PV spec and PVC `currentVolumeAttributesClassName` update; `modifyVolumeStatus` clears.
   - **Transient error:** stays `InProgress`, retried with backoff.
   - **InvalidArgument:** marked `Infeasible`. Reverting spec to the previous VAC cancels it.

No node-side calls happen during modification and pods are not restarted.

**Controller contract:** write `spec.volumeAttributesClassName`; read
`status.currentVolumeAttributesClassName` and `status.modifyVolumeStatus`.

## 5. Architecture

```
┌────────────────────────── Node (DaemonSet) ──────────────────────────┐
│ vta-agent                                                            │
│  • discover CSI mounts under /var/lib/kubelet                        │
│  • map mount → device (maj:min) → PV                                 │
│  • sample diskstats / cgroup io.stat / io.pressure / NFS mountstats  │
│  • publish VolumeIOSample (CR) + Prometheus /metrics                 │
└──────────────────────────────────┬───────────────────────────────────┘
                                   │
┌──────────────────────── Control plane (Deployment) ──────────────────┐
│ vta-controller (controller-runtime, leader-elected)                  │
│  • watches VolumeTierPolicy, VolumeTierState, PVCs, VACs, samples    │
│  • evaluates policy → recommendation                                 │
│  • (act mode) patches pvc.spec.volumeAttributesClassName             │
│  • tracks modifyVolumeStatus, cooldowns, infeasible tiers in         │
│    one VolumeTierState per PVC                                       │
└──────────────────────────────────────────────────────────────────────┘
```

### 5.1 Node agent: metrics collection

**Volume discovery**
- Filesystem volumes: `/var/lib/kubelet/pods/<pod-uid>/volumes/kubernetes.io~csi/<pv-name>/mount`
- Raw block volumes (`volumeMode: Block`): `/var/lib/kubelet/plugins/kubernetes.io/csi/volumeDevices/...`
- Resolve mount to device `maj:min` via the host's `/proc/1/mountinfo` (agent mounts the host's `/proc` read-only; no kubelet dir mount needed).
  Mounts with major 0 (NFS, Azure Files, tmpfs, projected) are skipped.

**Block-backed volumes**
- Per-device: `/proc/diskstats` (or `/sys/block/<dev>/stat`) gives IOPS, throughput,
  average latency (await), queue depth and utilization. On cloud block storage a
  device maps 1:1 to a PV, so these are per-volume figures.
- Per-pod attribution: cgroup v2 `io.stat`, keyed by `maj:min`.
- Pressure: cgroup v2 `io.pressure` (`some`/`full` avg10/avg60).
  **Caveat:** PSI is per-cgroup, not per-device. If a pod has several volumes,
  use device latency and queue depth to attribute saturation, and PSI only as
  a corroborating signal.

**NFS-backed volumes** (for example Trident `ontap-nas`)
- No local block device. Use `/proc/self/mountstats` per-mount op counts,
  RTT and execute time.

**Publishing**
- `VolumeIOSample` CR per PV, updated every 60s (configurable) with rolling
  aggregates: p50/p95 latency, IOPS, MB/s, util, PSI avg60.
- Only PVs whose PVC has a `VolumeTierState` with a non-empty `status.policyRef`
  get a sample; the agent deletes the sample once the PVC stops being managed.
  Samples are owned by the PVC so they are garbage collected with it.
- Prometheus `/metrics` endpoint with the same series (not yet implemented).

**As built** (`cmd/agent`, `internal/agent`, `config/agent`)
- Reads `/proc/diskstats` every 10s, publishes every 60s over a 60s window
  (`--sample-interval`, `--publish-interval`, `--window`).
- Latency p50/p95 are percentiles of per-interval average latency (diskstats
  has no per-I/O latency), over intervals that completed at least one I/O.
- PSI is the max `io.pressure` `some avg60` over the pods mounting the volume;
  pod cgroups are found for both the systemd and cgroupfs drivers.
- cgroup `io.stat` per-pod attribution is not used yet.
- Runs as root with all capabilities dropped and a read-only root filesystem;
  all API reads are uncached GETs, so the agent starts no informers.
- Scale note: 1 write/min/volume ≈ 17 writes/s at 1,000 volumes. For larger
  fleets, offer a `metricsSource: prometheus` mode where the controller queries
  Prometheus instead of reading CRs.

### 5.2 Saturation detection

Tier ceilings are opaque, so support two modes:

| Mode | Signal | Config needed |
|---|---|---|
| **Declared ceilings** | Usage above X% of a tier's declared IOPS/throughput for duration D | User states ceilings per tier in the policy |
| **Latency/pressure** (default) | Latency or PSI rising while throughput plateaus → volume is at its ceiling | None beyond thresholds |

Scale-down: sustained low utilization (vs. declared ceiling, or vs. the next
lower tier's ceiling if declared) with no pressure for a long window.

Use hysteresis: scale-up and scale-down thresholds must not overlap.

## 6. API design

Two CRDs, both namespaced:

| CRD | Written by | Purpose |
|---|---|---|
| `VolumeTierPolicy` | Users | Which PVCs, which tiers, when to move |
| `VolumeTierState` | Controller only | One per managed PVC: current decision and durable history |

Per-PVC state lives in its own object rather than in the policy status or PVC
annotations, because:
- A policy status listing every PVC grows without bound (etcd's ~1.5 MB object
  limit) and every per-volume update rewrites the whole policy.
- Annotation writes bump the PVC's resourceVersion, waking every PVC watcher
  (StatefulSet controller, backup tools) and showing as GitOps drift.
- A typed object gives per-volume conditions, events and a useful
  `kubectl get volumetierstates`. Precedent: VPA's `VerticalPodAutoscalerCheckpoint`.

### 6.1 VolumeTierPolicy

```yaml
apiVersion: vta.example.io/v1
kind: VolumeTierPolicy
metadata:
  name: postgres-tiers
  namespace: tenants
spec:
  selector:                  # PVC labels; {} matches every PVC in the namespace
    matchLabels: {app: postgres}
  mode: Recommend            # Recommend | Act
  tiers:                     # ordered lowest → highest; VAC names, unique, ≥2
    - name: standard
    - name: fast
      ceilings: {iops: 16000, throughputMiBps: 1000}   # optional
    - name: ultra
  scaleUp:                   # ANY trigger held for `for`
    ioPressureAbove: 20      # percent, PSI some avg60
    latencyP95AboveMs: 20
    utilizationAbove: 80     # percent of current tier's ceilings (if declared)
    for: 10m
  scaleDown:                 # ALL conditions held for `for`; omit to never scale down
    utilizationBelow: 30     # percent of next-lower tier's ceilings, else device busy%
    ioPressureBelow: 1
    for: 24h
  cooldown: 6h               # min time between modifications per PVC (planned field)
status:
  matchedVolumes: 12
  conditions:
    - type: Ready            # all tier VACs exist and share one driver
    - type: Conflict         # some matched PVCs also match another policy
```

Validation (CEL): at least one scale-up trigger; `scaleDown.utilizationBelow <
scaleUp.utilizationAbove` (hysteresis); unique tier names.

### 6.2 VolumeTierState

Named after its PVC, in the same namespace (1:1 lookup).

```yaml
apiVersion: vta.example.io/v1
kind: VolumeTierState
metadata:
  name: data-pg-0            # == PVC name
  namespace: tenants
  ownerReferences:           # GC'd with the PVC
    - {apiVersion: v1, kind: PersistentVolumeClaim, name: data-pg-0, uid: ...}
status:
  policyRef: postgres-tiers  # set by the policy controller; empty = unmanaged
  currentTier: standard
  recommendedTier: fast
  reason: "PSI avg60 34% for 14m, throughput flat"
  pendingDirection: Up       # trigger currently holding, and since when
  pendingSince: "2026-11-14T09:46:00Z"
  lastModified: "2026-11-14T10:00:00Z"   # cooldown anchor
  lastAppliedTier: fast                  # what the controller last wrote to the PVC
  infeasibleTiers:
    - name: ultra
      since: "2026-11-13T08:00:00Z"
      retryAfter: "2026-11-13T12:00:00Z"   # exponential: 1h, 2h, 4h … cap 7d
      attempts: 3
      vacUID: 5f1c…                        # VAC recreated → entry dropped
      capacity: 10Gi                       # PVC grew → entry dropped
      message: "IOPS exceeds 500 per GiB"  # from the PVC's events
  conditions:
    - type: Managed          # False when unmatched, in conflict, or opted out
    - type: Modifying        # a modification is in flight
    - type: ManualOverride   # someone else changed the PVC's VAC
```

`policyRef` lives in status alongside everything else; the spec is empty.
The policy reconciler is its only writer, and it is recomputed from the
policies' selectors on every reconcile, so losing it loses nothing. `pendingDirection`/`pendingSince` are needed to evaluate `for` windows
across reconciles (not yet in the Go types).

### 6.3 What stays on the PVC

Only user intent: `vta.example.io/opt-out: "true"` (annotation or label). The
controller reads it and never writes PVC metadata. The only PVC field the
controller writes is `spec.volumeAttributesClassName`.

## 7. Reconcile logic

Two reconcilers, split by responsibility.

### 7.1 Policy reconciler: membership

Watches: `VolumeTierPolicy`; PVCs (mapped to policies in the namespace whose
selector matches, before and after the change); VACs (mapped to policies that
list them).

```
for each policy in namespace:
  validate tiers: every VAC exists and has the same driverName → Ready condition
  matched = PVCs in namespace matching selector
for each matched PVC:
  owners = policies (in namespace) whose selector matches it
  ensure VolumeTierState(name = PVC name) exists, ownerRef → PVC
  status.policyRef = owners[0] if len(owners) == 1, else ""   (conflict)
for each VolumeTierState whose PVC no longer matches its policy:
  delete it    (re-matching starts from a fresh state)
policy.status = {matchedVolumes, conditions: Ready, Conflict}
requeue after 5m
```

As built: policies with invalid selectors are skipped when computing owners.
The conflict message lists each PVC with the policies claiming it, e.g.
`data (policy-a, policy-b)`. Deleting states of PVCs that no longer match is not
yet implemented, and PVC and neighbouring-policy watches are replaced for now by
the 5-minute requeue.

The policy reconciler never touches PVC specs.

### 7.2 State reconciler: decisions and actions

Watches: `VolumeTierState`; its PVC (same name); `VolumeIOSample` (PV →
claimRef → state); the referenced policy (via a field index on
`status.policyRef`). Also requeues at the next interesting time: end of a `for`
window, cooldown expiry, or the earliest `retryAfter`.

```
load state, PVC, PV, policy (status.policyRef)
if policyRef empty, policy missing, PVC opted out, or PVC unbound:
    Managed=False; return
skip if current VAC not in policy tiers, or VAC driver != PV's CSI driver
status.currentTier = pvc.status.currentVolumeAttributesClassName

# manual override
if lastAppliedTier != "" and pvc.spec.vac != lastAppliedTier:
    ManualOverride=True; lastAppliedTier = pvc.spec.vac; lastModified = now
    return (paused for one cooldown)

# expire infeasible entries
drop entries where now > retryAfter, VAC UID changed, or PVC capacity > recorded

# in-flight modification
switch pvc.status.modifyVolumeStatus.status:
  InProgress → Modifying=True; requeue; return
  Pending    → Modifying=True, reason VAC missing?; return
  Infeasible → upsert infeasibleTiers[target] (attempts++, retryAfter = backoff,
               vacUID, capacity, message from latest PVC event);
               if mode == Act and lastAppliedTier == target:
                   patch pvc.spec.vac = currentTier; lastAppliedTier = currentTier
               return
  unknown    → log; return (API may add statuses)
Modifying=False

# decide
sample = latest VolumeIOSample for the PV; return if stale
dir = Up if any scaleUp trigger holds, Down if all scaleDown conditions hold, else None
if dir != pendingDirection: pendingDirection = dir; pendingSince = now
if dir == None or now - pendingSince < rule.for: recommendedTier = currentTier; return
target = next tier in dir, skipping infeasible tiers; none → return
recommendedTier = target; reason = <which trigger, values, duration>

# act
if mode == Act and now - lastModified >= cooldown and rate limits allow
   and no capacity resize in progress:
    patch pvc.spec.volumeAttributesClassName = target
    lastAppliedTier = target; lastModified = now; pendingDirection = None
write state.status
```

**Safety rules**
- One tier step per change.
- Per-PVC cooldown (default 6h; providers differ, so keep it configurable).
- Global and per-driver concurrency limits on in-flight modifications
  (count states with `Modifying=True`).
- Manual changes to a PVC's VAC pause automation for one cooldown.
- A PVC matched by several policies is left unmanaged and flagged on each policy.
- Do not patch while a capacity resize is in progress. Some drivers coalesce
  expand and modify (EBS does), but don't rely on it.
- A `VolumeTierState` is deleted when its PVC stops matching (or its policy
  is deleted), so its history goes with it. A new state takes no action until
  a full `for` window has been observed. Swapping policies on a PVC can
  therefore land inside a provider's modification cooldown; that is the
  user's responsibility, and the driver's rejection is handled as a failed
  modification (TIERD-005).

## 8. Driver support (verify before release)

| Driver | VAC support | Notes |
|---|---|---|
| AWS EBS (`ebs.csi.aws.com`) | Yes | Type, IOPS, throughput. Provider cooldown on modifications. Coalesces expand + modify. |
| GCE PD (`pd.csi.storage.gke.io`) | Yes | IOPS/throughput, mainly Hyperdisk. |
| Portworx / Pure FlashArray (`pxd.portworx.com`) | Yes | PX-CSI 26.2.1+, K8s 1.34+. Strict `InvalidArgument` on unsupported params. |
| CSI hostpath | Yes (recent releases) | `--accepted-mutable-parameter-names`. Use for CI. |
| Ceph-CSI (RBD) | Design merged, release unconfirmed | QoS enforced node-side via cgroup `io.max`. |
| Azure Disk (`disk.csi.azure.com`) | Documented for Premium SSD v2 / Ultra | `DiskIOPSReadWrite`, `DiskMBpsReadWrite`. Max 500 IOPS/GiB. Validate with `hack/azure-vac-validate.sh`. |
| NetApp Trident | No (as of 26.06) | See §11. |

The controller discovers support at runtime: no VAC for the driver, or an
`Infeasible` result, marks that driver/tier unsupported for the PVC.

## 9. Testing

**Unit / integration**
- Policy evaluation as pure functions with table-driven tests (windows, hysteresis, cooldown).
- envtest for reconcile behavior against a real API server.
- Fake `modifyVolumeStatus` transitions, including unknown statuses.

**End-to-end (kind + hostpath)**
- Two or three VACs on the hostpath driver.
- `fio` load profiles: sustained random write (scale up), idle (scale down),
  bursty (verify hysteresis prevents flapping).
- Infeasible path: a VAC with a parameter not in the accepted list should produce `InvalidArgument`.
- Controller restart mid-modification: state recovered from `VolumeTierState` and PVC status.
- Delete a `VolumeTierState`: recreated by the policy reconciler, no immediate action.
- Two policies matching one PVC: `Conflict` on both, PVC untouched.

**Cloud validation**
- EBS: gp3 baseline vs. raised IOPS/throughput; verify cooldown handling.
- GCE Hyperdisk: provisioned IOPS tiers.
- Portworx/FlashArray if accessible (on-prem story).

## 10. Milestones

| # | Milestone | Deliverable | Status |
|---|---|---|---|
| M0 | Scaffold | Kubebuilder project, CRD types, RBAC, CI | Done (CI not yet) |
| M1 | Recommend-only | Controller writes recommendations from synthetic samples | Membership done; recommendations next |
| M2 | Node agent (block) | Mount → device → PV mapping; diskstats, io.stat, PSI; `VolumeIOSample` + `/metrics` | Mostly done; no `io.stat`, `/metrics` or block-mode yet |
| M3 | Act mode | Patching, cooldowns, Infeasible handling, rate limits, manual-override detection | Not started |
| M4 | Cloud validation | EBS, GCE and AKS e2e runs; docs on tier design per provider | AKS smoke test running |
| M5 | NFS path | `/proc/self/mountstats` collector | Not started |
| M6 | Prometheus source | Optional `metricsSource: prometheus` for large fleets | Not started |
| M7 | Release | Helm chart, Grafana dashboard, v0.1.0 | Not started |

## 11. Side track: Trident `ControllerModifyVolume` contribution

Trident (Go, open source) doesn't support VAC, but ONTAP supports online QoS
policy group reassignment. Contact the maintainers first, then:

1. Advertise the `MODIFY_VOLUME` controller capability.
2. Accept mutable parameters (for example `qosPolicy`, `adaptiveQosPolicy`) at create time.
3. In `ControllerModifyVolume`, reassign the ONTAP volume/LUN to the target policy group via ONTAP REST.
4. Return `InvalidArgument` for unknown policies or backends without QoS support (for example economy drivers).

This would make ONTAP a strong on-prem target and exercise the NFS metrics path.

## 12. Open questions

- Should tier ceilings be auto-learned from observed plateaus instead of declared?
- Should cost be a first-class input (a per-tier cost field to prefer cheaper tiers)?
- Cluster-scoped policy variant for platform teams?
- How to handle StatefulSets where replicas have different load (per-PVC decisions are the default, but group policies may be wanted)?
- Expose recommendations as Kubernetes Events in addition to status?

## 13. References

- KEP-3751: VolumeAttributesClass and ModifyVolume — https://www.kubernetes.dev/resources/keps/3751/
- VAC GA blog (v1.34) — https://kubernetes.io/blog/2025/09/08/kubernetes-v1-34-volume-attributes-class/
- VAC concepts — https://kubernetes.io/docs/concepts/storage/volume-attributes-classes/
- external-resizer — https://github.com/kubernetes-csi/external-resizer
- AWS EBS CSI — https://github.com/kubernetes-sigs/aws-ebs-csi-driver
- Portworx VAC docs — https://docs.portworx.com/portworx-csi/manage-provisioned-storage/modify-volume-attributes
- Trident VAC KB — https://kb.netapp.com/Cloud/Astra/NetApp_Trident/Trident_KBs/Does_Trident_support_VolumeAttributesClass