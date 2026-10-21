package contract

import (
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const candidateRequestSHA256 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func checkpointPayload(tree, parent, operationID, requestSHA256 string) string {
	return "tree " + tree + "\nparent " + parent +
		"\nauthor Delegate <delegate@localhost> 1761048000 +0000\ncommitter Delegate <delegate@localhost> 1761048000 +0000\n\n" +
		"Delegate checkpoint\n\n" + CheckpointTrailer + operationID + "\n" + CheckpointRequestTrailer + requestSHA256 + "\n"
}

func ordinaryTarget() CheckpointTarget {
	source := Source{Root: "/repo", CommonDir: "/repo/.git", GitDir: "/repo/.git", RelativeCwd: "sub", BaseOID: repeatHex('a', 40), ObjectFormat: "sha1"}
	return CheckpointTarget{Source: source.WithCommonDirIdentity(DirectoryIdentity{Device: 1, Inode: 2}), CheckoutRoot: "/repo", AdminDir: "/repo/.git"}
}

func managedTarget() CheckpointTarget {
	source := Source{Root: "/source", CommonDir: "/source/.git", GitDir: "/source/.git", RelativeCwd: ".", BaseOID: repeatHex('a', 40), ObjectFormat: "sha1"}
	return CheckpointTarget{Source: source.WithCommonDirIdentity(DirectoryIdentity{Device: 1, Inode: 2}), CheckoutRoot: "/container/workspace", AdminDir: "/source/.git/worktrees/workspace"}
}

func checkpointCandidate(t testing.TB, request OperationRequest, target CheckpointTarget) CheckpointCandidate {
	t.Helper()
	ref, err := CheckpointRef(request.RootID, request.OperationID)
	if err != nil {
		t.Fatalf("fixture ref: %v", err)
	}
	return CheckpointCandidate{
		SchemaVersion: task.SchemaVersion, OperationID: request.OperationID, RequestSHA256: candidateRequestSHA256, Target: target,
		ObservedHEAD: repeatHex('b', 40), IndexSHA256: repeatHex('e', 64), TreeOID: repeatHex('c', 40), CommitOID: repeatHex('d', 40),
		SnapshotRef: ref, CommitPayload: checkpointPayload(repeatHex('c', 40), repeatHex('b', 40), request.OperationID, candidateRequestSHA256),
	}
}

func TestCheckpointCandidateAcceptsOrdinaryManagedAndSHA256Targets(t *testing.T) {
	ordinary := checkpointRequest(ordinarySelection())
	managed := checkpointRequest(&CheckpointSelection{Kind: CheckpointManaged})
	wide := checkpointCandidate(t, managed, managedTarget())
	wide.Target.Source.ObjectFormat, wide.Target.Source.BaseOID = "sha256", repeatHex('a', 64)
	wide.ObservedHEAD, wide.TreeOID, wide.CommitOID = repeatHex('b', 64), repeatHex('c', 64), repeatHex('d', 64)
	wide.CommitPayload = checkpointPayload(wide.TreeOID, wide.ObservedHEAD, managed.OperationID, candidateRequestSHA256)
	for name, check := range map[string]struct {
		candidate CheckpointCandidate
		request   OperationRequest
	}{
		"ordinary": {candidate: checkpointCandidate(t, ordinary, ordinaryTarget()), request: ordinary},
		"managed":  {candidate: checkpointCandidate(t, managed, managedTarget()), request: managed},
		"sha256":   {candidate: wide, request: managed},
	} {
		if err := ValidateCheckpointCandidate(check.candidate, check.request, candidateRequestSHA256); err != nil {
			t.Fatalf("%s candidate rejected: %v", name, err)
		}
	}
}

func TestCheckpointCandidateRefusesIdentityAndTargetMismatches(t *testing.T) {
	cases := map[string]func(*CheckpointCandidate, *OperationRequest){
		"schema":    func(candidate *CheckpointCandidate, _ *OperationRequest) { candidate.SchemaVersion = 0 },
		"operation": func(candidate *CheckpointCandidate, _ *OperationRequest) { candidate.OperationID = repeatHex('9', 32) },
		"request digest": func(candidate *CheckpointCandidate, _ *OperationRequest) {
			candidate.RequestSHA256 = repeatHex('9', 64)
		},
		"removal request":     func(_ *CheckpointCandidate, request *OperationRequest) { request.Kind = OperationCheckpointRemove },
		"selection missing":   func(_ *CheckpointCandidate, request *OperationRequest) { request.Checkpoint = nil },
		"index digest":        func(candidate *CheckpointCandidate, _ *OperationRequest) { candidate.IndexSHA256 = "" },
		"head format":         func(candidate *CheckpointCandidate, _ *OperationRequest) { candidate.ObservedHEAD = repeatHex('b', 64) },
		"commit format":       func(candidate *CheckpointCandidate, _ *OperationRequest) { candidate.CommitOID = "HEAD" },
		"unpinned common dir": func(candidate *CheckpointCandidate, _ *OperationRequest) { candidate.Target.Source.CommonDirInode = 0 },
		"incomplete source":   func(candidate *CheckpointCandidate, _ *OperationRequest) { candidate.Target.Source.BaseOID = "" },
		"relative checkout":   func(candidate *CheckpointCandidate, _ *OperationRequest) { candidate.Target.Source.Root = "repo" },
		"ordinary admin not gitdir": func(candidate *CheckpointCandidate, _ *OperationRequest) {
			candidate.Target.AdminDir = "/elsewhere/.git"
		},
		"ordinary other checkout": func(candidate *CheckpointCandidate, _ *OperationRequest) { candidate.Target.CheckoutRoot = "/repo/sub" },
		"ordinary other admin": func(candidate *CheckpointCandidate, _ *OperationRequest) {
			candidate.Target.AdminDir = "/repo/.git/worktrees/other"
		},
		"ordinary other cwd": func(_ *CheckpointCandidate, request *OperationRequest) { request.Checkpoint.Cwd = "/repo" },
		"ordinary disguised": func(_ *CheckpointCandidate, request *OperationRequest) {
			request.Checkpoint = &CheckpointSelection{Kind: CheckpointManaged}
		},
		"unknown selection kind":   func(_ *CheckpointCandidate, request *OperationRequest) { request.Checkpoint.Kind = "worktree" },
		"ref of another root":      func(_ *CheckpointCandidate, request *OperationRequest) { request.RootID = repeatHex('9', 32) },
		"ref of capture namespace": func(candidate *CheckpointCandidate, _ *OperationRequest) { candidate.SnapshotRef += "/captures" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			request := checkpointRequest(ordinarySelection())
			candidate := checkpointCandidate(t, request, ordinaryTarget())
			mutate(&candidate, &request)
			if err := ValidateCheckpointCandidate(candidate, request, candidateRequestSHA256); err == nil {
				t.Fatal("mismatched candidate accepted")
			}
		})
	}
}

