/*
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
*/

package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	tierdv1 "github.com/zanderyk/tierd/api/v1"
	"github.com/zanderyk/tierd/internal/agent/procfs"
)

// Config is the node agent's configuration.
type Config struct {
	// NodeName is the node this agent runs on.
	NodeName string
	// ProcRoot is the host's /proc, mounted into the agent's container.
	ProcRoot string
	// CgroupRoot is the host's cgroup v2 hierarchy (/sys/fs/cgroup).
	CgroupRoot string
	// SampleInterval is how often diskstats is read.
	SampleInterval time.Duration
	// PublishInterval is how often VolumeIOSamples are written.
	PublishInterval time.Duration
	// Window is how much history each VolumeIOSample summarizes.
	Window time.Duration
}

// volume is a CSI filesystem volume mounted on this node.
type volume struct {
	pvName string
	kind   kind
	mount  procfs.Mount
	// podUIDs are the pods on this node that mount the volume.
	podUIDs []string
}

type reading struct {
	stat procfs.Counters
	at   time.Time
}

// claim identifies the PVC bound to a PV.
type claim struct {
	namespace, name string
	uid             types.UID
}

// Collector samples the node's CSI volumes and publishes a VolumeIOSample
// per PVC. It implements manager.Runnable.
type Collector struct {
	// Client is the controller runtime k8s client.
	Client client.Client
	// Reader reads PVs, VolumeTierStates and VolumeIOSamples straight from
	// the API server, so the agent does not cache those cluster-wide on
	// every node.
	Reader client.Reader
	Config Config

	volumes map[string]volume // by PV name
	prev    map[string]reading
	windows map[string]*Window // by device
	claims  map[string]claim   // by PV name
	// removed holds PVs whose sample was already deleted because the PVC is
	// unmanaged, so the delete is not repeated every publish.
	removed map[string]bool
}

// Start runs the sample and publish loops until ctx is cancelled.
func (c *Collector) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("collector")
	c.prev = make(map[string]reading)
	c.windows = make(map[string]*Window)
	c.claims = make(map[string]claim)
	c.removed = make(map[string]bool)

	sampleTicker := time.NewTicker(c.Config.SampleInterval)
	defer sampleTicker.Stop()
	publishTicker := time.NewTicker(c.Config.PublishInterval)
	defer publishTicker.Stop()

	log.Info("Starting volume sampling", "node", c.Config.NodeName,
		"sampleInterval", c.Config.SampleInterval, "publishInterval", c.Config.PublishInterval)

	if err := c.sample(time.Now()); err != nil {
		log.Error(err, "Failed to sample volumes")
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-sampleTicker.C:
			if err := c.sample(now); err != nil {
				log.Error(err, "Failed to sample volumes")
			}
		case <-publishTicker.C:
			c.publish(ctx)
		}
	}
}

// kind is how a volume's I/O is measured.
type kind string

const (
	kindBlock kind = "block" // /proc/diskstats
	kindCifs  kind = "cifs"  // /proc/fs/cifs/Stats
	kindNFS   kind = "nfs"   // /proc/1/mountstats
	kindNone  kind = ""      // nothing can measure it
)

func mountKind(m procfs.Mount) kind {
	switch {
	case !strings.HasPrefix(m.Device, "0:"):
		return kindBlock
	case m.FSType == "cifs":
		return kindCifs
	case m.FSType == "nfs", m.FSType == "nfs4":
		return kindNFS
	}
	return kindNone
}

