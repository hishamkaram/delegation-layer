package testsupport

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// pairFaultRecords are the private generation records and the fake daemon's
// start log. A refused mutation leaves every one byte-identical.
var pairFaultRecords = []string{
	"supervisor.identity.json", "daemon.identity.json", "bootstrap.pending",
	"migration.json", "migration-history.json", "identity.pending.json", "daemon-started",
}

// PairFaultFile is one record's presence and exact bytes.
type PairFaultFile struct {
	Present bool
	data    string
}

func CapturePairFaultWitness(t *testing.T, base string) map[string]PairFaultFile {
	t.Helper()
	witness := make(map[string]PairFaultFile, len(pairFaultRecords))
	for _, name := range pairFaultRecords {
		data, err := os.ReadFile(filepath.Join(base, name))
		switch {
		case errors.Is(err, os.ErrNotExist):
			witness[name] = PairFaultFile{}
		case err != nil:
			t.Fatal(err)
		default:
			witness[name] = PairFaultFile{Present: true, data: string(data)}
		}
	}
	return witness
}

func AssertPairFaultWitness(t *testing.T, base string, before map[string]PairFaultFile) {
	t.Helper()
	after := CapturePairFaultWitness(t, base)
	for _, name := range pairFaultRecords {
		if after[name] != before[name] {
			t.Fatalf("refused pair recovery changed %s (present %t -> %t)", name, before[name].Present, after[name].Present)
		}
	}
}
