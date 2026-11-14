package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func BenchmarkCanonicalizePathDenseAncestor(b *testing.B) {
	for _, siblings := range []int{16, 4096} {
		b.Run(fmt.Sprintf("siblings=%d", siblings), func(b *testing.B) {
			path, want := canonicalBenchmarkPath(b, siblings)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				got, err := CanonicalizePath(path)
				if err != nil {
					b.Fatal(err)
				}
				if got != want {
					b.Fatalf("canonical path = %q, want %q", got, want)
				}
			}
		})
	}
}

func canonicalBenchmarkPath(b *testing.B, siblings int) (string, string) {
	b.Helper()
	base := b.TempDir()
	for index := range siblings {
		if err := os.Mkdir(filepath.Join(base, fmt.Sprintf("sibling-%06d", index)), 0o700); err != nil {
			b.Fatal(err)
		}
	}
	path := filepath.Join(base, "target", "inner", "leaf")
	if err := os.MkdirAll(path, 0o700); err != nil {
		b.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(path)
	if err != nil {
		b.Fatal(err)
	}
	return path, want
}