// sample rediscovers the node's volumes and adds one interval per device.
func (c *Collector) sample(now time.Time) error {
	mf, err := os.Open(filepath.Join(c.Config.ProcRoot, "1", "mountinfo"))
	if err != nil {
		return fmt.Errorf("cannot open host mountinfo: %w", err)
	}
	mounts, err := procfs.ParseMountInfo(mf)
	_ = mf.Close()
	if err != nil {
		return err
	}

	df, err := os.Open(filepath.Join(c.Config.ProcRoot, "diskstats"))
	if err != nil {
		return fmt.Errorf("cannot open diskstats: %w", err)
	}
	disk, err := procfs.ParseDiskStats(df)
	_ = df.Close()
	if err != nil {
		return err
	}
	c.volumes = discoverVolumes(mounts, disk)

	// Read on the first volume that needs them.
	var cifs, nfs map[string]procfs.Counters
	live := make(map[string]bool, len(c.volumes))
	for _, v := range c.volumes {
		live[v.mount.Device] = true

		var cur procfs.Counters
		var ok bool
		switch v.kind {
		case kindBlock:
			var d procfs.DiskStat
			d, ok = disk[v.mount.Device]
			cur = d.Counters()
		case kindCifs:
			if cifs == nil {
				if cifs, err = c.readCifsStats(); err != nil {
					return err
				}
			}
			cur, ok = cifs[v.mount.Source]
		case kindNFS:
			if nfs == nil {
				if nfs, err = c.readMountStats(); err != nil {
					return err
				}
			}
			cur, ok = nfs[v.mount.MountPoint]
		default:
			continue // no source can measure it
		}
		if !ok {
			continue
		}

		if prev, seen := c.prev[v.mount.Device]; seen {
			if iv, ok := procfs.Delta(prev.stat, cur, now.Sub(prev.at)); ok {
				w := c.windows[v.mount.Device]
				if w == nil {
					w = NewWindow(c.Config.Window)
					c.windows[v.mount.Device] = w
				}
				w.Add(iv)
			}
		}
		c.prev[v.mount.Device] = reading{stat: cur, at: now}
	}

	for pv := range c.claims {
		if _, ok := c.volumes[pv]; !ok {
			DeleteVolumeMetrics(pv)
		}
	}

	// Forget devices whose volume was unmounted.
	maps.DeleteFunc(c.prev, func(dev string, _ reading) bool { return !live[dev] })
	maps.DeleteFunc(c.windows, func(dev string, _ *Window) bool { return !live[dev] })
	maps.DeleteFunc(c.claims, func(pv string, _ claim) bool { _, ok := c.volumes[pv]; return !ok })
	maps.DeleteFunc(c.removed, func(pv string, _ bool) bool { _, ok := c.volumes[pv]; return !ok })
	return nil
}

// readCifsStats reads /proc/fs/cifs/Stats. The file is missing when the
// cifs module isn't loaded, which means no shares.
func (c *Collector) readCifsStats() (map[string]procfs.Counters, error) {
	f, err := os.Open(filepath.Join(c.Config.ProcRoot, "fs", "cifs", "Stats"))
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]procfs.Counters{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot open cifs stats: %w", err)
	}
	defer func() { _ = f.Close() }()
	return procfs.ParseCifsStats(f)
}

// readMountStats reads the host's /proc/1/mountstats for NFS counters.
func (c *Collector) readMountStats() (map[string]procfs.Counters, error) {
	f, err := os.Open(filepath.Join(c.Config.ProcRoot, "1", "mountstats"))
	if err != nil {
		return nil, fmt.Errorf("cannot open host mountstats: %w", err)
	}
	defer func() { _ = f.Close() }()
	return procfs.ParseMountStats(f)
}

// discoverVolumes finds kubelet CSI volumes and groups them by PV: filesystem
// mounts, and block-mode volumes resolved to their disk through disk.
func discoverVolumes(mounts []procfs.Mount, disk map[string]procfs.DiskStat) map[string]volume {
	// Block-mode volumes name their device ("nvme0n4"); diskstats maps names
	// to "major:minor".
	byName := make(map[string]string, len(disk))
	for dev, d := range disk {
		byName[d.Name] = dev
	}

	vols := make(map[string]volume)
	for _, m := range mounts {
		podUID, pv, ok := procfs.ParseCSIMountPoint(m.MountPoint)
		if !ok {
			if podUID, pv, ok = procfs.ParseCSIBlockPath(m.MountPoint); !ok {
				continue
			}
			// The bind mount's own device is devtmpfs; use the disk's.
			dev, found := byName[strings.TrimPrefix(m.Root, "/")]
			if !found {
				continue
			}
			m.Device = dev
		}
		v := vols[pv]
		v.pvName, v.mount, v.kind = pv, m, mountKind(m)
		if !slices.Contains(v.podUIDs, podUID) {
			v.podUIDs = append(v.podUIDs, podUID)
		}
		vols[pv] = v
	}
	return vols
}

