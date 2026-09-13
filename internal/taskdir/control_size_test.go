package taskdir

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func oversizedControlInputs() map[string]string {
	return map[string]string{
		"plain":        strings.Repeat("x", task.MaxControlRecordSize),
		"json-escaped": strings.Repeat("\"", task.MaxControlRecordSize/2),
	}
}

func TestOversizedSealNeverCommits(t *testing.T) {
	for name, text := range oversizedControlInputs() {
		t.Run(name, func(t *testing.T) {
			s := testStore(t)
			td := preparedTask(t, s)
			permit := consumeStart(t, td)
			defer func() { must(t, permit.Release()) }()
			must(t, td.WriteRawFiles("answer", ""))
			seal, err := td.Seal(task.InvocationStarted, 0, text, task.FixturePredicateRef())
			if seal != nil || !errors.Is(err, task.ErrControlRecordTooBig) {
				t.Fatalf("oversized seal committed: seal=%t err=%v", seal != nil, err)
			}
			testAbsent(t, filepath.Join(td.Dir, "provider.exit"))
			seal, err = td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef())
			must(t, err)
			if seal == nil {
				t.Fatal("bounded seal retry failed")
			}
		})
	}
}

func TestOversizedPreparedControlNeverCommits(t *testing.T) {
	for _, record := range []string{"task", "meta"} {
		for name, text := range oversizedControlInputs() {
			t.Run(record+"/"+name, func(t *testing.T) {
				s := testStore(t)
				req, meta, brief := preparedInput(t, s)
				if record == "task" {
					req.RequestedConfig.Model = text
					meta.RequestedConfig = req.RequestedConfig
				} else {
					meta.ProviderVersion = text
				}
				td, err := s.CreateTask(req.TaskID, req, brief, meta)
				if td != nil {
					must(t, td.Close())
				}
				if td != nil || !errors.Is(err, task.ErrControlRecordTooBig) {
					t.Fatalf("oversized prepared control accepted: handle=%t err=%v", td != nil, err)
				}
				testAbsent(t, filepath.Join(s.Root, "tasks", req.TaskID))
			})
		}
	}
}

func TestOversizedSessionClaimNeverCommits(t *testing.T) {
	for name, conversation := range oversizedControlInputs() {
		t.Run(name, func(t *testing.T) {
			s := testStore(t)
			td := preparedTask(t, s)
			err := td.ClaimSession("fixture:test", conversation)
			if !errors.Is(err, task.ErrControlRecordTooBig) {
				t.Fatalf("oversized claim accepted: %v", err)
			}
			testAbsent(t, filepath.Join(td.sessionDirectory("fixture:test", conversation), td.TaskID+".claim.json"))
		})
	}
}

func TestOversizedSessionReleaseNeverCommits(t *testing.T) {
	s := testStore(t)
	td := preparedTask(t, s)
	empty := task.SessionClaimRecord{SchemaVersion: task.SchemaVersion, RootID: s.RootID, TaskID: td.TaskID, Provider: "fixture:test", ConversationID: "", ClaimedAt: timestamp()}
	data, err := task.MarshalCanonical(empty)
	must(t, err)
	conversation := strings.Repeat("c", task.MaxControlRecordSize-len(data)-32)
	must(t, td.ClaimSession("fixture:test", conversation))
	claimPath := filepath.Join(td.sessionDirectory("fixture:test", conversation), td.TaskID+".claim.json")
	original := readTestFile(t, claimPath)
	before, err := os.Stat(claimPath)
	must(t, err)
	p := consumeStart(t, td)
	must(t, td.WriteRawFiles("answer", ""))
	seal, err := td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef())
	must(t, err)
	must(t, p.Release())
	err = td.ReleaseSession("fixture:test", conversation, seal.ManifestSHA256)
	if !errors.Is(err, task.ErrControlRecordTooBig) {
		t.Fatalf("oversized release accepted: %v", err)
	}
	testAbsent(t, filepath.Join(td.sessionDirectory("fixture:test", conversation), td.TaskID+".release.json"))
	after, err := os.Stat(claimPath)
	must(t, err)
	if !os.SameFile(before, after) || !bytes.Equal(original, readTestFile(t, claimPath)) {
		t.Fatal("rejected release changed claim")
	}
}

