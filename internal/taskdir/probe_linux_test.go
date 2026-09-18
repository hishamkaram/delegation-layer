//go:build linux

package taskdir

import "testing"

func TestLinuxFilesystemPolicyDoesNotAllowlistTypes(t *testing.T) {
	if err := checkPlatformFilesystemPolicy(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}