// publish writes a VolumeIOSample for every volume with a full summary.
func (c *Collector) publish(ctx context.Context) {
	log := logf.FromContext(ctx).WithName("collector")
	for _, v := range c.volumes {
		// Volumes nothing can measure still get a sample, with no figures,
		// so the controller can say why.
		var volMetrics VolumeMetrics
		if v.kind != kindNone {
			w := c.windows[v.mount.Device]
			if w == nil {
				continue
			}
			var ok bool
			if volMetrics, ok = w.Summarize(); !ok {
				continue
			}
		}
		cl, err := c.claimFor(ctx, v.pvName)
		if err != nil {
			log.Error(err, "Failed to resolve PVC for PersistentVolume", "pv", v.pvName)
			continue
		}
		if cl == nil {
			continue
		}

		managed, err := c.managed(ctx, *cl)
		if err != nil {
			log.Error(err, "Failed to check whether PVC is managed", "namespace", cl.namespace, "pvc", cl.name)
			continue
		}
		if !managed {
			c.removeSample(ctx, *cl, v.pvName)
			continue
		}
		delete(c.removed, v.pvName)

		psi := c.maxPressure(v.podUIDs)
		if err := c.writeSample(ctx, *cl, v, volMetrics, psi); err != nil {
			log.Error(err, "Failed to write VolumeIOSample", "namespace", cl.namespace, "pvc", cl.name)
		}

		if v.kind != kindNone {
			SetVolumeMetrics(*cl, v.pvName, c.Config.NodeName, volMetrics, psi)
		}
	}
}

// measured is the sample's Measured condition: which figures v's source
// could report.
func measured(v volume, s VolumeMetrics) metav1.Condition {
	cond := metav1.Condition{Type: tierdv1.VolumeIOSampleMeasured, Status: metav1.ConditionTrue}
	if v.kind == kindNone {
		cond.Status = metav1.ConditionFalse
		cond.Reason = tierdv1.MeasuredNoSource
		cond.Message = fmt.Sprintf("tierd cannot measure %s volumes", v.mount.FSType)
		return cond
	}
	var missing []string
	if !s.HasLatency {
		missing = append(missing, "latency", "queue depth")
	}
	if !s.HasUtilization {
		missing = append(missing, "utilization")
	}
	if len(missing) == 0 {
		cond.Reason = tierdv1.MeasuredComplete
		return cond
	}
	cond.Reason = tierdv1.MeasuredPartial
	cond.Message = fmt.Sprintf("%s volumes report no %s", v.kind, strings.Join(missing, ", "))
	return cond
}

// managed reports whether the policy controller has assigned the PVC to a
// policy: its VolumeTierState exists and has a policyRef. PVCs in conflict
// between policies have an empty policyRef and are not sampled.
func (c *Collector) managed(ctx context.Context, cl claim) (bool, error) {
	state := &tierdv1.VolumeTierState{}
	if err := c.Reader.Get(ctx, client.ObjectKey{Namespace: cl.namespace, Name: cl.name}, state); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	return state.Status.PolicyRef != "", nil
}

// removeSample deletes the VolumeIOSample of a PVC that is no longer managed,
// once per PV.
func (c *Collector) removeSample(ctx context.Context, cl claim, pvName string) {
	if c.removed[pvName] {
		return
	}
	sample := &tierdv1.VolumeIOSample{Name: cl.name, Namespace: cl.namespace}
	err := c.Client.Delete(ctx, sample)
	if client.IgnoreNotFound(err) != nil {
		logf.FromContext(ctx).Error(err, "Failed to delete VolumeIOSample of unmanaged PVC",
			"namespace", cl.namespace, "name", cl.name)
		return
	}
	if err == nil {
		logf.FromContext(ctx).Info("Deleted VolumeIOSample of unmanaged PVC", "namespace", cl.namespace, "name", cl.name)
	}
	c.removed[pvName] = true
	DeleteVolumeMetrics(pvName)
}

// claimFor returns the PVC bound to pv, or nil if it is not bound.
func (c *Collector) claimFor(ctx context.Context, pvName string) (*claim, error) {
	if cl, ok := c.claims[pvName]; ok {
		return &cl, nil
	}
	pv := &corev1.PersistentVolume{}
	if err := c.Reader.Get(ctx, client.ObjectKey{Name: pvName}, pv); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	ref := pv.Spec.ClaimRef
	if ref == nil || ref.UID == "" {
		return nil, nil
	}
	cl := claim{namespace: ref.Namespace, name: ref.Name, uid: ref.UID}
	c.claims[pvName] = cl
	return &cl, nil
}

// maxPressure returns the highest io.pressure "some" avg60 among the pods,
// or nil if none could be read (for example on cgroup v1).
func (c *Collector) maxPressure(podUIDs []string) *float64 {
	var best *float64
	for _, uid := range podUIDs {
		path, err := podCgroupPath(c.Config.CgroupRoot, uid)
		if err != nil {
			continue
		}
		f, err := os.Open(filepath.Join(path, "io.pressure"))
		if err != nil {
			continue
		}
		p, err := procfs.ParsePressure(f)
		_ = f.Close()
		if err != nil {
			continue
		}
		if best == nil || p.Some.Avg60 > *best {
			avg := p.Some.Avg60
			best = &avg
		}
	}
	return best
}