func TestOversizedOutcomeDoesNotStagePayload(t *testing.T) {
	s := testStore(t)
	td := sealedTask(t, s, "answer")
	_, _, meta, spec, metaHash, err := td.loadAndValidatePreparedSet()
	must(t, err)
	seal, err := td.readSeal(meta.Predicate, spec, metaHash)
	must(t, err)
	predicate := task.FixturePredicateRef()
	predicate.Version = strings.Repeat("v", task.MaxControlRecordSize)
	content := []byte("answer")
	candidate := &task.OutcomeRecord{SchemaVersion: task.SchemaVersion, RootID: s.RootID, TaskID: td.TaskID, SpecSHA256: spec, MetaSHA256: metaHash, EvidenceSHA256: seal.ManifestSHA256, Predicate: predicate, Verdict: task.VerdictCommitted, Payload: task.PayloadDescriptor{Basename: "result.txt", Length: int64(len(content)), SHA256: task.ComputeSHA256(content)}}
	result, cleanup, err := td.publishCandidate(candidate, bytes.NewReader(content))
	must(t, cleanup)
	if result != nil || !errors.Is(err, task.ErrControlRecordTooBig) {
		t.Fatalf("oversized outcome accepted: result=%t err=%v", result != nil, err)
	}
	testAbsent(t, filepath.Join(td.Dir, "result.txt"))
	testAbsent(t, filepath.Join(td.Dir, "outcome.json"))
}

func TestControlRecordSerializedBoundary(t *testing.T) {
	for _, escaped := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "json-escaped"}[escaped], func(t *testing.T) {
			record := map[string]string{"value": ""}
			base, err := task.MarshalCanonical(record)
			must(t, err)
			available := task.MaxControlRecordSize - len(base)
			if escaped {
				record["value"] = strings.Repeat("\"", available/2) + strings.Repeat("x", available%2)
			} else {
				record["value"] = strings.Repeat("x", available)
			}
			data, err := marshalControlRecord(record)
			must(t, err)
			if len(data) != task.MaxControlRecordSize {
				t.Fatalf("incorrect exact boundary: %d", len(data))
			}
			read, err := task.ReadControlRecord(bytes.NewReader(data))
			must(t, err)
			if !bytes.Equal(data, read) {
				t.Fatal("writer and reader bounds differ")
			}
			record["value"] += "x"
			data, err = marshalControlRecord(record)
			if data != nil || !errors.Is(err, task.ErrControlRecordTooBig) {
				t.Fatalf("one byte over limit accepted: length=%d err=%v", len(data), err)
			}
		})
	}
}

func TestControlCeilingDoesNotLimitBrief(t *testing.T) {
	s := testStore(t)
	req, meta, _ := preparedInput(t, s)
	brief := bytes.Repeat([]byte("b"), task.MaxControlRecordSize+128)
	req.BriefLength = int64(len(brief))
	req.BriefSHA256 = task.ComputeSHA256(brief)
	td, err := s.CreateTask(req.TaskID, req, brief, meta)
	must(t, err)
	defer closeTaskQuietly(td)
	if !bytes.Equal(brief, readTestFile(t, filepath.Join(td.Dir, "brief.md"))) {
		t.Fatal("brief was capped by control ceiling")
	}
}

func historicalClaim(td *TaskDir, conversation string) task.SessionClaimRecord {
	return task.SessionClaimRecord{SchemaVersion: task.SchemaVersion, RootID: td.store.RootID, TaskID: td.TaskID, Provider: "fixture:test", ConversationID: conversation, ClaimedAt: "2000-01-01T00:00:00Z"}
}

func stageHistoricalSessionRecord(t *testing.T, td *TaskDir, conversation, name string, record any) string {
	t.Helper()
	data, err := task.MarshalCanonical(record)
	must(t, err)
	if len(data) > task.MaxControlRecordSize {
		t.Fatal("historical fixture exceeded limit")
	}
	must(t, td.withSession("fixture:test", conversation, func(dir string) error {
		_, cleanup, writeErr := td.store.stageAndCommit(dir, name, data, nil)
		return errors.Join(writeErr, cleanup)
	}))
	return filepath.Join(td.sessionDirectory("fixture:test", conversation), name)
}

