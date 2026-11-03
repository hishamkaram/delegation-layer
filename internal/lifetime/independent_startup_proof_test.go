//go:build linux || darwin

package lifetime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type independentStartupProbe struct {
	directory *nativeSchedulerDirectory
	commands  []*exec.Cmd
	waits     []chan error
	records   map[string]ProcessRef
	deadline  time.Time
}

func TestIndependentStartupNativeProof(t *testing.T) {
	if os.Getenv("DELEGATE_INDEPENDENT_EXPECTED_BACKEND") == "" {
		t.Skip("explicit native infrastructure component proof")
	}
	if err := CheckIndependentHost(t.Context(), os.Environ()); err != nil {
		t.Fatal(err)
	}
	for _, abrupt := range []bool{false, true} {
		if !t.Run(fmt.Sprintf("abrupt=%t", abrupt), func(t *testing.T) { exerciseIndependentStartup(t, abrupt) }) {
			return
		}
	}
}

func exerciseIndependentStartup(t *testing.T, abrupt bool) {
	p := &independentStartupProbe{records: make(map[string]ProcessRef), deadline: time.Now().Add(90 * time.Second)}
	var err error
	p.directory, err = newNativeSchedulerDirectory()
	t.Cleanup(func() { p.cleanup(t) })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithDeadline(t.Context(), p.deadline)
	defer cancel()
	driver, err := nestedObserveNative(os.Getpid(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("driver native placement: cgroup=%s marked=%t", driver.Cgroup, driver.ScopeMarked)
	if err = p.prepare(ctx, abrupt); err != nil {
		t.Fatal(err)
	}
	scopeRecord, err := p.readRecord(ctx, "scope")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("selected native backend: %s/%s", scopeRecord.Backend, scopeRecord.Assurance)
	if err = p.verifyAdoptionAndScope(ctx); err != nil {
		t.Fatal(err)
	}
	if err = p.verifyOwnerLoss(ctx); err != nil {
		t.Fatal(err)
	}
	if err = p.closeIndependentB(ctx); err != nil {
		t.Fatal(err)
	}

	t.Log("native component: inherited lease absent from descendant, parent adoption, task scope/orphan termination, independent resident/B scope/worker survival, owner loss releases lease while adopted child remains responsive")
}

func (p *independentStartupProbe) prepare(ctx context.Context, abrupt bool) error {
	var err error
	if err = p.start("origin", abrupt); err != nil {
		return err
	}
	if _, err = p.record(ctx, "origin"); err != nil {
		return err
	}
	if err = p.start("scope-owner", abrupt); err != nil {
		return err
	}
	for _, role := range []string{"scope", "requester", "resident", "daemon", "b-scope", "b-worker", "a-provider", "b-provider"} {
		if _, err = p.record(ctx, role); err != nil {
			return err
		}
	}
	if err = p.verifyDescendantDescriptors(ctx); err != nil {
		return err
	}
	resident, err := p.record(ctx, "resident")
	if err != nil {
		return err
	}
	if !resident.LeasePresent {
		return errors.New("resident did not inherit instance lease")
	}
	if err = p.checkLease(false); err != nil {
		return err
	}
	if err = p.verifyPlacement(); err != nil {
		return err
	}
	_, err = p.ordinary(ctx)
	if err != nil {
		return err
	}
	return nil
}

func (p *independentStartupProbe) verifyAdoptionAndScope(ctx context.Context) error {
	var err error
	if err = p.release("origin"); err != nil {
		return err
	}
	if err = p.awaitCommand(ctx, 0); err != nil {
		return err
	}
	if err = p.adopted("resident", "origin"); err != nil {
		return err
	}
	if err = p.checkLease(false); err != nil {
		return err
	}
	if err = p.challenge(ctx, "daemon", 1); err != nil {
		return err
	}
	if err = p.alive("a-provider"); err != nil {
		return err
	}
	if err = p.alive("ordinary"); err != nil {
		return err
	}
	if err = p.release("scope"); err != nil {
		return err
	}
	if err = p.awaitCommand(ctx, 1); err != nil {
		return err
	}
	if err = WaitProcessAbsent(ctx, p.records["ordinary"]); err != nil {
		return err
	}
	if err = p.challenge(ctx, "daemon", 2); err != nil {
		return err
	}
	if err = p.alive("resident"); err != nil {
		return err
	}
	return p.challengeB(ctx, 1)
}

func (p *independentStartupProbe) verifyOwnerLoss(ctx context.Context) error {
	var err error
	if err = p.release("resident"); err != nil {
		return err
	}
	if err = WaitProcessAbsent(ctx, p.records["resident"]); err != nil {
		return err
	}
	if err = p.checkLease(true); err != nil {
		return err
	}
	if err = p.adopted("daemon", "resident"); err != nil {
		return err
	}
	if err = p.challenge(ctx, "daemon", 3); err != nil {
		return err
	}
	return p.challengeB(ctx, 2)
}

func (p *independentStartupProbe) start(mode string, abrupt bool) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	command := exec.Command(executable, "-test.run=^TestIndependentStartupFixture$", "-test.timeout=2m")
	command.Dir = p.directory.path
	command.Env = append(os.Environ(), independentFixtureMode+"="+mode,
		"DELEGATE_INDEPENDENT_FIXTURE_DIRECTORY="+p.directory.path,
		"DELEGATE_INDEPENDENT_FIXTURE_DEADLINE="+strconv.FormatInt(p.deadline.UnixNano(), 10),
		"DELEGATE_INDEPENDENT_FIXTURE_ABRUPT="+map[bool]string{true: "1", false: "0"}[abrupt])
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	if err = ConfigureIndependentSession(command); err != nil {
		return err
	}
	if err = command.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	p.commands = append(p.commands, command)
	p.waits = append(p.waits, done)
	go func() { done <- command.Wait() }()
	return nil
}

func (p *independentStartupProbe) waitFile(ctx context.Context, name string) error {
	return waitForEmpty(ctx, 10*time.Millisecond, func() (bool, error) {
		_, err := p.directory.root.Stat(name)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return err == nil, err
	}, sleepContext)
}

func (p *independentStartupProbe) record(ctx context.Context, role string) (independentFixtureRecord, error) {
	record, err := p.readRecord(ctx, role)
	if err != nil {
		return record, err
	}
	return record, p.alive(role)
}

func (p *independentStartupProbe) readRecord(ctx context.Context, role string) (independentFixtureRecord, error) {
	var record independentFixtureRecord
	if err := p.waitFile(ctx, role+".json"); err != nil {
		return record, err
	}
	data, err := p.directory.root.ReadFile(role + ".json")
	if err != nil {
		return record, err
	}
	if err = json.Unmarshal(data, &record); err != nil {
		return record, err
	}
	ref, err := NewProcessRef(record.PID, record.Token)
	if err != nil {
		return record, err
	}
	p.records[role] = ref
	return record, nil
}

func (p *independentStartupProbe) ordinary(ctx context.Context) (ProcessRef, error) {
	if err := p.waitFile(ctx, "ordinary.json"); err != nil {
		return ProcessRef{}, err
	}
	data, err := p.directory.root.ReadFile("ordinary.json")
	if err != nil {
		return ProcessRef{}, err
	}
	var record nestedNativeObservation
	if err = json.Unmarshal(data, &record); err != nil {
		return ProcessRef{}, err
	}
	ref, err := NewProcessRef(record.PID, record.StartToken)
	if err != nil {
		return ref, err
	}
	p.records["ordinary"] = ref
	return ref, p.alive("ordinary")
}

func (p *independentStartupProbe) alive(role string) error {
	present, err := ProcessPresent(p.records[role])
	if err != nil || !present {
		return errors.Join(fmt.Errorf("%s incarnation is not live", role), err)
	}
	return nil
}

func (p *independentStartupProbe) release(role string) error {
	name := role + ".release"
	if _, err := p.directory.root.Stat(name); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return p.directory.write(name, []byte("release\n"))
}

func (p *independentStartupProbe) challenge(ctx context.Context, role string, sequence int) error {
	if err := p.alive(role); err != nil {
		return err
	}
	name := fmt.Sprintf("%s.challenge.%d", role, sequence)
	if err := p.directory.write(name, []byte("fresh\n")); err != nil {
		return err
	}
	if err := p.waitFile(ctx, name+".reply"); err != nil {
		return err
	}
	return p.alive(role)
}

func (p *independentStartupProbe) checkLease(wantFree bool) (resultErr error) {
	file, err := p.directory.root.OpenFile("instance.lock", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		if !wantFree {
			return nil
		}
		return errors.New("instance lease remains held after resident exit")
	}
	if err != nil {
		return err
	}
	if !wantFree {
		return errors.New("instance lease unexpectedly free while resident alive")
	}
	return nil
}

func (p *independentStartupProbe) awaitCommand(ctx context.Context, index int) error {
	select {
	case err := <-p.waits[index]:
		p.waits[index] = nil
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *independentStartupProbe) cleanup(t *testing.T) {
	t.Helper()
	if p.directory == nil {
		return
	}
	if p.directory.root == nil {
		if err := p.directory.finish(false); err != nil {
			t.Error(err)
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	stopErr := p.stopFixtures()
	joinErr := p.joinCreators(ctx)
	recoverErr := p.recoverIdentities(ctx)
	absenceErr := p.verifyClosed(ctx)
	closed := errors.Join(stopErr, joinErr, recoverErr, absenceErr) == nil
	if !closed {
		t.Error(errors.Join(stopErr, joinErr, recoverErr, absenceErr))
	}
	if !closed {
		t.Logf("retained component diagnostics: %s", p.directory.path)
	}
	if err := p.directory.finish(closed); err != nil {
		t.Error(err)
	}
}

func (p *independentStartupProbe) stopFixtures() (resultErr error) {
	resultErr = p.directory.write("fixture.abort", []byte("stop-new-launches\n"))
	for _, role := range []string{"ordinary", "a-provider", "b-provider", "b-scope", "daemon", "resident", "origin", "scope", "requester", "b-worker"} {
		resultErr = errors.Join(resultErr, p.release(role))
	}
	return resultErr
}

func (p *independentStartupProbe) joinCreators(ctx context.Context) (resultErr error) {
	for index, done := range p.waits {
		if done != nil {
			err := p.awaitCommand(ctx, index)
			var exited *exec.ExitError
			// A nonzero exact Wait still proves this creator closed. Normal
			// scenario assertions check its exit before cleanup; scope.closed
			// and identity absence independently gate resource disposal here.
			if err != nil && !errors.As(err, &exited) {
				resultErr = errors.Join(resultErr, err)
			}
		}
	}
	return resultErr
}

func (p *independentStartupProbe) recoverIdentities(ctx context.Context) (resultErr error) {
	for _, role := range []string{"resident", "daemon", "requester", "b-scope", "b-worker", "a-provider", "b-provider"} {
		if _, ok := p.records[role]; !ok {
			_, err := p.readRecord(ctx, role)
			resultErr = errors.Join(resultErr, err)
		}
	}
	return resultErr
}

func (p *independentStartupProbe) verifyClosed(ctx context.Context) (resultErr error) {
	for role, ref := range p.records {
		if err := WaitProcessAbsent(ctx, ref); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("%s closure: %w", role, err))
		}
	}
	if len(p.commands) > 1 {
		for _, role := range []string{"scope", "b-scope", "daemon"} {
			_, err := p.directory.root.Stat(role + ".closed")
			resultErr = errors.Join(resultErr, err)
		}
	}
	if resultErr == nil {
		resultErr = p.checkLease(true)
	}
	return resultErr
}

func (p *independentStartupProbe) native(role string) (nestedNativeObservation, error) {
	ref := p.records[role]
	observation, err := nestedObserveNative(ref.PID(), "")
	if err != nil {
		return observation, err
	}
	if !observation.Present || !nestedFixtureTokensMatch(observation.StartToken, ref.StartToken()) {
		return observation, errors.New("native fixture identity changed")
	}
	return observation, nil
}

func (p *independentStartupProbe) adopted(role, priorParent string) error {
	current, err := p.native(role)
	if err != nil {
		return err
	}
	if current.ParentPID == p.records[priorParent].PID() {
		return errors.New("resident fixture retained absent parent")
	}
	return nil
}

func (p *independentStartupProbe) verifyPlacement() error {
	requester, err := p.native("a-provider")
	if err != nil {
		return err
	}
	for _, role := range []string{"resident", "daemon", "b-scope", "b-worker", "b-provider"} {
		current, err := p.native(role)
		if err != nil {
			return err
		}
		if requester.Cgroup != "" && (current.Cgroup == requester.Cgroup || strings.HasPrefix(current.Cgroup, requester.Cgroup+"/")) {
			if os.Getenv("DELEGATE_INDEPENDENT_EXPECTED_BACKEND") == string(KindCgroupV2) {
				return errors.New("independent infrastructure resides in finite requester cgroup")
			}
		}
	}
	return nil
}

func (p *independentStartupProbe) verifyDescendantDescriptors(ctx context.Context) error {
	for _, role := range []string{"daemon", "b-scope", "b-worker", "b-provider"} {
		actor, err := p.record(ctx, role)
		if err != nil {
			return err
		}
		if actor.LeasePresent {
			return fmt.Errorf("instance lease leaked into %s descendant", role)
		}
	}
	return nil
}

func (p *independentStartupProbe) closeIndependentB(ctx context.Context) error {
	if err := p.alive("b-provider"); err != nil {
		return err
	}
	if err := p.release("b-scope"); err != nil {
		return err
	}
	if err := p.waitFile(ctx, "b-scope.closed"); err != nil {
		return err
	}
	for _, role := range []string{"b-scope", "b-worker", "b-provider"} {
		if err := WaitProcessAbsent(ctx, p.records[role]); err != nil {
			return err
		}
	}
	return p.challenge(ctx, "daemon", 4)
}

func (p *independentStartupProbe) challengeB(ctx context.Context, sequence int) error {
	if err := p.alive("b-scope"); err != nil {
		return err
	}
	if err := p.challenge(ctx, "b-worker", sequence); err != nil {
		return err
	}
	return p.challenge(ctx, "b-provider", sequence)
}
