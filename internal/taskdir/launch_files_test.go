package taskdir

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func launchTestTask(t *testing.T, s *Store, inputs []task.InputFile, outputs []task.OutputArtifact) *TaskDir {
	t.Helper()
	req, meta, brief := preparedInput(t, s)
	meta.InputFiles = inputs
	meta.OutputArtifacts = outputs
	if len(outputs) > 0 {
		meta.OutputWriterContract = task.OutputWriterProcessExitEOF
	}
	td, err := s.CreateTask(req.TaskID, req, brief, meta)
	must(t, err)
	t.Cleanup(func() { must(t, td.Close()) })
	return td
}

func launchOutputTask(t *testing.T, s *Store) *TaskDir {
	t.Helper()
	return launchTestTask(t, s, nil, []task.OutputArtifact{{Name: "artifact.bin", ArgumentIndex: 1}})
}

func writeNativeOutput(t *testing.T, path string, content []byte) {
	t.Helper()
	must(t, os.WriteFile(path, content, 0o600))
	info, err := os.Lstat(path)
	must(t, err)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("native output has unsafe mode or type: mode=%s", info.Mode())
	}
}

func startAndPrepareLaunch(t *testing.T, td *TaskDir, arguments []string) (*StartPermit, []string) {
	t.Helper()
	p := consumeStart(t, td)
	prepared, err := td.PrepareLaunchFiles(arguments)
	must(t, err)
	return p, prepared
}

func sealPreparedOutput(t *testing.T, td *TaskDir, path string, present bool) *task.ProviderExitRecord {
	t.Helper()
	if present {
		writeNativeOutput(t, path, nil)
	}
	must(t, td.WriteRawFiles("answer", ""))
	must(t, td.ImportOutputArtifacts())
	seal, err := td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef())
	must(t, err)
	return seal
}

func TestPrepareLaunchFilesRequiresConsumedPermitAndIsSingleUse(t *testing.T) {
	s := testStore(t)
	td := launchTestTask(t, s, nil, nil)
	arguments := []string{"provider"}

	if _, err := td.PrepareLaunchFiles(arguments); !errors.Is(err, task.ErrInvalidPermit) {
		t.Fatalf("preparation before start returned %v", err)
	}
	p, err := td.PrepareStart(0)
	must(t, err)
	defer func() { must(t, p.Release()) }()
	if _, err = td.PrepareLaunchFiles(arguments); !errors.Is(err, task.ErrInvalidPermit) {
		t.Fatalf("preparation before permit consumption returned %v", err)
	}
	must(t, p.Consume())
	prepared, err := td.PrepareLaunchFiles(arguments)
	must(t, err)
	if len(prepared) != len(arguments) || prepared[0] != arguments[0] {
		t.Fatalf("unexpected prepared arguments: %v", prepared)
	}
	if _, err = td.PrepareLaunchFiles(arguments); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("second preparation returned %v", err)
	}
}

func TestPrepareLaunchFilesRejectsPreexistingStagingDirectory(t *testing.T) {
	tests := []struct {
		name    string
		inputs  []task.InputFile
		outputs []task.OutputArtifact
		dir     string
	}{
		{
			name: "input",
			inputs: []task.InputFile{{
				Name: "config.json", ArgumentIndex: 1, Content: "{\"mode\":\"test\"}\n",
			}},
			dir: "provider-input",
		},
		{
			name:    "output",
			outputs: []task.OutputArtifact{{Name: "artifact.bin", ArgumentIndex: 1}},
			dir:     "provider-output",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := testStore(t)
			td := launchTestTask(t, s, test.inputs, test.outputs)
			p, err := td.PrepareStart(0)
			must(t, err)
			defer func() { must(t, p.Release()) }()
			must(t, p.Consume())
			must(t, os.Mkdir(filepath.Join(td.Dir, test.dir), 0o700))

			arguments := []string{"provider", ""}
			prepared, err := td.PrepareLaunchFiles(arguments)
			if prepared != nil || !errors.Is(err, os.ErrExist) {
				t.Fatalf("preexisting %s was reused: prepared=%v err=%v", test.dir, prepared, err)
			}
		})
	}
}

