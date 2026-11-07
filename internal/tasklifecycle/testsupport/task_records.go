package testsupport

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func NewAppTestTask(t *testing.T, sealed bool) (*taskdir.Store, *taskdir.TaskDir, *task.TaskRecord) {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	td, req := NewAppTaskInStore(t, store, strings.Repeat("c", 32), sealed, nil)
	return store, td, req
}

func NewAppTaskInStore(t *testing.T, store *taskdir.Store, id string, sealed bool, prior *task.PriorSession) (*taskdir.TaskDir, *task.TaskRecord) {
	return NewAppTaskInStoreWithSupervisorConfig(t, store, id, sealed, prior, "/tmp/app-pueue.yml")
}

func NewAppTaskInStoreWithSupervisorConfig(t *testing.T, store *taskdir.Store, id string, sealed bool, prior *task.PriorSession, supervisorConfig string) (*taskdir.TaskDir, *task.TaskRecord) {
	t.Helper()
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	brief := []byte("brief")
	paths := appFixturePaths{provider: "/tmp/app-provider", client: "/tmp/app-pueue", endpoint: "unix:/tmp/app-pueue.sock", config: supervisorConfig}
	req, meta := appFixtureRecords(store.RootID, id, cwd, brief, prior, paths)
	td, err := store.CreateTask(id, req, brief, meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if sealed {
		permit, permitErr := td.PrepareStart(0)
		if permitErr != nil {
			t.Fatal(permitErr)
		}
		t.Cleanup(func() {
			if releaseErr := permit.Release(); releaseErr != nil {
				t.Error(releaseErr)
			}
		})
		if err = permit.Consume(); err != nil {
			t.Fatal(err)
		}
		if err = td.RecordStarted(0); err != nil {
			t.Fatal(err)
		}
		if err = td.WriteRawFiles("answer\n", ""); err != nil {
			t.Fatal(err)
		}
		if _, err = td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef()); err != nil {
			t.Fatal(err)
		}
		if err = permit.Release(); err != nil {
			t.Fatal(err)
		}
	}
	return td, req
}

// appFixturePaths names the inert executable and supervisor locations a
// fixture task records. They are never launched or created.
type appFixturePaths struct {
	provider string
	client   string
	endpoint string
	config   string
}

// appFixtureRecords derives the one valid fixture request/binding pair for
// brief, so every fixture constructor shares one source for record data.
func appFixtureRecords(rootID, id, cwd string, brief []byte, prior *task.PriorSession, paths appFixturePaths) (*task.TaskRecord, *task.MetaRecord) {
	requested := task.TaskConfig{Permission: "read-only", Budget: "1m0s"}
	digest := task.ComputeSHA256([]byte("app-test"))
	req := &task.TaskRecord{SchemaVersion: task.SchemaVersion, RootID: rootID, TaskID: id, Provider: "fixture:test", Mode: "read-only", CanonicalCwd: cwd, RequestedConfig: requested, BudgetNanos: int64(time.Minute), PriorSession: prior, BriefSHA256: task.ComputeSHA256(brief), BriefLength: int64(len(brief))}
	meta := &task.MetaRecord{SchemaVersion: task.SchemaVersion, RootID: rootID, TaskID: id, RequestedConfig: requested, EffectiveConfig: task.EffectiveConfig{Containment: "fixture-only", Approval: "never", Digest: digest}, Containment: "fixture-only", Approval: "never", ProviderExecutable: paths.provider, ProviderVersion: "fixture-v2", PublisherBuild: "app-test", PublisherVersion: "app-test", Predicate: task.FixturePredicateRef(), SupervisorConfig: task.SupervisorRef{ClientExecutable: paths.client, ClientSHA256: digest, ResolvedConfigSHA256: digest, ConfigPath: paths.config, ConfigDigest: digest, Endpoint: paths.endpoint, ObservedVersion: pueue.FixtureVersion}, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	return req, meta
}

func CloseAppTestTask(t *testing.T, store *taskdir.Store, td *taskdir.TaskDir) {
	t.Helper()
	if err := td.Close(); err != nil {
		t.Error(err)
	}
	if err := store.Close(); err != nil {
		t.Error(err)
	}
}

func PrepareAppBudgetRecords(t *testing.T, td *taskdir.TaskDir) *task.MetaRecord {
	t.Helper()
	_, meta, err := td.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	submitPermit, err := td.PrepareSubmission(meta.SupervisorConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := submitPermit.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	if err = submitPermit.Consume(); err != nil {
		t.Fatal(err)
	}
	if err = submitPermit.Release(); err != nil {
		t.Fatal(err)
	}
	submit, err := td.ReadSubmission()
	if err != nil {
		t.Fatal(err)
	}
	ref := submit.Supervisor
	receipt := task.SupervisorReceipt{
		SchemaVersion:        task.SchemaVersion,
		RootID:               submit.RootID,
		TaskID:               submit.TaskID,
		SpecSHA256:           submit.SpecSHA256,
		MetaSHA256:           submit.MetaSHA256,
		NumericTaskID:        7,
		Label:                submit.Label,
		ConfigPath:           ref.ConfigPath,
		ConfigDigest:         ref.ConfigDigest,
		Endpoint:             ref.Endpoint,
		ObservedVersion:      ref.ObservedVersion,
		ClientExecutable:     ref.ClientExecutable,
		ClientSHA256:         ref.ClientSHA256,
		ResolvedConfigSHA256: ref.ResolvedConfigSHA256,
	}
	if err = td.RecordSupervisorReceipt(receipt); err != nil {
		t.Fatal(err)
	}
	start, err := td.PrepareStart(0)
	if err != nil {
		t.Fatal(err)
	}
	if err = start.Release(); err != nil {
		t.Fatal(err)
	}
	return meta
}
