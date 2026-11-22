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

package procfs

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
)

func TestParseMountInfo(t *testing.T) {
	mounts, err := ParseMountInfo(open(t, "mountinfo"))
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 7 {
		t.Fatalf("got %d mounts, want 7", len(mounts))
	}

	csi := mounts[3]
	if csi.Device != "8:32" || csi.FSType != "ext4" || csi.Source != "/dev/sdc" {
		t.Errorf("CSI mount = %+v", csi)
	}
	uid, pv, ok := ParseCSIMountPoint(csi.MountPoint)
	if !ok || uid != "3b7e2a10-6c1d-4a7e-9f0a-1d2c3b4a5e6f" || pv != "pvc-0a1b2c3d" {
		t.Errorf("ParseCSIMountPoint = %q, %q, %v", uid, pv, ok)
	}

	for _, i := range []int{0, 2, 4} {
		if _, _, ok := ParseCSIMountPoint(mounts[i].MountPoint); ok {
			t.Errorf("mount %d (%s) parsed as a CSI volume mount", i, mounts[i].MountPoint)
		}
	}

	nfs := mounts[5]
	if nfs.MountPoint != "/mnt/with space" || nfs.Source != "10.0.0.4:/export one" {
		t.Errorf("octal escapes not decoded: %+v", nfs)
	}

	// Captured on AKS: a block-mode PVC's publish path, a bind mount of the
	// device node.
	block := mounts[6]
	if block.Root != "/nvme0n4" || block.FSType != "devtmpfs" {
		t.Errorf("block-mode mount = %+v", block)
	}
	uid, pv, ok = ParseCSIBlockPath(block.MountPoint)
	if !ok || uid != "6903c4a1-00cf-48d5-8155-242d81a4d92a" || pv != "pvc-243b4d34-9bc4-4f96-b0e9-4c0238cb1ed0" {
		t.Errorf("ParseCSIBlockPath = %q, %q, %v", uid, pv, ok)
	}
	if _, _, ok := ParseCSIBlockPath(csi.MountPoint); ok {
		t.Error("filesystem mount parsed as a block-mode volume")
	}
	// kubelet's second path for the same volume must not match, or the
	// volume would be counted twice.
	staging := "/var/lib/kubelet/plugins/kubernetes.io/csi/volumeDevices/pvc-243b4d34/dev/6903c4a1-00cf-48d5-8155-242d81a4d92a"
	if _, _, ok := ParseCSIBlockPath(staging); ok {
		t.Error("volumeDevices/<pv>/dev/<pod> path parsed as a block-mode volume")
	}
}

func TestParseMountInfoMalformed(t *testing.T) {
	if _, err := ParseMountInfo(strings.NewReader("22 1 8:1 / / rw\n")); err == nil {
		t.Error("want error for a line without the \" - \" separator")
	}
}

func TestParseMountInfoErrors(t *testing.T) {
	tests := []struct {
		name, in string
	}{
		{"missing separator", "22 1 8:1 / / rw\n"},
		{"too few fields before separator", "22 1 8:1 / - ext4 /dev/sda1 rw\n"},
		{"too few fields after separator", "22 1 8:1 / / rw - ext4\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseMountInfo(strings.NewReader(tt.in)); err == nil {
				t.Errorf("want error for %q", tt.in)
			}
		})
	}
}

func TestParseMountInfoOptionalFields(t *testing.T) {
	// Zero or more optional fields (shared:N, master:N, ...) sit between the
	// mount options and the separator; blank lines are skipped.
	in := "\n" +
		"22 1 8:1 / / rw - ext4 /dev/sda1 rw\n" +
		"23 22 8:2 /sub /data rw,noatime shared:1 master:2 propagate_from:3 - xfs /dev/sda2 rw\n"
	mounts, err := ParseMountInfo(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := []Mount{
		{Device: "8:1", Root: "/", MountPoint: "/", FSType: "ext4", Source: "/dev/sda1"},
		{Device: "8:2", Root: "/sub", MountPoint: "/data", FSType: "xfs", Source: "/dev/sda2"},
	}
	if !slices.Equal(mounts, want) {
		t.Errorf("got %+v, want %+v", mounts, want)
	}
}

func TestParseMountInfoEmpty(t *testing.T) {
	mounts, err := ParseMountInfo(strings.NewReader(""))
	if err != nil || len(mounts) != 0 {
		t.Errorf("got %+v, %v; want no mounts and no error", mounts, err)
	}
}

func TestParseMountInfoReadError(t *testing.T) {
	if _, err := ParseMountInfo(iotest.ErrReader(errors.New("boom"))); err == nil {
		t.Error("want error when the reader fails")
	}
}

func TestUnescapeOctal(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"/plain/path", "/plain/path"},
		{`/a\040b`, "/a b"},
		{`/a\011b`, "/a\tb"},
		{`/a\012b`, "/a\nb"},
		{`/a\134b`, `/a\b`},
		{`\040`, " "},
		{`/end\040`, "/end "},
		{`/two\040\040`, "/two  "},
		{`/short\04`, `/short\04`},
		{`/trailing\`, `/trailing\`},
		{`/notoctal\089`, `/notoctal\089`},
		{`/overflow\777`, `/overflow\777`},
	}
	for _, tt := range tests {
		if got := unescapeOctal(tt.in); got != tt.want {
			t.Errorf("unescapeOctal(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParseCSIMountPoint(t *testing.T) {
	const uid = "3b7e2a10-6c1d-4a7e-9f0a-1d2c3b4a5e6f"
	tests := []struct {
		path       string
		uid, pv    string
		wantParsed bool
	}{
		{"/var/lib/kubelet/pods/" + uid + "/volumes/kubernetes.io~csi/pvc-1/mount", uid, "pvc-1", true},
		{"/custom/kubelet/pods/" + uid + "/volumes/kubernetes.io~csi/my-pv/mount", uid, "my-pv", true},
		// Block-mode volumes and the global staging mount are not per-pod
		// filesystem mounts.
		{"/var/lib/kubelet/pods/" + uid + "/volumeDevices/kubernetes.io~csi/pvc-1", "", "", false},
		{"/var/lib/kubelet/plugins/kubernetes.io/csi/ebs.csi.aws.com/abc/globalmount", "", "", false},
		{"/var/lib/kubelet/pods/" + uid + "/volumes/kubernetes.io~empty-dir/cache", "", "", false},
		{"/var/lib/kubelet/pods/" + uid + "/volumes/kubernetes.io~csi/pvc-1/mount/sub", "", "", false},
		{"/var/lib/kubelet/pods/NOT-A-UID/volumes/kubernetes.io~csi/pvc-1/mount", "", "", false},
		{"", "", "", false},
	}
	for _, tt := range tests {
		gotUID, pv, ok := ParseCSIMountPoint(tt.path)
		if gotUID != tt.uid || pv != tt.pv || ok != tt.wantParsed {
			t.Errorf("ParseCSIMountPoint(%q) = %q, %q, %v; want %q, %q, %v",
				tt.path, gotUID, pv, ok, tt.uid, tt.pv, tt.wantParsed)
		}
	}
}
