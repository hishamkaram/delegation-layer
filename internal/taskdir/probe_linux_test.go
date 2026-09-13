//go:build linux

package taskdir

import (
	"errors"
	"testing"
)

func TestLinuxFilesystemPolicy(t *testing.T) {
	for _, kind := range []uint32{0xef53, 0x58465342, 0x9123683e} {
		if err := checkLinuxFilesystemType(kind); err != nil {
			t.Fatal(err)
		}
	}
	for _, kind := range []uint32{0x01021994, 0x858458f6, 0x6969, 0x517b, 0xff534d42, 0x794c7630, 0} {
		if err := checkLinuxFilesystemType(kind); !errors.Is(err, ErrUnsupportedFilesystem) {
			t.Fatalf("accepted magic%x: %v", kind, err)
		}
	}
}
