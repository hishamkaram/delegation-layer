package journal

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestProbeGroupContextEstablishedGroupDoesNotMutate(t *testing.T) {
	for _, migrated := range []bool{false, true} {
		name := "root-journal"
		if migrated {
			name = "published-generation"
		}
		t.Run(name, func(t *testing.T) {
			store, group := openTestGroup(t)
			binding := groupTestBinding()
			if migrated {
				successor := group.Request()
				successor.Supervisor.Endpoint += ".successor"
				publishTestGroupMigration(t, store, group.Request(), successor)
				binding = successor.Supervisor
				generation, err := OpenGroupGeneration(t.Context(), store, binding)
				mustGroup(t, err)
				t.Cleanup(func() { mustGroup(t, generation.Close()) })
				group = generation
			}
			snapshot := testGroupSnapshot(group.Name(), "Running", 1)
			mustGroup(t, group.ObserveContext(t.Context(), snapshot))

			ready, err := probeGroupUnchanged(t, store, binding, snapshot)
			if !ready || err != nil {
				t.Fatalf("established group not ready: ready=%v err=%v", ready, err)
			}
		})
	}
}

func TestProbeGroupContextMissingStateDoesNotCreate(t *testing.T) {
	t.Run("journal", func(t *testing.T) {
		store := groupTestStore(t)
		snapshot := testGroupSnapshot(groupForRoot(store.RootID), "Running", 1)
		ready, err := probeGroupUnchanged(t, store, groupTestBinding(), snapshot)
		if ready || err != nil {
			t.Fatalf("missing journal was not deferred: ready=%v err=%v", ready, err)
		}
	})
	t.Run("observation", func(t *testing.T) {
		store, group := openTestGroup(t)
		snapshot := testGroupSnapshot(group.Name(), "Running", 1)
		ready, err := probeGroupUnchanged(t, store, groupTestBinding(), snapshot)
		if ready || err != nil {
			t.Fatalf("missing observation was not deferred: ready=%v err=%v", ready, err)
		}
	})
	for _, wrongName := range []bool{false, true} {
		name := "current-group"
		if wrongName {
			name = "different-current-group"
		}
		t.Run(name, func(t *testing.T) {
			store, group := openTestGroup(t)
			mustGroup(t, group.ObserveContext(t.Context(), testGroupSnapshot(group.Name(), "Running", 1)))
			snapshot := pueue.QueueSnapshot{}
			if wrongName {
				snapshot = testGroupSnapshot(group.Name()+"-other", "Running", 1)
			}
			ready, err := probeGroupUnchanged(t, store, groupTestBinding(), snapshot)
			if ready || err != nil {
				t.Fatalf("stored observation replaced missing current group: ready=%v err=%v", ready, err)
			}
		})
	}
}

func TestProbeGroupContextRejectsInvalidFreshSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   string
		parallel uint64
	}{
		{name: "paused", status: "Paused", parallel: 1},
		{name: "zero-width", status: "Running", parallel: 0},
		{name: "wide", status: "Running", parallel: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, group := openTestGroup(t)
			mustGroup(t, group.ObserveContext(t.Context(), testGroupSnapshot(group.Name(), "Running", 1)))
			snapshot := testGroupSnapshot(group.Name(), tc.status, tc.parallel)
			ready, err := probeGroupUnchanged(t, store, groupTestBinding(), snapshot)
			if ready || !errors.Is(err, pueue.ErrUnknown) {
				t.Fatalf("invalid fresh group was accepted: ready=%v err=%v", ready, err)
			}
		})
	}
}

func TestProbeGroupContextRejectsNoncanonicalRecords(t *testing.T) {
	for _, recordName := range []string{groupRequestRecordName, groupObservationRecordName} {
		for _, encoding := range []string{"reordered", "whitespace"} {
			t.Run(recordName+"/"+encoding, func(t *testing.T) {
				store, group := openTestGroup(t)
				snapshot := testGroupSnapshot(group.Name(), "Running", 1)
				mustGroup(t, group.ObserveContext(t.Context(), snapshot))
				path := filepath.Join(store.Root, "inspection-group", recordName)
				canonical, err := os.ReadFile(path)
				mustGroup(t, err)
				changed := noncanonicalGroupProbeRecord(t, canonical, encoding)
				mustGroup(t, os.WriteFile(path, changed, 0o600))

				ready, err := probeGroupUnchanged(t, store, groupTestBinding(), snapshot)
				if ready || !errors.Is(err, task.ErrEvidenceFault) {
					t.Fatalf("noncanonical %s accepted: ready=%v err=%v", recordName, ready, err)
				}
			})
		}
	}
}