func TestCheckpointCandidateKeepsOrdinaryAdminOutsideCommonDirShape(t *testing.T) {
	request := checkpointRequest(ordinarySelection())
	outside := func() CheckpointCandidate {
		candidate := checkpointCandidate(t, request, ordinaryTarget())
		candidate.Target.Source.GitDir, candidate.Target.AdminDir = "/gitdirs/repo", "/gitdirs/repo"
		return candidate
	}
	if err := ValidateCheckpointCandidate(outside(), request, candidateRequestSHA256); err != nil {
		t.Fatalf("ordinary admin directory outside the common directory rejected: %v", err)
	}
	controls := map[string]func(*CheckpointTarget){
		"admin is not the source gitdir":  func(target *CheckpointTarget) { target.AdminDir = "/gitdirs/other" },
		"source gitdir is not the admin":  func(target *CheckpointTarget) { target.Source.GitDir = "/repo/.git" },
		"checkout is not the source root": func(target *CheckpointTarget) { target.CheckoutRoot = "/gitdirs" },
		"unclean admin": func(target *CheckpointTarget) {
			target.AdminDir, target.Source.GitDir = "/gitdirs/../repo", "/gitdirs/../repo"
		},
		"unpinned common dir": func(target *CheckpointTarget) { target.Source.CommonDirInode = 0 },
	}
	for name, mutate := range controls {
		candidate := outside()
		mutate(&candidate.Target)
		if err := ValidateCheckpointCandidate(candidate, request, candidateRequestSHA256); err == nil {
			t.Fatalf("%s: mismatched ordinary target accepted", name)
		}
	}
}