// podCgroupPath finds a pod's cgroup under the kubepods hierarchy for either
// the systemd driver (kubepods.slice/kubepods-burstable.slice/
// kubepods-burstable-pod<uid_with_underscores>.slice) or the cgroupfs driver
// (kubepods/burstable/pod<uid>).
func podCgroupPath(root, podUID string) (string, error) {
	systemdUID := strings.ReplaceAll(podUID, "-", "_")
	var candidates []string
	for _, qos := range []string{"", "burstable", "besteffort"} {
		if qos == "" {
			candidates = append(candidates,
				filepath.Join(root, "kubepods.slice", "kubepods-pod"+systemdUID+".slice"),
				filepath.Join(root, "kubepods", "pod"+podUID))
			continue
		}
		candidates = append(candidates,
			filepath.Join(root, "kubepods.slice", "kubepods-"+qos+".slice", "kubepods-"+qos+"-pod"+systemdUID+".slice"),
			filepath.Join(root, "kubepods", qos, "pod"+podUID))
	}
	for _, p := range candidates {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("no cgroup for pod %s: %w", podUID, fs.ErrNotExist)
}

// writeSample creates the PVC's VolumeIOSample if needed and updates its
// status.
func (c *Collector) writeSample(ctx context.Context, cl claim, v volume, s VolumeMetrics, psi *float64) error {
	sample := &tierdv1.VolumeIOSample{}
	err := c.Reader.Get(ctx, client.ObjectKey{Namespace: cl.namespace, Name: cl.name}, sample)
	if apierrors.IsNotFound(err) {
		sample = &tierdv1.VolumeIOSample{
			Name:      cl.name,
			Namespace: cl.namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "v1",
				Kind:       "PersistentVolumeClaim",
				Name:       cl.name,
				UID:        cl.uid,
			}},
		}
		if err := c.Client.Create(ctx, sample); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("cannot create VolumeIOSample: %w", err)
			}
			if err := c.Reader.Get(ctx, client.ObjectKeyFromObject(sample), sample); err != nil {
				return fmt.Errorf("cannot get VolumeIOSample: %w", err)
			}
		} else {
			logf.FromContext(ctx).Info("Created VolumeIOSample", "namespace", cl.namespace, "name", cl.name)
		}
	} else if err != nil {
		return fmt.Errorf("cannot get VolumeIOSample: %w", err)
	}
	if !ownedBy(sample, cl.uid) {
		// The PVC was deleted and recreated with the same name; the old
		// sample is garbage-collected, so leave it alone until then.
		return errors.New("VolumeIOSample is owned by a previous PVC with the same name")
	}

	base := sample.DeepCopy()
	now := metav1.Now()
	sample.Status = tierdv1.VolumeIOSampleStatus{
		NodeName:                 c.Config.NodeName,
		PVName:                   v.pvName,
		Device:                   v.mount.Device,
		ObservedAt:               &now,
		Window:                   metav1.Duration{Duration: s.Span},
		Samples:                  int32(s.Samples),
		IOPS:                     int64(math.Round(s.IOPS)),
		ThroughputBytesPerSecond: int64(math.Round(s.BytesPerSecond)),
		// Cloned: SetStatusCondition edits in place, and base must keep the
		// old conditions for the patch to include the change.
		Conditions: slices.Clone(base.Status.Conditions),
	}
	cond := measured(v, s)
	cond.ObservedGeneration = sample.Generation
	meta.SetStatusCondition(&sample.Status.Conditions, cond)
	if s.HasLatency {
		sample.Status.LatencyP50Micros = new(s.LatencyP50.Microseconds())
		sample.Status.LatencyP95Micros = new(s.LatencyP95.Microseconds())
		sample.Status.QueueDepth = milliQuantity(s.QueueDepth)
	}
	if s.HasUtilization {
		sample.Status.UtilizationPercent = new(int32(math.Round(s.Utilization * 100)))
	}
	if psi != nil {
		sample.Status.IOPressureSomeAvg60 = milliQuantity(*psi)
	}
	if err := c.Client.Status().Patch(ctx, sample, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("cannot patch VolumeIOSample status: %w", err)
	}
	return nil
}

func ownedBy(obj metav1.Object, uid types.UID) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.UID == uid {
			return true
		}
	}
	return false
}

// milliQuantity rounds v to three decimals.
func milliQuantity(v float64) *resource.Quantity {
	return resource.NewMilliQuantity(int64(math.Round(v*1000)), resource.DecimalSI)
}