func TestPrepareLaunchFilesBindsImmutableInputContent(t *testing.T) {
	s := testStore(t)
	inputs := []task.InputFile{{Name: "config.json", ArgumentIndex: 1, Content: "{\"immutable\":true}\n"}}
	td := launchTestTask(t, s, inputs, nil)
	inputs[0].Content = "{\"mutated\":true}\n"

	p, arguments := startAndPrepareLaunch(t, td, []string{"provider", ""})
	defer func() { must(t, p.Release()) }()
	if arguments[1] != filepath.Join(td.Dir, "provider-input", "config.json") {
		t.Fatalf("input argument was not bound to task path: %v", arguments)
	}
	if arguments[1] == "" {
		t.Fatal("input argument remained reserved")
	}
	if got := readTestFile(t, arguments[1]); !bytes.Equal(got, []byte("{\"immutable\":true}\n")) {
		t.Fatalf("input bytes changed after caller mutation: %q", got)
	}
	info, err := os.Stat(arguments[1])
	must(t, err)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("input mode = %o, want 0600", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(filepath.Join(td.Dir, "provider-input"))
	must(t, err)
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("input directory mode = %o, want 0700", dirInfo.Mode().Perm())
	}
	_, meta, err := td.PreparedRecords()
	must(t, err)
	if len(meta.InputFiles) != 1 || meta.InputFiles[0].Content != "{\"immutable\":true}\n" {
		t.Fatalf("persisted input metadata changed: %+v", meta.InputFiles)
	}
}

func TestNamedEvidenceDistinguishesAbsentFromPresentEmpty(t *testing.T) {
	for _, present := range []bool{false, true} {
		name := "absent"
		if present {
			name = "present-empty"
		}
		t.Run(name, func(t *testing.T) {
			s := testStore(t)
			td := launchOutputTask(t, s)
			p, arguments := startAndPrepareLaunch(t, td, []string{"provider", ""})
			defer func() { must(t, p.Release()) }()
			seal := sealPreparedOutput(t, td, arguments[1], present)

			evidence := mustOpenEvidence(t, td, seal)
			var named predicate.NamedEvidence = evidence
			called := false
			var content []byte
			err := named.ReadNamed("artifact.bin", func(reader io.Reader) error {
				called = true
				var err error
				content, err = io.ReadAll(reader)
				return err
			})
			if present {
				if err != nil || !called || len(content) != 0 {
					t.Fatalf("present empty evidence read: called=%v bytes=%q err=%v", called, content, err)
				}
			} else if !errors.Is(err, predicate.ErrEvidenceAbsent) || called {
				t.Fatalf("absent optional evidence: called=%v err=%v", called, err)
			}
			must(t, evidence.Close())
		})
	}
}

func TestNamedEvidenceRejectsUndeclaredNameAndPoisonsClose(t *testing.T) {
	s := testStore(t)
	td := launchOutputTask(t, s)
	p, arguments := startAndPrepareLaunch(t, td, []string{"provider", ""})
	defer func() { must(t, p.Release()) }()
	seal := sealPreparedOutput(t, td, arguments[1], false)
	evidence := mustOpenEvidence(t, td, seal)
	var named predicate.NamedEvidence = evidence
	called := false
	err := named.ReadNamed("undeclared.bin", func(io.Reader) error {
		called = true
		return nil
	})
	if called || !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("undeclared named evidence was accepted: called=%v err=%v", called, err)
	}
	if closeErr := evidence.Close(); !errors.Is(closeErr, task.ErrEvidenceFault) {
		t.Fatalf("undeclared read did not poison evidence close: %v", closeErr)
	}
}

func mustOpenEvidence(t *testing.T, td *TaskDir, seal *task.ProviderExitRecord) *sealedEvidence {
	t.Helper()
	evidence, err := td.openValidatedEvidence(seal)
	must(t, err)
	return evidence
}

func TestImportOutputArtifactsCopiesBytesToDistinctRawInode(t *testing.T) {
	s := testStore(t)
	td := launchOutputTask(t, s)
	p, arguments := startAndPrepareLaunch(t, td, []string{"provider", ""})
	defer func() { must(t, p.Release()) }()
	content := []byte("same output bytes")
	writeNativeOutput(t, arguments[1], content)
	nativeInfo, err := os.Stat(arguments[1])
	must(t, err)
	seal := sealPreparedOutput(t, td, arguments[1], false)
	rawPath := filepath.Join(td.Dir, "raw", "artifact.bin")
	rawInfo, err := os.Stat(rawPath)
	must(t, err)
	if os.SameFile(nativeInfo, rawInfo) {
		t.Fatal("raw evidence reused the native output inode")
	}
	if got := readTestFile(t, rawPath); !bytes.Equal(got, content) {
		t.Fatalf("raw output bytes = %q, want %q", got, content)
	}
	if rawInfo.Mode().Perm() != 0o600 {
		t.Fatalf("raw output mode = %o, want 0600", rawInfo.Mode().Perm())
	}
	if seal == nil {
		t.Fatal("missing provider seal")
	}
}