func TestCheckpointCandidateRefusesManagedTargetDisguises(t *testing.T) {
	cases := map[string]func(*CheckpointTarget){
		"source root as checkout": func(target *CheckpointTarget) { target.CheckoutRoot = target.Source.Root },
		"common dir as admin":     func(target *CheckpointTarget) { target.AdminDir = target.Source.CommonDir },
	}
	for name, mutate := range cases {
		request := checkpointRequest(&CheckpointSelection{Kind: CheckpointManaged})
		candidate := checkpointCandidate(t, request, managedTarget())
		mutate(&candidate.Target)
		if err := ValidateCheckpointCandidate(candidate, request, candidateRequestSHA256); err == nil {
			t.Fatalf("%s: managed target accepted", name)
		}
	}
}

func TestCheckpointCandidateBindsPayloadToTreeParentAndIdentity(t *testing.T) {
	cases := map[string]func(*CheckpointCandidate){
		"tree metadata changed": func(candidate *CheckpointCandidate) { candidate.TreeOID = repeatHex('f', 40) },
		"head metadata changed": func(candidate *CheckpointCandidate) { candidate.ObservedHEAD = repeatHex('f', 40) },
		"payload tree changed": func(candidate *CheckpointCandidate) {
			candidate.CommitPayload = strings.Replace(candidate.CommitPayload, "tree c", "tree f", 1)
		},
		"payload parent changed": func(candidate *CheckpointCandidate) {
			candidate.CommitPayload = strings.Replace(candidate.CommitPayload, "parent b", "parent f", 1)
		},
		"payload of another op": func(candidate *CheckpointCandidate) {
			candidate.CommitPayload = strings.Replace(candidate.CommitPayload, ": 2222", ": 3333", 1)
		},
		"payload of other digest": func(candidate *CheckpointCandidate) {
			candidate.CommitPayload = strings.Replace(candidate.CommitPayload, ": 0123", ": 9123", 1)
		},
		"second parent": func(candidate *CheckpointCandidate) {
			candidate.CommitPayload = strings.Replace(candidate.CommitPayload, "\nauthor", "\nparent "+repeatHex('f', 40)+"\nauthor", 1)
		},
		"no parent": func(candidate *CheckpointCandidate) {
			candidate.CommitPayload = strings.Replace(candidate.CommitPayload, "parent "+candidate.ObservedHEAD+"\n", "", 1)
		},
		"extra header": func(candidate *CheckpointCandidate) {
			candidate.CommitPayload = strings.Replace(candidate.CommitPayload, "\n\nDelegate checkpoint", "\nencoding UTF-8\n\nDelegate checkpoint", 1)
		},
		"empty author": func(candidate *CheckpointCandidate) {
			candidate.CommitPayload = strings.Replace(candidate.CommitPayload, "author Delegate <delegate@localhost> 1761048000 +0000", "author  ", 1)
		},
		"committer first": func(candidate *CheckpointCandidate) {
			candidate.CommitPayload = strings.Replace(candidate.CommitPayload, "author ", "committer ", 1)
		},
		"trailers not final": func(candidate *CheckpointCandidate) { candidate.CommitPayload += "Signed-off-by: someone\n" },
		"duplicate trailer": func(candidate *CheckpointCandidate) {
			candidate.CommitPayload = strings.Replace(candidate.CommitPayload, "Delegate checkpoint", CheckpointTrailer+repeatHex('3', 32), 1)
		},
		"trailers only in subject": func(candidate *CheckpointCandidate) {
			candidate.CommitPayload = strings.Replace(candidate.CommitPayload, "\n"+CheckpointRequestTrailer, " "+CheckpointRequestTrailer, 1)
		},
		"no message": func(candidate *CheckpointCandidate) {
			candidate.CommitPayload, _, _ = strings.Cut(candidate.CommitPayload, "\n\n")
		},
		"no final newline": func(candidate *CheckpointCandidate) {
			candidate.CommitPayload = strings.TrimSuffix(candidate.CommitPayload, "\n")
		},
		"carriage return": func(candidate *CheckpointCandidate) {
			candidate.CommitPayload = strings.Replace(candidate.CommitPayload, "\n", "\r\n", 1)
		},
		"tab": func(candidate *CheckpointCandidate) {
			candidate.CommitPayload = strings.Replace(candidate.CommitPayload, "Delegate checkpoint", "Delegate\tcheckpoint", 1)
		},
		"non ascii": func(candidate *CheckpointCandidate) {
			candidate.CommitPayload = strings.Replace(candidate.CommitPayload, "Delegate <", "Délégué <", 1)
		},
		"nul": func(candidate *CheckpointCandidate) {
			candidate.CommitPayload = strings.Replace(candidate.CommitPayload, "checkpoint\n", "checkpoint\x00\n", 1)
		},
		"empty": func(candidate *CheckpointCandidate) { candidate.CommitPayload = "" },
		"over bound": func(candidate *CheckpointCandidate) {
			candidate.CommitPayload = strings.Repeat("x", CheckpointCommitPayloadLimit+1)
		},
		"blank header line": func(candidate *CheckpointCandidate) { candidate.CommitPayload = "\n" + candidate.CommitPayload },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			request := checkpointRequest(ordinarySelection())
			candidate := checkpointCandidate(t, request, ordinaryTarget())
			mutate(&candidate)
			if err := ValidateCheckpointCandidate(candidate, request, candidateRequestSHA256); err == nil {
				t.Fatalf("payload accepted:\n%q", candidate.CommitPayload)
			}
		})
	}
}

