package agent

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const pvLabel = "persistentvolume"

var volumeLabels = []string{"namespace", "persistentvolumeclaim", pvLabel, "node"}

func volumeGauge(name, help string) *prometheus.GaugeVec {
	return prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "tierd", Subsystem: "volume", Name: name, Help: help,
	}, volumeLabels)
}

var (
	volumeIOPS        = volumeGauge("iops", "Completed reads and writes per second.")
	volumeThroughput  = volumeGauge("throughput_bytes_per_second", "Bytes read and written per second.")
	volumeLatencyP95  = volumeGauge("latency_p95_seconds", "95th percentile I/O latency.")
	volumeUtilization = volumeGauge("utilization_ratio", "Fraction of time the device was busy (0-1).")
	volumeQueueDepth  = volumeGauge("queue_depth", "Average number of I/Os in flight.")
	volumeIOPressure  = volumeGauge("io_pressure_some_avg60", "PSI some avg60 of the pods using the volume (0-1).")

	volumeGauges = []*prometheus.GaugeVec{
		volumeIOPS, volumeThroughput, volumeLatencyP95,
		volumeUtilization, volumeQueueDepth, volumeIOPressure,
	}
)

// SetVolumeMetrics updates all six I/O and PSI gauges for a given volume.
func SetVolumeMetrics(cl claim, pvName, node string, m VolumeMetrics, psi *float64) {
	l := prometheus.Labels{
		"namespace": cl.namespace, "persistentvolumeclaim": cl.name,
		pvLabel: pvName, "node": node,
	}
	volumeIOPS.With(l).Set(m.IOPS)
	volumeThroughput.With(l).Set(m.BytesPerSecond)
	if m.HasLatency {
		volumeLatencyP95.With(l).Set(m.LatencyP95.Seconds())
		volumeQueueDepth.With(l).Set(m.QueueDepth)
	} else {
		volumeLatencyP95.Delete(l)
		volumeQueueDepth.Delete(l)
	}
	if m.HasUtilization {
		volumeUtilization.With(l).Set(m.Utilization)
	} else {
		volumeUtilization.Delete(l)
	}

	if psi != nil {
		volumeIOPressure.With(l).Set(*psi / 100)
	} else {
		volumeIOPressure.Delete(l)
	}
}

// DeleteVolumeMetrics removes a deleted volume's series across all six gauges.
func DeleteVolumeMetrics(pvName string) {
	for _, g := range volumeGauges {
		g.DeletePartialMatch(prometheus.Labels{pvLabel: pvName})
	}
}

func init() {
	for _, g := range volumeGauges {
		metrics.Registry.MustRegister(g)
	}
}