func TestImportOutputArtifactsRejectsSymlinkAndNonregular(t *testing.T) {
	for _, kind := range []string{"symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			s := testStore(t)
			td := launchOutputTask(t, s)
			p, arguments := startAndPrepareLaunch(t, td, []string{"provider", ""})
			defer func() { must(t, p.Release()) }()
			if kind == "symlink" {
				target := filepath.Join(t.TempDir(), "outside")
				writeNativeOutput(t, target, []byte("outside"))
				must(t, os.Symlink(target, arguments[1]))
			} else {
				must(t, os.Mkdir(arguments[1], 0o700))
			}
			if err := td.ImportOutputArtifacts(); err == nil || !errors.Is(err, task.ErrEvidenceFault) {
				t.Fatalf("unsafe %s was imported: %v", kind, err)
			}
			seal, err := td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef())
			if seal != nil || !errors.Is(err, task.ErrEvidenceFault) {
				t.Fatalf("unsafe %s did not poison seal: seal=%v err=%v", kind, seal, err)
			}
		})
	}
}

func TestSealedDeclaredArtifactRemovalOrCorruptionIsEvidenceFault(t *testing.T) {
	for _, operation := range []string{"remove", "corrupt"} {
		t.Run(operation, func(t *testing.T) {
			s := testStore(t)
			td := launchOutputTask(t, s)
			p, arguments := startAndPrepareLaunch(t, td, []string{"provider", ""})
			defer func() { must(t, p.Release()) }()
			seal := sealPreparedOutput(t, td, arguments[1], true)
			rawPath := filepath.Join(td.Dir, "raw", "artifact.bin")
			if operation == "remove" {
				must(t, os.Remove(rawPath))
			} else {
				must(t, os.WriteFile(rawPath, []byte("corrupted"), 0o600))
			}
			evidence, err := td.openValidatedEvidence(seal)
			if evidence != nil {
				must(t, evidence.Close())
			}
			if err == nil || !errors.Is(err, task.ErrEvidenceFault) {
				t.Fatalf("declared %s artifact was treated as optional absence: evidence=%v err=%v", operation, evidence, err)
			}
		})
	}
}

func TestMissingProviderOutputDirectoryPoisonsImportAndSeal(t *testing.T) {
	s := testStore(t)
	td := launchOutputTask(t, s)
	p, arguments := startAndPrepareLaunch(t, td, []string{"provider", ""})
	defer func() { must(t, p.Release()) }()
	archived := filepath.Join(td.Dir, "provider-output.archived")
	must(t, os.Rename(filepath.Dir(arguments[1]), archived))
	importErr := td.ImportOutputArtifacts()
	if importErr == nil || !errors.Is(importErr, task.ErrEvidenceFault) {
		t.Fatalf("missing provider-output directory was treated as optional absence: %v", importErr)
	}
	seal, sealErr := td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef())
	if seal != nil || !errors.Is(sealErr, task.ErrEvidenceFault) {
		t.Fatalf("missing provider-output directory did not poison seal: seal=%v err=%v", seal, sealErr)
	}
}

func TestImportOutputArtifactsStageFailuresPoisonSeal(t *testing.T) {
	faults := []struct {
		name string
		make func() *testFaultInjector
	}{
		{name: "write", make: func() *testFaultInjector { return &testFaultInjector{failStageWrite: true} }},
		{name: "barrier", make: func() *testFaultInjector { return &testFaultInjector{failStageBarrier: true} }},
		{name: "close", make: func() *testFaultInjector { return &testFaultInjector{failStageClose: true} }},
	}
	for _, test := range faults {
		t.Run(test.name, func(t *testing.T) {
			s := testStore(t)
			td := launchOutputTask(t, s)
			p, arguments := startAndPrepareLaunch(t, td, []string{"provider", ""})
			defer func() { must(t, p.Release()) }()
			writeNativeOutput(t, arguments[1], []byte("faulted output"))
			must(t, td.WriteRawFiles("answer", ""))
			s.SetFaultInjector(test.make())
			importErr := td.ImportOutputArtifacts()
			s.SetFaultInjector(nil)
			if importErr == nil || !errors.Is(importErr, task.ErrEvidenceFault) {
				t.Fatalf("stage %s failure was hidden: %v", test.name, importErr)
			}
			seal, sealErr := td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef())
			if seal != nil || !errors.Is(sealErr, task.ErrEvidenceFault) {
				t.Fatalf("stage %s failure did not poison seal: seal=%v err=%v", test.name, seal, sealErr)
			}
			testAbsent(t, filepath.Join(td.Dir, "raw", "artifact.bin"))
		})
	}
}

type outputMutationFault struct {
	BaseFaultInjector
	name        string
	nativePath  string
	kind        string
	replacement []byte
	triggered   bool
}

