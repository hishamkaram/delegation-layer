//go:build linux

package lifetime

import (
	"strings"
	"testing"
)

const (
	unifiedCgroupMountInfo = "36 25 0:32 / /sys/fs/cgroup rw,nosuid,nodev,noexec,relatime - cgroup2 cgroup2 rw\n"
	// outOfViewCgroupMountInfo is the bind mount a native Docker run with a
	// private cgroup namespace and a read-only host cgroup bind reported:
	// root /../.. at /host-cgroup, beside the ordinary root / mount.
	outOfViewCgroupMountInfo = "41 36 0:32 /../.. /host-cgroup ro,nosuid,nodev,noexec,relatime - cgroup2 cgroup2 rw\n"
)

func TestResolveCgroupV2LocationMapsCanonicalResolver(t *testing.T) {
	tests := []struct {
		name      string
		cgroup    string
		mountInfo string
		want      [3]string
	}{
		{
			name:      "unified root mount",
			cgroup:    "0::/user.slice/user-1000.slice/session.scope\n",
			mountInfo: unifiedCgroupMountInfo,
			want:      [3]string{"/", "/sys/fs/cgroup", "/sys/fs/cgroup/user.slice/user-1000.slice/session.scope"},
		},
		{
			name:   "longest nested mount root wins",
			cgroup: "0::/user.slice/user-1000.slice/app.scope\n",
			mountInfo: unifiedCgroupMountInfo +
				"40 36 0:32 /user.slice/user-1000.slice /mnt/delegated rw - cgroup2 cgroup2 rw\n",
			want: [3]string{"/user.slice/user-1000.slice", "/mnt/delegated", "/mnt/delegated/app.scope"},
		},
		{
			name:      "escaped mount point",
			cgroup:    "0::/work.scope\n",
			mountInfo: "36 25 0:32 / /sys/fs/cgroup\\040delegated rw - cgroup2 cgroup2 rw\n",
			want:      [3]string{"/", "/sys/fs/cgroup delegated", "/sys/fs/cgroup delegated/work.scope"},
		},
		{
			name:   "hybrid unified mount",
			cgroup: "12:memory:/user.slice\n1:name=systemd:/user.slice/session.scope\n0::/user.slice/session.scope\n",
			mountInfo: "30 25 0:26 / /sys/fs/cgroup/memory rw - cgroup cgroup rw,memory\n" +
				"31 25 0:27 / /sys/fs/cgroup/unified rw - cgroup2 cgroup2 rw\n",
			want: [3]string{"/", "/sys/fs/cgroup/unified", "/sys/fs/cgroup/unified/user.slice/session.scope"},
		},
		{
			name:      "out-of-view bind mount after valid mount",
			cgroup:    "0::/\n",
			mountInfo: unifiedCgroupMountInfo + outOfViewCgroupMountInfo,
			want:      [3]string{"/", "/sys/fs/cgroup", "/sys/fs/cgroup"},
		},
		{
			name:      "out-of-view bind mount before valid mount",
			cgroup:    "0::/\n",
			mountInfo: outOfViewCgroupMountInfo + unifiedCgroupMountInfo,
			want:      [3]string{"/", "/sys/fs/cgroup", "/sys/fs/cgroup"},
		},
		{
			name:      "namespace root membership",
			cgroup:    "0::/\n",
			mountInfo: unifiedCgroupMountInfo,
			want:      [3]string{"/", "/sys/fs/cgroup", "/sys/fs/cgroup"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			location, err := ResolveCgroupV2Location([]byte(test.cgroup), []byte(test.mountInfo))
			if err != nil {
				t.Fatal(err)
			}
			got := [3]string{location.MountRoot(), location.MountPath(), location.CurrentPath()}
			if got != test.want {
				t.Fatalf("location = %q, want %q", got, test.want)
			}
		})
	}
}

func TestResolveCgroupV2LocationRejectsUnresolvableMembership(t *testing.T) {
	tests := []struct {
		name      string
		cgroup    string
		mountInfo string
		detail    string
	}{
		// Native bounded Docker --pid=host run with a private cgroup namespace
		// printed this membership for host PID 1 with mount root / at
		// /sys/fs/cgroup. It is outside the observer's view, not /init.scope.
		{name: "outside cgroup namespace view", cgroup: "0::/../../init.scope\n", mountInfo: unifiedCgroupMountInfo, detail: "namespace"},
		{name: "unclean dot component", cgroup: "0::/user.slice/./app.scope\n", mountInfo: unifiedCgroupMountInfo, detail: "malformed"},
		{name: "cgroup v1 only", cgroup: "1:name=systemd:/user.slice\n", mountInfo: "30 25 0:26 / /sys/fs/cgroup/systemd rw - cgroup cgroup rw,name=systemd\n", detail: "no v2 entry"},
		{name: "duplicate v2 entry", cgroup: "0::/one\n0::/two\n", mountInfo: unifiedCgroupMountInfo, detail: "duplicate"},
		{name: "relative v2 entry", cgroup: "0::relative\n", mountInfo: unifiedCgroupMountInfo, detail: "malformed"},
		{name: "no cgroup v2 mount", cgroup: "0::/app.scope\n", mountInfo: "30 25 0:26 / /sys/fs/cgroup/memory rw - cgroup cgroup rw,memory\n", detail: "no cgroup v2 mount"},
		{name: "membership outside every mount root", cgroup: "0::/system.slice/app.scope\n", mountInfo: "40 36 0:32 /user.slice /mnt/delegated rw - cgroup2 cgroup2 rw\n", detail: "contains the current cgroup"},
		{name: "only out-of-view bind mount", cgroup: "0::/\n", mountInfo: outOfViewCgroupMountInfo, detail: "no cgroup v2 mount"},
		{name: "only unclean mount root", cgroup: "0::/user.slice/x/app.scope\n", mountInfo: "36 25 0:32 /user.slice/./x /mnt rw - cgroup2 cgroup2 rw\n", detail: "no cgroup v2 mount"},
		{name: "unsupported mount escape", cgroup: "0::/app.scope\n", mountInfo: "36 25 0:32 / /sys/fs/cgroup\\012x rw - cgroup2 cgroup2 rw\n", detail: "escape"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			location, err := ResolveCgroupV2Location([]byte(test.cgroup), []byte(test.mountInfo))
			if err == nil || !strings.Contains(err.Error(), test.detail) {
				t.Fatalf("error = %v, want detail %q (location %q)", err, test.detail, location.CurrentPath())
			}
			if location != (CgroupV2Location{}) {
				t.Fatalf("failed resolution returned location %+v", location)
			}
		})
	}
}
