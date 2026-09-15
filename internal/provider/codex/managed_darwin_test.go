//go:build darwin

package codex

import (
	"errors"
	"slices"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestManagedPreferencesReleaseEveryOwnedObject(t *testing.T) {
	for _, configured := range []bool{false, true} {
		checkManagedPreferenceOwnership(t, configured)
	}
}

func checkManagedPreferenceOwnership(t *testing.T, configured bool) {
	t.Helper()
	var names []string
	var released []uintptr
	api := preferenceAPI{
		create: func(_ uintptr, value string, encoding uint32) uintptr {
			if encoding != 0x08000100 {
				t.Fatal("preference string is not UTF-8")
			}
			names = append(names, value)
			return uintptr(len(names))
		},
		copyValue: func(key, domain uintptr) uintptr {
			if domain != 1 || key < 2 {
				t.Fatal("wrong native preference coordinates")
			}
			if configured {
				return 10
			}
			return 0
		},
		release: func(value uintptr) { released = append(released, value) },
	}
	sources, err := readManagedPreferences(api)
	if configured {
		if !errors.Is(err, ErrUnsupportedProfile) || !slices.Equal(released, []uintptr{2, 10, 1}) {
			t.Fatalf("configured preference not rejected/released: %v %v", err, released)
		}
	} else {
		if err != nil || len(sources) != 2 || !slices.Equal(released, []uintptr{2, 3, 1}) {
			t.Fatalf("absent preferences: %+v %v %v", sources, err, released)
		}
		if !slices.Equal(names, []string{"com.openai.codex", "config_toml_base64", "requirements_toml_base64"}) {
			t.Fatalf("wrong managed source lookup: %v", names)
		}
		if _, policyErr := sealPolicy(profileRequest(), profileEnvironment{}, sources, task.ComputeSHA256([]byte("test runtime"))); policyErr != nil {
			t.Fatalf("managed observation cannot enter immutable policy: %v", policyErr)
		}
	}
}

func TestManagedPreferenceAllocationFailureClosesDomain(t *testing.T) {
	for _, failedAt := range []int{1, 2, 3} {
		calls := 0
		var released []uintptr
		_, err := readManagedPreferences(preferenceAPI{
			create: func(uintptr, string, uint32) uintptr {
				calls++
				if calls == failedAt {
					return 0
				}
				return uintptr(calls)
			},
			copyValue: func(uintptr, uintptr) uintptr { return 0 },
			release:   func(value uintptr) { released = append(released, value) },
		})
		if !errors.Is(err, ErrUnsupportedProfile) {
			t.Fatalf("allocation failure accepted: %v", err)
		}
		want := map[int][]uintptr{1: nil, 2: {1}, 3: {2, 1}}[failedAt]
		if !slices.Equal(released, want) {
			t.Fatalf("failed allocation %d released %v want %v", failedAt, released, want)
		}
	}
}
