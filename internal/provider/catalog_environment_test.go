package provider

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// catalogTestEnvironment returns a fresh explicit launch snapshot for catalog
// preparations whose provider factory does not depend on its contents.
func catalogTestEnvironment() []string {
	return []string{"PATH=/catalog-test-bin", "LANG=C"}
}

func TestCatalogPreparationRejectsUncapturedEnvironment(t *testing.T) {
	called := false
	registration := testRegistration("alpha:print", "read-only")
	registration.Prepare = func(task.TaskRecord, []string) (ProfileCandidate, error) {
		called = true
		return ProfileCandidate{}, errors.New("factory must not run")
	}
	registration.PrepareExisting = func(task.TaskRecord, task.MetaRecord, []string) (ProfileCandidate, error) {
		called = true
		return ProfileCandidate{}, errors.New("factory must not run")
	}
	catalog, err := NewCatalog(registration)
	if err != nil {
		t.Fatal(err)
	}
	request := catalogPreparationRequest()
	operations := map[string]func() error{
		"fresh": func() error { _, callErr := catalog.Candidate(request, nil); return callErr },
		"existing": func() error {
			_, callErr := catalog.ExistingCandidate(request, task.MetaRecord{}, nil)
			return callErr
		},
		"successor": func() error {
			_, callErr := catalog.SuccessorCandidate(request, task.MetaRecord{}, nil)
			return callErr
		},
		"prepare": func() error { _, callErr := catalog.Prepare(request, nil); return callErr },
	}
	for name, operation := range operations {
		if err = operation(); !errors.Is(err, task.ErrEvidenceFault) {
			t.Fatalf("%s accepted an uncaptured environment: %v", name, err)
		}
	}
	if called {
		t.Fatal("an uncaptured environment reached the provider factory")
	}
}

func TestCatalogPreparationReceivesIndependentEnvironmentCopy(t *testing.T) {
	tests := []struct {
		name        string
		environment []string
	}{
		{name: "values", environment: []string{"PATH=/selected-bin", "HOME=/selected-home"}},
		{name: "explicit empty", environment: []string{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertCatalogPassesEnvironmentCopy(t, test.environment)
		})
	}
}

// assertCatalogPassesEnvironmentCopy proves one fresh preparation receives a
// non-nil copy of the caller's snapshot that it cannot alias.
func assertCatalogPassesEnvironmentCopy(t *testing.T, environment []string) {
	t.Helper()
	var received []string
	registration := testRegistration("alpha:print", "read-only")
	fresh := (&catalogPreparationProbe{}).fresh("fresh")
	registration.Prepare = func(request task.TaskRecord, prepared []string) (ProfileCandidate, error) {
		received = slices.Clone(prepared)
		if prepared == nil {
			return ProfileCandidate{}, errors.New("explicit environment collapsed to nil")
		}
		if len(prepared) > 0 {
			prepared[0] = "PATH=/provider-mutation"
		}
		return fresh(request, prepared)
	}
	catalog, err := NewCatalog(registration)
	if err != nil {
		t.Fatal(err)
	}
	caller := slices.Clone(environment)
	if _, err = catalog.Candidate(catalogPreparationRequest(), caller); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(received, environment) || received == nil {
		t.Fatalf("provider received %q, want %q", received, environment)
	}
	if !slices.Equal(caller, environment) {
		t.Fatalf("provider preparation changed the caller snapshot to %q", caller)
	}
}

func TestCatalogConcurrentPreparationsResolveTheirOwnExecutables(t *testing.T) {
	first := writeCatalogTestExecutable(t, "first")
	second := writeCatalogTestExecutable(t, "second")
	missing := t.TempDir()
	ambient := os.Environ()
	registration := testRegistration("alpha:print", "read-only")
	registration.Prepare = func(request task.TaskRecord, environment []string) (ProfileCandidate, error) {
		info, err := LocateCLI("catalog-cli", environment)
		if err != nil {
			return ProfileCandidate{}, err
		}
		return ReadyCandidate(func(request task.TaskRecord) (PreparedProfile, error) {
			plan := executionPlanForCatalogTest(request)
			plan.Executable = info.Path
			return catalogTestProfile(plan), nil
		})(request, environment)
	}
	catalog, err := NewCatalog(registration)
	if err != nil {
		t.Fatal(err)
	}
	environments := [][]string{
		{"PATH=" + filepath.Dir(first)},
		{"PATH=" + filepath.Dir(second)},
		{"PATH=" + missing},
	}
	executables := make([]string, len(environments))
	errs := make([]error, len(environments))
	start := make(chan struct{})
	var wait sync.WaitGroup
	for index, environment := range environments {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			candidate, candidateErr := catalog.Candidate(catalogPreparationRequest(), environment)
			if candidateErr != nil {
				errs[index] = candidateErr
				return
			}
			profile, finalizeErr := candidate.Finalize(nil, time.Unix(0, 0))
			executables[index], errs[index] = profile.Plan.Executable, finalizeErr
		}()
	}
	close(start)
	wait.Wait()
	if errs[0] != nil || errs[1] != nil || executables[0] != first || executables[1] != second {
		t.Fatalf("concurrent preparations resolved %q with errors %v", executables[:2], errs[:2])
	}
	if !errors.Is(errs[2], ErrProfileUnavailable) || !errors.Is(errs[2], exec.ErrNotFound) {
		t.Fatalf("missing executable preparation = %v", errs[2])
	}
	if !slices.Equal(os.Environ(), ambient) {
		t.Fatal("explicit preparation changed the process environment")
	}
}

func writeCatalogTestExecutable(t *testing.T, contents string) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "catalog-cli")
	if err = os.WriteFile(path, []byte("#!/bin/sh\n# "+contents+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