func (f *outputMutationFault) OnStageWrite(destination string, data []byte) (int, error) {
	if f.triggered || filepath.Base(destination) != f.name {
		return len(data), nil
	}
	var err error
	switch f.kind {
	case "mutate":
		err = os.WriteFile(f.nativePath, f.replacement, 0o600)
	case "replace":
		temporary := f.nativePath + ".replacement"
		if err = os.WriteFile(temporary, f.replacement, 0o600); err == nil {
			err = os.Rename(temporary, f.nativePath)
		}
	default:
		err = errors.New("unknown output mutation kind")
	}
	if err != nil {
		return 0, err
	}
	f.triggered = true
	return len(data), nil
}

func TestImportOutputArtifactsDetectsNativeMutationAndReplacement(t *testing.T) {
	content := bytes.Repeat([]byte("n"), 64*1024)
	for _, kind := range []string{"mutate", "replace"} {
		t.Run(kind, func(t *testing.T) {
			s := testStore(t)
			td := launchOutputTask(t, s)
			p, arguments := startAndPrepareLaunch(t, td, []string{"provider", ""})
			defer func() { must(t, p.Release()) }()
			writeNativeOutput(t, arguments[1], content)
			must(t, td.WriteRawFiles("answer", ""))
			replacement := append([]byte(nil), content...)
			if kind == "mutate" {
				replacement = bytes.Repeat([]byte("m"), len(content))
			}
			fault := &outputMutationFault{name: "artifact.bin", nativePath: arguments[1], kind: kind, replacement: replacement}
			s.SetFaultInjector(fault)
			importErr := td.ImportOutputArtifacts()
			s.SetFaultInjector(nil)
			if !fault.triggered {
				t.Fatal("mutation fault did not run during import")
			}
			if importErr == nil || !errors.Is(importErr, task.ErrEvidenceFault) {
				t.Fatalf("native %s was accepted: %v", kind, importErr)
			}
			seal, sealErr := td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef())
			if seal != nil || !errors.Is(sealErr, task.ErrEvidenceFault) {
				t.Fatalf("native %s failure did not poison seal: seal=%v err=%v", kind, seal, sealErr)
			}
		})
	}
}

type sealDuringImportFault struct {
	BaseFaultInjector
	td        *TaskDir
	attempted bool
	sealErr   error
}

func (f *sealDuringImportFault) OnStageWrite(_ string, data []byte) (int, error) {
	if !f.attempted {
		f.attempted = true
		_, f.sealErr = f.td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef())
	}
	return len(data), nil
}

func TestImportOutputArtifactsFencesSealUntilImportCompletes(t *testing.T) {
	s := testStore(t)
	td := launchOutputTask(t, s)
	p, arguments := startAndPrepareLaunch(t, td, []string{"provider", ""})
	defer func() { must(t, p.Release()) }()
	writeNativeOutput(t, arguments[1], []byte("fenced output"))
	must(t, td.WriteRawFiles("answer", ""))
	fault := &sealDuringImportFault{td: td}
	s.SetFaultInjector(fault)
	must(t, td.ImportOutputArtifacts())
	s.SetFaultInjector(nil)
	if !fault.attempted || !errors.Is(fault.sealErr, task.ErrEvidenceFault) {
		t.Fatalf("seal was not fenced during import: attempted=%v err=%v", fault.attempted, fault.sealErr)
	}
	seal, err := td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef())
	must(t, err)
	if seal == nil {
		t.Fatal("successful import could not be sealed")
	}
}

func TestSealedOutputReplayNeedsNoNativeStaging(t *testing.T) {
	s := testStore(t)
	td := launchOutputTask(t, s)
	p, arguments := startAndPrepareLaunch(t, td, []string{"provider", ""})
	writeNativeOutput(t, arguments[1], []byte("replay output"))
	sealPreparedOutput(t, td, arguments[1], false)
	out, cleanupErr, err := td.Finalize(task.FixturePredicateRef())
	must(t, cleanupErr)
	must(t, err)
	must(t, p.Release())

	archived := filepath.Join(td.Dir, "provider-output.archived")
	must(t, os.Rename(filepath.Join(td.Dir, "provider-output"), archived))
	_, scavengeErr := s.Scavenge()
	must(t, scavengeErr)
	s.SetFaultInjector(&testFaultInjector{failStageWrite: true})
	replayed, replayCleanup, replayErr := td.Collect(task.FixturePredicateRef())
	s.SetFaultInjector(nil)
	must(t, replayCleanup)
	must(t, replayErr)
	if !task.CompareOutcomes(out, replayed) {
		t.Fatalf("replay changed sealed outcome: original=%+v replay=%+v", out, replayed)
	}
}
