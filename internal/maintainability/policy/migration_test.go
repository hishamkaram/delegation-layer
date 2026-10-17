package policy

import (
	"strings"
	"testing"
)

func TestMigrationAcceptsOnlyUnchangedOwnedBaseline(t *testing.T) {
	digest := strings.Repeat("a", 64)
	finding := Finding{Path: "legacy.py", Kind: "size", Detail: "file exceeds physical line budget"}
	sources := []Source{{Path: finding.Path, SHA256: digest}}
	entry := Migration{Path: finding.Path, Kind: finding.Kind, Detail: finding.Detail, SHA256: digest, Owner: "acceptance", Stage: "acceptance-split"}
	findings, deferred := ApplyMigration([]Finding{finding}, sources, []Migration{entry}, false)
	if len(findings) != 0 || deferred != 1 {
		t.Fatalf("unchanged baseline rejected: %+v, %d", findings, deferred)
	}
	sources[0].SHA256 = strings.Repeat("b", 64)
	findings, deferred = ApplyMigration([]Finding{finding}, sources, []Migration{entry}, false)
	if len(findings) != 2 || deferred != 0 {
		t.Fatalf("modified legacy file was waived: %+v, %d", findings, deferred)
	}
}

func TestMigrationRejectsStaleAndDuplicateEntries(t *testing.T) {
	entry := Migration{Path: "gone.py", Kind: "size", Detail: "old", SHA256: strings.Repeat("a", 64), Owner: "owner", Stage: "stage"}
	findings, _ := ApplyMigration(nil, nil, []Migration{entry}, false)
	if len(findings) != 1 {
		t.Fatalf("stale entry accepted: %+v", findings)
	}
	findings, _ = ApplyMigration(nil, nil, []Migration{entry, entry}, false)
	if len(findings) != 2 {
		t.Fatalf("duplicate entry accepted: %+v", findings)
	}
}

func TestStrictMigrationRejectsEveryNonemptyInventory(t *testing.T) {
	digest := strings.Repeat("a", 64)
	finding := Finding{Path: "legacy.py", Kind: "size", Detail: "file exceeds physical line budget"}
	sources := []Source{{Path: finding.Path, SHA256: digest}}
	entry := Migration{Path: finding.Path, Kind: finding.Kind, Detail: finding.Detail, SHA256: digest, Owner: "owner", Stage: "stage"}
	for _, current := range [][]Finding{nil, {finding}} {
		findings, deferred := ApplyMigration(current, sources, []Migration{entry}, true)
		if len(findings) != len(current)+1 || deferred != 0 {
			t.Fatalf("strict audit accepted or applied a nonempty inventory: %+v, deferred=%d", findings, deferred)
		}
		if findings[len(findings)-1].Kind != "migration" || !strings.Contains(findings[len(findings)-1].Detail, "empty") {
			t.Fatalf("strict audit did not identify the inventory requirement: %+v", findings)
		}
	}
	findings, deferred := ApplyMigration([]Finding{finding}, sources, nil, true)
	if len(findings) != 1 || findings[0] != finding || deferred != 0 {
		t.Fatalf("strict audit changed an unwaived finding: %+v, %d", findings, deferred)
	}
	if findings, deferred = ApplyMigration(nil, sources, nil, true); len(findings) != 0 || deferred != 0 {
		t.Fatalf("strict audit rejected clean source with an empty inventory: %+v, %d", findings, deferred)
	}
}
