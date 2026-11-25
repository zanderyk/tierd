//go:build e2e

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

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	tierdv1alpha1 "github.com/zanderyk/tierd/api/v1alpha1"
	"github.com/zanderyk/tierd/test/utils"
)

// Profile describes the cluster the suite runs against: how tierd gets
// there, which storage to tier, and how fast things happen. $E2E_PROFILE
// names the file, relative to the project root; testdata/profiles has one
// for kind and an example for AKS.
type Profile struct {
	// Kind sets up the kind cluster `make test-e2e` creates: builds the image
	// and loads it into kind, puts the CSI hostpath driver on a loop device,
	// and runs NFS and SMB servers and drivers for the source specs.
	Kind bool `json:"kind"`
	// Image is the tierd image to deploy. $IMG overrides it.
	Image string `json:"image"`
	// Deploy installs the CRDs and deploys tierd (`make deploy`) with Image
	// and Agent's intervals. Without it, tierd must already run in
	// tierd-system under its default names.
	Deploy bool `json:"deploy"`
	// Agent intervals, set on the agent when deploying.
	Agent AgentIntervals `json:"agent"`
	// Manifests are applied before the specs, relative to the project root:
	// the StorageClasses and VolumeAttributesClasses the profile names.
	Manifests []string `json:"manifests"`
	// ScaleFor is the policies' scale-up and scale-down `for` window.
	ScaleFor metav1.Duration `json:"scaleFor"`
	// Timeout bounds each wait for tierd or the driver to act.
	Timeout metav1.Duration `json:"timeout"`
	// LoadIOPS is fio's sustained write rate. It must exceed 80% of the base
	// tiers' IOPS ceilings, so sustained load scales up, and stay under the
	// fast tiers' ceilings.
	LoadIOPS int `json:"loadIOPS"`
	// PVCSize is the size of every PVC the suite creates.
	PVCSize resource.Quantity `json:"pvcSize"`
	// Tiering is the storage the tiering specs move between tiers.
	Tiering Tiering `json:"tiering"`
	// Sources are the volumes the measurement specs check, one spec each.
	Sources []Source `json:"sources"`
}

// AgentIntervals are the node agent's flags.
type AgentIntervals struct {
	SampleInterval  metav1.Duration `json:"sampleInterval"`
	PublishInterval metav1.Duration `json:"publishInterval"`
	Window          metav1.Duration `json:"window"`
}

// Tier is a VolumeAttributesClass and the ceilings a policy declares for it.
type Tier struct {
	Name            string `json:"name"`
	IOPS            int64  `json:"iops"`
	ThroughputMiBps int64  `json:"throughputMiBps"`
}

// Tiering is a StorageClass whose driver can modify volumes, and two of its
// classes.
type Tiering struct {
	StorageClass string `json:"storageClass"`
	Base         Tier   `json:"base"`
	Fast         Tier   `json:"fast"`
	// Rejected is a VolumeAttributesClass of the driver that the driver
	// refuses. Empty skips the rollback spec.
	Rejected string `json:"rejected,omitempty"`
	// Bursty is fio's bursty pattern. Each burst must keep the agent's
	// window over the base ceiling for well under ScaleFor. Unset skips the
	// flapping spec.
	Bursty *Bursty `json:"bursty,omitempty"`
}

// Bursty is fio writing at LoadIOPS for Burst, idling for Idle, and the
// spec watching for Observe.
type Bursty struct {
	Burst   metav1.Duration `json:"burst"`
	Idle    metav1.Duration `json:"idle"`
	Observe metav1.Duration `json:"observe"`
}

// Source is one kind of volume the agent measures.
type Source struct {
	// Name names the spec and the PVC, policy and fio pod it creates.
	Name         string `json:"name"`
	StorageClass string `json:"storageClass"`
	// Block makes a raw block (volumeMode: Block) PVC.
	Block bool `json:"block,omitempty"`
	// Direct writes with O_DIRECT; without it fio fsyncs each write, for
	// filesystems without O_DIRECT (cifs without cache=none).
	Direct bool `json:"direct,omitempty"`
	// Mode is the policy's mode. In Act the spec waits for the PVC to move
	// to Fast; in Recommend, for the recommendation.
	Mode tierdv1alpha1.VolumeTierPolicyMode `json:"mode"`
	Base Tier                               `json:"base"`
	Fast Tier                               `json:"fast"`
	// StartOnBase starts the PVC on Base; otherwise tierd adopts it.
	StartOnBase bool `json:"startOnBase,omitempty"`
	// Measured is the expected reason of the sample's Measured condition.
	Measured string `json:"measured"`
	// Latency expects the sample to report latency.
	Latency bool `json:"latency,omitempty"`
}

// profile is the profile the suite runs with, read when the package loads
// because spec trees are built before BeforeSuite runs.
var profile = mustLoadProfile()

func mustLoadProfile() *Profile {
	p, err := loadProfile()
	if err != nil {
		panic(fmt.Sprintf("e2e profile: %v", err))
	}
	return p
}

func loadProfile() (*Profile, error) {
	path := os.Getenv("E2E_PROFILE")
	if path == "" {
		path = "test/e2e/testdata/profiles/kind.yaml"
	}
	dir, err := utils.GetProjectDir()
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	p := &Profile{}
	if err := yaml.UnmarshalStrict(data, p); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if img := os.Getenv("IMG"); img != "" {
		p.Image = img
	}
	switch {
	case p.Deploy && p.Image == "":
		return nil, fmt.Errorf("%s: deploy needs an image; set image or $IMG", path)
	case p.ScaleFor.Duration <= 0, p.Timeout.Duration <= 0, p.LoadIOPS <= 0, p.PVCSize.IsZero():
		return nil, fmt.Errorf("%s: scaleFor, timeout, loadIOPS and pvcSize are required", path)
	case p.Tiering.StorageClass == "" || p.Tiering.Base.Name == "" || p.Tiering.Fast.Name == "":
		return nil, fmt.Errorf("%s: tiering needs storageClass, base and fast", path)
	}
	return p, nil
}

// scaleFor is the policies' `for` window.
func scaleFor() time.Duration { return profile.ScaleFor.Duration }
