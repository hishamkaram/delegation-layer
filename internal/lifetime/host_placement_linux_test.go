//go:build linux

package lifetime

import (
	"errors"
	"strings"
	"testing"
)

func TestHostPlacementCgroupMembership(t *testing.T) {
	name := "delegate-v1-" + strings.Repeat("a", 32)
	for _, tc := range []struct {
		data      string
		contained bool
		fault     bool
	}{
		{data: "0::/user.slice/shared.scope\n"},
		{data: "0::/user.slice/" + name + "/nested\n", contained: true},
		{data: "0::/user.slice/" + name + "-unrelated\n"},
		{data: "0::/user.slice/delegate-v1-short\n"},
		{data: "4:cpu:/shared\n"},
		{data: "0::/../unknown\n", fault: true},
		{data: "0::/first\n0::/second\n", fault: true},
		{fault: true},
	} {
		err := checkHostCgroupMembership([]byte(tc.data))
		if errors.Is(err, ErrHostContained) != tc.contained || (err != nil) != (tc.contained || tc.fault) {
			t.Fatalf("membership %q: %v", tc.data, err)
		}
	}
}