func TestProbeGroupContextDefersDifferentHeadBinding(t *testing.T) {
	for _, publishedSuccessor := range []bool{false, true} {
		name := "different-binding"
		if publishedSuccessor {
			name = "retained-binding"
		}
		t.Run(name, func(t *testing.T) {
			store, group := openTestGroup(t)
			snapshot := testGroupSnapshot(group.Name(), "Running", 1)
			mustGroup(t, group.ObserveContext(t.Context(), snapshot))
			successor := group.Request()
			successor.Supervisor.Endpoint += ".successor"
			binding := successor.Supervisor
			if publishedSuccessor {
				publishTestGroupMigration(t, store, group.Request(), successor)
				binding = groupTestBinding()
			}
			ready, err := probeGroupUnchanged(t, store, binding, snapshot)
			if ready || err != nil {
				t.Fatalf("different head binding was not deferred: ready=%v err=%v", ready, err)
			}
		})
	}
}

func TestProbeGroupContextDefersMissingPublishedGeneration(t *testing.T) {
	store, group := openTestGroup(t)
	snapshot := testGroupSnapshot(group.Name(), "Running", 1)
	mustGroup(t, group.ObserveContext(t.Context(), snapshot))
	successor := group.Request()
	successor.Supervisor.Endpoint += ".successor"
	publishTestGroupMigration(t, store, group.Request(), successor)
	generationPath := filepath.Join(store.Root, "inspection-group-"+groupGeneration(successor.Supervisor))
	if _, err := os.Stat(generationPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture did not leave the published generation missing: %v", err)
	}

	ready, err := probeGroupUnchanged(t, store, successor.Supervisor, snapshot)
	if ready || err != nil {
		t.Fatalf("missing published generation was not deferred: ready=%v err=%v", ready, err)
	}
	if _, err := os.Stat(generationPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("probe materialized the published generation: %v", err)
	}
}

func probeGroupUnchanged(t *testing.T, store *taskdir.Store, binding task.SupervisorRef, snapshot pueue.QueueSnapshot) (bool, error) {
	t.Helper()
	before := groupProbeState(t, store.Root)
	fault := &groupProbeStageFault{}
	previous := store.FaultInjector()
	store.SetFaultInjector(fault)
	defer store.SetFaultInjector(previous)
	ready, err := ProbeGroupContext(t.Context(), store, binding, snapshot)
	if fault.attempts != 0 {
		t.Errorf("probe attempted to stage %d records", fault.attempts)
	}
	if after := groupProbeState(t, store.Root); !reflect.DeepEqual(before, after) {
		t.Error("probe changed persisted bytes or filesystem entries")
	}
	return ready, err
}

func groupProbeState(t *testing.T, root string) map[string]string {
	t.Helper()
	state := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			state[rel] = "directory"
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		state[rel] = "file:" + string(data)
		return nil
	})
	mustGroup(t, err)
	return state
}

func noncanonicalGroupProbeRecord(t *testing.T, canonical []byte, encoding string) []byte {
	t.Helper()
	var changed []byte
	if encoding == "reordered" {
		var fields map[string]json.RawMessage
		mustGroup(t, json.Unmarshal(canonical, &fields))
		data, err := task.MarshalCanonical(fields)
		mustGroup(t, err)
		changed = data
	} else {
		var indented bytes.Buffer
		mustGroup(t, json.Indent(&indented, canonical, "", "  "))
		changed = indented.Bytes()
	}
	if bytes.Equal(changed, canonical) {
		t.Fatal("fixture did not change the canonical encoding")
	}
	var before, after any
	mustGroup(t, json.Unmarshal(canonical, &before))
	mustGroup(t, json.Unmarshal(changed, &after))
	if !reflect.DeepEqual(before, after) {
		t.Fatal("fixture changed record values instead of only encoding")
	}
	return changed
}

type groupProbeStageFault struct {
	taskdir.BaseFaultInjector
	attempts int
}

func (f *groupProbeStageFault) OnBeforeStageCreate(_, _ string) error {
	f.attempts++
	return errors.New("group probe must not stage records")
}
