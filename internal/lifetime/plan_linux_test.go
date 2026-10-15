//go:build linux

package lifetime

import (
	"errors"
	"strings"
	"testing"
)

func TestCgroupPlanRoundTrip(t *testing.T) {
	want := cgroupPlanLocator{
		parent: "/sys/fs/cgroup/user.slice/test",
		dev:    7,
		ino:    11,
		name:   "delegate-v1-0123456789abcdef0123456789abcdef",
	}
	encoded := encodeCgroupPlan(want)
	got, err := decodeCgroupPlan(encoded)
	if err != nil {
		t.Fatalf("decodeCgroupPlan: %v", err)
	}
	if got != want {
		t.Fatalf("decoded locator = %#v, want %#v", got, want)
	}
}

func TestCgroupPlanRejectsMalformedIdentity(t *testing.T) {
	valid := encodeCgroupPlan(cgroupPlanLocator{
		parent: "/sys/fs/cgroup/user.slice/test",
		dev:    7,
		ino:    11,
		name:   "delegate-v1-0123456789abcdef0123456789abcdef",
	})
	for _, value := range []string{
		"",
		"cgroup2-plan:bad",
		strings.Replace(valid, ":7:", ":bad:", 1),
		strings.Replace(valid, ":11:", ":0:", 1),
		strings.Replace(valid, "delegate-v1-", "../", 1),
	} {
		if _, err := decodeCgroupPlan(value); err == nil {
			t.Fatalf("decodeCgroupPlan accepted %q", value)
		}
	}
}

func TestValidatePlannedCgroupParentAcceptsMountRoot(t *testing.T) {
	mountPath := "/sys/fs/cgroup"
	for _, parent := range []string{mountPath, mountPath + "/user.slice/test"} {
		if err := validatePlannedCgroupParent(mountPath, cgroupPlanLocator{parent: parent}); err != nil {
			t.Fatalf("parent %q rejected: %v", parent, err)
		}
	}
	for _, parent := range []string{"/sys/fs/cgroup-other", "/tmp"} {
		if err := validatePlannedCgroupParent(mountPath, cgroupPlanLocator{parent: parent}); !errors.Is(err, ErrInvalidRef) {
			t.Fatalf("parent %q error = %v, want ErrInvalidRef", parent, err)
		}
	}
}
