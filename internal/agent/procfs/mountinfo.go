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

// Package procfs parses the kernel files the node agent samples:
// mountinfo, diskstats and cgroup v2 io.pressure.
package procfs

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// Mount is one line of /proc/<pid>/mountinfo.
type Mount struct {
	// Device is the "major:minor" of the filesystem's device.
	Device     string
	Root       string
	MountPoint string
	FSType     string
	Source     string
}

// ParseMountInfo parses the mountinfo format described in proc(5).
func ParseMountInfo(r io.Reader) ([]Mount, error) {
	var mounts []Mount
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for line := 1; sc.Scan(); line++ {
		text := sc.Text()
		if text == "" {
			continue
		}
		// 36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw,errors=continue
		pre, post, ok := strings.Cut(text, " - ")
		if !ok {
			return nil, fmt.Errorf("mountinfo line %d: missing separator", line)
		}
		f := strings.Fields(pre)
		if len(f) < 6 {
			return nil, fmt.Errorf("mountinfo line %d: want at least 6 fields before separator, got %d", line, len(f))
		}
		g := strings.Fields(post)
		if len(g) < 2 {
			return nil, fmt.Errorf("mountinfo line %d: want fstype and source after separator", line)
		}
		mounts = append(mounts, Mount{
			Device:     f[2],
			Root:       unescapeOctal(f[3]),
			MountPoint: unescapeOctal(f[4]),
			FSType:     g[0],
			Source:     unescapeOctal(g[1]),
		})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("cannot read mountinfo: %w", err)
	}
	return mounts, nil
}

// unescapeOctal reverses the kernel's \ooo escaping of space, tab, newline
// and backslash in mountinfo paths.
func unescapeOctal(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// csiMountRE matches kubelet's per-pod mount path for a CSI filesystem
// volume: <kubelet>/pods/<pod-uid>/volumes/kubernetes.io~csi/<pv-name>/mount
var csiMountRE = regexp.MustCompile(`/pods/([0-9a-f-]+)/volumes/kubernetes\.io~csi/([^/]+)/mount$`)

// csiBlockRE matches kubelet's per-pod publish path for a CSI block-mode
// volume: <kubelet>/plugins/kubernetes.io/csi/volumeDevices/publish/<pv-name>/<pod-uid>.
// It is a bind mount of the device node, so the mount's Root is the node's
// path under /dev (e.g. "/nvme0n4").
var csiBlockRE = regexp.MustCompile(`/plugins/kubernetes\.io/csi/volumeDevices/publish/([^/]+)/([0-9a-f-]+)$`)

// ParseCSIBlockPath extracts the pod UID and PV name from a kubelet CSI
// block-mode publish path. ok is false for any other path.
func ParseCSIBlockPath(mountPoint string) (podUID, pvName string, ok bool) {
	m := csiBlockRE.FindStringSubmatch(mountPoint)
	if m == nil {
		return "", "", false
	}
	return m[2], m[1], true
}

// ParseCSIMountPoint extracts the pod UID and PV name from a kubelet CSI
// volume mount point. ok is false for any other path.
func ParseCSIMountPoint(mountPoint string) (podUID, pvName string, ok bool) {
	m := csiMountRE.FindStringSubmatch(mountPoint)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}
