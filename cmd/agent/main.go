/*
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
*/

package main

import (
	"flag"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	tierdv1 "github.com/zanderyk/tierd/api/v1"
	"github.com/zanderyk/tierd/internal/agent"
)

func main() {
	var cfg agent.Config
	var metricsAddr, probeAddr string
	flag.StringVar(&cfg.NodeName, "node-name", os.Getenv("NODE_NAME"), "Node this agent runs on (defaults to $NODE_NAME).")
	flag.StringVar(&cfg.ProcRoot, "proc-root", "/host/proc", "Path where the host's /proc is mounted.")
	flag.StringVar(&cfg.CgroupRoot, "cgroup-root", "/host/sys/fs/cgroup",
		"Path where the host's cgroup v2 hierarchy is mounted.")
	flag.DurationVar(&cfg.SampleInterval, "sample-interval", 10*time.Second, "How often to read device counters.")
	flag.DurationVar(&cfg.PublishInterval, "publish-interval", time.Minute, "How often to write VolumeIOSamples.")
	flag.DurationVar(&cfg.Window, "window", time.Minute, "How much history each VolumeIOSample summarizes.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "Address for the metrics endpoint; 0 disables it.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address for the health probe endpoint.")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("setup")

	if cfg.NodeName == "" {
		log.Error(nil, "Node name is not set; pass --node-name or set NODE_NAME")
		os.Exit(1)
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(tierdv1.AddToScheme(scheme))

	// One agent per node: no leader election. All reads go through the API
	// reader, so the manager's cache never starts cluster-wide informers.
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
	})
	if err != nil {
		log.Error(err, "Failed to create manager")
		os.Exit(1)
	}

	if err := mgr.Add(&agent.Collector{
		Client: mgr.GetClient(),
		Reader: mgr.GetAPIReader(),
		Config: cfg,
	}); err != nil {
		log.Error(err, "Failed to add collector")
		os.Exit(1)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		log.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	log.Info("Starting agent", "node", cfg.NodeName)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "Failed to run agent")
		os.Exit(1)
	}
}
