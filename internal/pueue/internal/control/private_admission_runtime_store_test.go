package control

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	goRuntime "runtime"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

type privateAdmissionRuntimeFixture struct {
	root, base string
	runtime    task.SupervisorRef
}

func privateAdmissionRuntimeBinding(root string) task.SupervisorRef {
	binding := validBindingForTest()
	binding.ConfigPath = PrivateConfigPath(root)
	binding.Endpoint = "unix:" + filepath.Join(root, privateSupervisorDirectory, "run", "pueue.sock")
	binding.DaemonExecutable = "/fixture/pueued"
	binding.ExecutableIdentity = task.ExecutableIdentityInstalledPathV1
	binding.ClientSHA256, binding.DaemonSHA256 = "", ""
	return binding
}

// Canonical Store, bootstrap lock and typed historical records only: this
// fixture never constructs a process, private client or native Start grant.
func newPrivateAdmissionRuntimeFixture(t *testing.T) privateAdmissionRuntimeFixture {
	t.Helper()
	store := repairEditorTestStore(t)
	if err := WithBootstrapLockContext(t.Context(), store.Root, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	f := privateAdmissionRuntimeFixture{root: store.Root, base: filepath.Join(store.Root, privateSupervisorDirectory), runtime: privateAdmissionRuntimeBinding(store.Root)}
	writePrivateAdmissionRuntimeRecord(t, privateSupervisorBindingPath(f.root), privateSupervisorBindingRecord{SchemaVersion: task.SchemaVersion, Binding: f.runtime})
	writePrivateAdmissionRuntimeRecord(t, privateDaemonIdentityPath(f.base), privateDaemonIdentityForBinding(f.runtime))
	if binding, found, err := ReadPrivateSupervisorBinding(f.root); err != nil || !found || binding != f.runtime {
		t.Fatalf("canonical fixture publication: found=%t err=%v", found, err)
	}
	return f
}

func writePrivateAdmissionRuntimeRecord[T privateSupervisorBindingRecord | privateDaemonIdentity | privateBootstrapRecord | privateIdentityPublicationRecord | privateRecoveryBindingRecord](t *testing.T, path string, record T) {
	t.Helper()
	data, err := task.MarshalCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	writePrivateSuccessorTestBytes(t, path, data)
}

func TestCheckPrivateAdmissionRuntimeCanonicalStoreDecisionsAreReadOnly(t *testing.T) {
	for _, name := range []string{"exact", "different client", "alias", "alias read error", "migration error", "daemon path", "logical identity", "unsettled", "missing publication", "missing identity"} {
		t.Run(name, func(t *testing.T) {
			f := newPrivateAdmissionRuntimeFixture(t)
			selected, refused := preparePrivateAdmissionRuntimeDecision(t, f, name)
			before := snapshotSavedProbeTestState(t, f.root)
			err := CheckPrivateAdmissionRuntime(t.Context(), f.root, selected)
			if refused {
				if !errors.Is(err, ErrPrivateSupervisorSubmissionIncomplete) {
					t.Fatalf("known mismatch did not refuse: %v", err)
				}
			} else if err != nil {
				t.Fatalf("preflight lost supported pass-through: %v", err)
			}
			if !maps.Equal(before, snapshotSavedProbeTestState(t, f.root)) {
				t.Fatal("preflight changed canonical state")
			}
		})
	}
}

func preparePrivateAdmissionRuntimeDecision(t *testing.T, f privateAdmissionRuntimeFixture, name string) (task.SupervisorRef, bool) {
	t.Helper()
	runtime := f.runtime
	switch name {
	case "different client":
		runtime.ClientExecutable = "/other/pueue"
	case "alias":
		return privateAdmissionRuntimeAlias(t, f), false
	case "alias read error":
		writePrivateSuccessorTestBytes(t, privateRecoveryRecordPath(f.root), []byte("malformed alias owned by retirement"))
	case "migration error":
		writePrivateSuccessorTestBytes(t, privateMigrationRecordPath(f.base), []byte("malformed migration owned by recovery"))
	case "daemon path":
		runtime.DaemonExecutable = "/other/pueued"
		return runtime, false
	case "logical identity":
		runtime.ConfigDigest = task.ComputeSHA256([]byte("other config"))
		return runtime, true
	case "unsettled":
		writePrivateAdmissionRuntimeRecord(t, privateBootstrapRecordPath(f.root), privateBootstrapRecord{SchemaVersion: task.SchemaVersion})
	case "missing publication":
		removePrivateTestPath(t, privateSupervisorBindingPath(f.root))
	case "missing identity":
		removePrivateTestPath(t, privateDaemonIdentityPath(f.base))
	}
	return runtime, false
}

func privateAdmissionRuntimeAlias(t *testing.T, f privateAdmissionRuntimeFixture) task.SupervisorRef {
	t.Helper()
	data := []byte(privateConfigYAML(f.base))
	resolution := ResolutionContext{OS: goRuntime.GOOS, Home: filepath.Dir(f.root), Username: "fixture"}
	resolved, err := resolvePrivateConfigSource(data, f.base, resolution)
	if err != nil {
		t.Fatal(err)
	}
	binding := f.runtime
	binding.ConfigDigest = task.ComputeSHA256(data)
	binding.ResolvedConfigSHA256, err = resolved.Digest()
	if err != nil {
		t.Fatal(err)
	}
	binding.Endpoint = resolved.Endpoint()
	writePrivateSuccessorTestBytes(t, binding.ConfigPath, data)
	writePrivateSuccessorTestBytes(t, privateRecoveryConfigPath(f.root), data)
	writePrivateAdmissionRuntimeRecord(t, privateSupervisorBindingPath(f.root), privateSupervisorBindingRecord{SchemaVersion: task.SchemaVersion, Binding: binding})
	writePrivateAdmissionRuntimeRecord(t, privateDaemonIdentityPath(f.base), privateDaemonIdentityForBinding(binding))
	writePrivateAdmissionRuntimeRecord(t, privateRecoveryRecordPath(f.root), privateRecoveryBindingRecord{SchemaVersion: task.SchemaVersion, Supervisor: binding, ConfigPath: privateRecoveryConfigPath(f.root), ConfigSHA256: binding.ConfigDigest})
	alias, found, err := readValidatedPrivateRecoveryAlias(f.root)
	if err != nil || !found || !alias.matches(binding) {
		t.Fatalf("canonical same-active alias fixture: found=%t err=%v", found, err)
	}
	return binding
}

func TestCheckPrivateAdmissionRuntimeKeepsRequiredReadFaults(t *testing.T) {
	for _, record := range []string{"publication", "identity", "unsettled identity", "transition", "identity publication"} {
		t.Run(record, func(t *testing.T) {
			f := newPrivateAdmissionRuntimeFixture(t)
			path := map[string]string{"publication": privateSupervisorBindingPath(f.root), "identity": privateDaemonIdentityPath(f.base), "unsettled identity": privateDaemonIdentityPath(f.base), "transition": privateBootstrapRecordPath(f.root), "identity publication": privateIdentityPublicationPath(f.root)}[record]
			if record == "unsettled identity" {
				writePrivateAdmissionRuntimeRecord(t, privateBootstrapRecordPath(f.root), privateBootstrapRecord{SchemaVersion: task.SchemaVersion})
			}
			removePrivateTestPath(t, path)
			if err := os.Symlink(path, path); err != nil {
				t.Fatal(err)
			}
			err := CheckPrivateAdmissionRuntime(t.Context(), f.root, f.runtime)
			if !errors.Is(err, unix.ELOOP) || errors.Is(err, ErrPrivateSupervisorSubmissionIncomplete) {
				t.Fatalf("required read fault became mismatch/pass: %v", err)
			}
		})
	}
}

func TestCheckPrivateAdmissionRuntimeUnsettledCannotMaskIdentityPublicationReadFault(t *testing.T) {
	f := newPrivateAdmissionRuntimeFixture(t)
	writePrivateAdmissionRuntimeRecord(t, privateBootstrapRecordPath(f.root), privateBootstrapRecord{SchemaVersion: task.SchemaVersion})
	path := privateIdentityPublicationPath(f.root)
	if err := os.Symlink(path, path); err != nil {
		t.Fatal(err)
	}
	facts, err := loadPrivateGenerationFacts(f.root)
	if err != nil || facts.transitionErr != nil || facts.settled {
		t.Fatalf("fixture changed legacy transition projection: %v", err)
	}
	if ready, readyErr := facts.publishedReady(f.runtime); ready || readyErr != nil {
		t.Fatalf("legacy predicate ordering changed: ready=%t err=%v", ready, readyErr)
	}
	err = CheckPrivateAdmissionRuntime(t.Context(), f.root, f.runtime)
	if !errors.Is(err, unix.ELOOP) || errors.Is(err, ErrPrivateSupervisorSubmissionIncomplete) {
		t.Fatalf("bootstrap pending hid independent publication IO: %v", err)
	}
}

func TestCheckPrivateAdmissionRuntimeLockAndContextFaultsStayFaults(t *testing.T) {
	f := newPrivateAdmissionRuntimeFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, ctx := range []context.Context{nil, ctx} {
		if err := CheckPrivateAdmissionRuntime(ctx, f.root, f.runtime); !errors.Is(err, context.Canceled) {
			t.Fatalf("invalid context became mismatch/pass: %v", err)
		}
	}
	var held *bootstrapLock
	t.Cleanup(func() {
		if err := closeBootstrapLock(held); err != nil {
			t.Error(err)
		}
	})
	var err error
	held, err = acquireBootstrapLock(t.Context(), filepath.Join(f.base, "bootstrap.lock"))
	if err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	err = CheckPrivateAdmissionRuntime(bounded, f.root, f.runtime)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrPrivateSupervisorSubmissionIncomplete) {
		t.Fatalf("lock contention became mismatch/pass: %v", err)
	}
}

func TestCheckPrivateAdmissionRuntimeMissingLockIsNotPositiveMismatch(t *testing.T) {
	f := newPrivateAdmissionRuntimeFixture(t)
	removePrivateTestPath(t, filepath.Join(f.base, "bootstrap.lock"))
	err := CheckPrivateAdmissionRuntime(t.Context(), f.root, f.runtime)
	if !errors.Is(err, ErrPrivateSupervisorNotInitialized) || errors.Is(err, ErrPrivateSupervisorSubmissionIncomplete) {
		t.Fatalf("missing generation lock became pass/mismatch: %v", err)
	}
}