func TestCheckpointCandidateAcceptsPayloadAtBound(t *testing.T) {
	request := checkpointRequest(ordinarySelection())
	candidate := checkpointCandidate(t, request, ordinaryTarget())
	padding := CheckpointCommitPayloadLimit - len(candidate.CommitPayload)
	candidate.CommitPayload = strings.Replace(candidate.CommitPayload, "Delegate checkpoint", "Delegate checkpoint"+strings.Repeat("x", padding), 1)
	if len(candidate.CommitPayload) != CheckpointCommitPayloadLimit {
		t.Fatalf("fixture length=%d", len(candidate.CommitPayload))
	}
	if err := ValidateCheckpointCandidate(candidate, request, candidateRequestSHA256); err != nil {
		t.Fatalf("payload at bound rejected: %v", err)
	}
}

func TestCheckpointRefHasOneCanonicalOwnedName(t *testing.T) {
	root, checkpoint := repeatHex('1', 32), repeatHex('2', 32)
	ref, err := CheckpointRef(root, checkpoint)
	if err != nil || ref != "refs/delegate/"+root+"/checkpoints/"+checkpoint {
		t.Fatalf("CheckpointRef=%q, %v", ref, err)
	}
	if err = ValidateOwnedRef(ref); err != nil {
		t.Fatalf("checkpoint ref is not owned: %v", err)
	}
	if err = ValidateCheckpointRef(ref, root, checkpoint); err != nil {
		t.Fatalf("canonical ref rejected: %v", err)
	}
	for _, other := range []string{
		"refs/delegate/" + root + "/workspaces/" + checkpoint + "/base",
		"refs/delegate/" + root + "/checkpoints/" + repeatHex('3', 32),
		"refs/delegate/" + repeatHex('4', 32) + "/checkpoints/" + checkpoint,
		"refs/delegate/" + root + "/checkpoints/" + checkpoint + "/",
		"",
	} {
		if err = ValidateCheckpointRef(other, root, checkpoint); err == nil {
			t.Fatalf("foreign ref %q accepted", other)
		}
	}
	for _, invalid := range [][2]string{{"root", checkpoint}, {root, "../" + checkpoint}, {root, ""}} {
		if _, err = CheckpointRef(invalid[0], invalid[1]); err == nil {
			t.Fatalf("CheckpointRef accepted %q", invalid)
		}
	}
}