func TestSessionSizeCheckPreservesExistingClaim(t *testing.T) {
	s := testStore(t)
	td := preparedTask(t, s)
	claim := historicalClaim(td, "")
	base, err := task.MarshalCanonical(claim)
	must(t, err)
	claim.ConversationID = strings.Repeat("c", task.MaxControlRecordSize-len(base))
	path := stageHistoricalSessionRecord(t, td, claim.ConversationID, td.TaskID+".claim.json", claim)
	original := readTestFile(t, path)
	before, err := os.Stat(path)
	must(t, err)
	must(t, td.ClaimSession("fixture:test", claim.ConversationID))
	after, err := os.Stat(path)
	must(t, err)
	if !os.SameFile(before, after) || !bytes.Equal(original, readTestFile(t, path)) {
		t.Fatal("claim retry replaced boundary record")
	}
}

func TestSessionSizeCheckPreservesExistingRelease(t *testing.T) {
	s := testStore(t)
	td := sealedTask(t, s, "answer")
	_, _, meta, spec, metaHash, err := td.loadAndValidatePreparedSet()
	must(t, err)
	seal, err := td.readSeal(meta.Predicate, spec, metaHash)
	must(t, err)
	release := task.SessionReleaseRecord{SchemaVersion: task.SchemaVersion, RootID: s.RootID, TaskID: td.TaskID, Provider: "fixture:test", PredecessorEvidenceSHA256: seal.ManifestSHA256, ReleasedAt: "2000-01-01T00:00:00Z"}
	base, err := task.MarshalCanonical(release)
	must(t, err)
	release.ConversationID = strings.Repeat("c", task.MaxControlRecordSize-len(base))
	stageHistoricalSessionRecord(t, td, release.ConversationID, td.TaskID+".claim.json", historicalClaim(td, release.ConversationID))
	path := stageHistoricalSessionRecord(t, td, release.ConversationID, td.TaskID+".release.json", release)
	original := readTestFile(t, path)
	before, err := os.Stat(path)
	must(t, err)
	must(t, td.ReleaseSession("fixture:test", release.ConversationID, seal.ManifestSHA256))
	after, err := os.Stat(path)
	must(t, err)
	if !os.SameFile(before, after) || !bytes.Equal(original, readTestFile(t, path)) {
		t.Fatal("release retry replaced boundary record")
	}
}

func TestPreparedSizeCheckPreservesExistingMetadata(t *testing.T) {
	s := testStore(t)
	req, meta, brief := preparedInput(t, s)
	reqData, err := task.MarshalCanonical(req)
	must(t, err)
	meta.SpecSHA256 = task.ComputeSHA256(reqData)
	meta.CreatedAt = "2000-01-01T00:00:00Z"
	base, err := task.MarshalCanonical(meta)
	must(t, err)
	meta.ProviderVersion = strings.Repeat("v", task.MaxControlRecordSize-len(base)+len(meta.ProviderVersion))
	td, err := s.CreateTask(req.TaskID, req, brief, meta)
	must(t, err)
	defer closeTaskQuietly(td)
	permit, err := td.PrepareSubmission(meta.SupervisorConfig)
	must(t, err)
	must(t, permit.Release())
	path := filepath.Join(td.Dir, "meta.json")
	original := readTestFile(t, path)
	before, err := os.Stat(path)
	must(t, err)
	if len(original) != task.MaxControlRecordSize {
		t.Fatalf("metadata fixture not at boundary: %d", len(original))
	}
	retryMeta := *meta
	retryMeta.CreatedAt = "2000-01-01T00:00:00.123456789Z"
	retry, err := s.CreateTask(req.TaskID, req, brief, &retryMeta)
	must(t, err)
	defer closeTaskQuietly(retry)
	conflict := *meta
	conflict.ProviderVersion = "w" + meta.ProviderVersion[1:]
	rejected, err := s.CreateTask(req.TaskID, req, brief, &conflict)
	if rejected != nil {
		must(t, rejected.Close())
	}
	if rejected != nil || !errors.Is(err, task.ErrRequestConflict) {
		t.Fatalf("changed version bypassed guarded metadata comparison: handle=%t err=%v", rejected != nil, err)
	}
	after, err := os.Stat(path)
	must(t, err)
	if !os.SameFile(before, after) || !bytes.Equal(original, readTestFile(t, path)) {
		t.Fatal("retry replaced metadata")
	}
}
