package control

import (
	"errors"
	"strings"
	"testing"
)

func TestUnixSocketPathByteBound(t *testing.T) {
	for _, fixture := range []struct {
		name    string
		goos    string
		bytes   int
		tooLong bool
		invalid bool
	}{
		{name: "Linux boundary", goos: "linux", bytes: 107},
		{name: "Linux oversized", goos: "linux", bytes: 108, tooLong: true},
		{name: "Darwin boundary", goos: "darwin", bytes: 103},
		{name: "Darwin oversized", goos: "darwin", bytes: 104, tooLong: true},
		{name: "Darwin multibyte pathname", goos: "darwin", bytes: len(strings.Repeat("é", 52)), tooLong: true},
		{name: "unsupported host", goos: "windows", bytes: 20, invalid: true},
		{name: "invalid count", goos: "linux", bytes: -1, invalid: true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			err := ValidateUnixSocketPathBytes(fixture.goos, fixture.bytes)
			if fixture.tooLong {
				if !errors.Is(err, errUnixSocketPathTooLong) || !errors.Is(err, ErrConfiguration) {
					t.Fatalf("native bound classification lost: %v", err)
				}
			} else if fixture.invalid {
				if !errors.Is(err, ErrConfiguration) || errors.Is(err, errUnixSocketPathTooLong) {
					t.Fatalf("invalid byte-count request classification: %v", err)
				}
			} else if err != nil {
				t.Fatalf("representable pathname rejected: %v", err)
			}
		})
	}
}
