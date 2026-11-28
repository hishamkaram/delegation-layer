package guardian

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestUnlimitedGuardianSeparatesExecutionFromControlBounds(t *testing.T) {
	g := &Guardian{options: Options{Unlimited: true, Now: time.Now}}
	if g.deadlineReached() {
		t.Fatal("unlimited guardian considers execution expired")
	}
	timer, events := lifetimeGuardianDeadlineTimer(time.Time{})
	if timer != nil || events != nil {
		t.Fatal("unlimited guardian armed an execution expiry timer")
	}
	idle, cancelIdle := g.receiveContext(context.Background())
	defer cancelIdle()
	if _, bounded := idle.Deadline(); bounded {
		t.Fatal("idle unlimited guardian inherited a finite execution bound")
	}
	for _, check := range []struct {
		name    string
		bound   time.Duration
		context func(context.Context) (context.Context, context.CancelFunc)
	}{
		{"control", 10 * time.Second, g.workContext},
		{"startup", 30 * time.Second, g.startupContext},
	} {
		t.Run(check.name, func(t *testing.T) {
			before := time.Now()
			ctx, cancel := check.context(context.Background())
			defer cancel()
			deadline, bounded := ctx.Deadline()
			if !bounded || deadline.Before(before.Add(check.bound)) || deadline.After(time.Now().Add(check.bound)) {
				t.Fatal("unlimited execution lost its independent operation bound")
			}
			parent, end := context.WithDeadline(context.Background(), before.Add(time.Second))
			defer end()
			child, endChild := check.context(parent)
			defer endChild()
			actual, bounded := child.Deadline()
			if !bounded || !actual.Equal(before.Add(time.Second)) {
				t.Fatal("operation extended its caller's earlier deadline")
			}
		})
	}
}

func TestUnlimitedWorkerHelloUsesIndependentControlDeadline(t *testing.T) {
	worker, owner := net.Pipe()
	cleanupLifetimeTestResource(t, worker)
	observed := &lifetimeGuardianDeadlineConn{Conn: worker, deadline: make(chan time.Time, 1)}
	id := strings.Repeat("a", 32)
	server := startLifetimeWireServer(t, owner, id, []lifetimeWireExpectation{{kind: lifetimeMessageHello, accepted: true}})
	before := time.Now()
	client, err := NewWorkerClient(observed, &lifetimeWireScope{}, id, time.Time{}, lifetimeWireNoopLaunchGateFactory)
	if err != nil {
		t.Fatal(err)
	}
	closeClient := ownLifetimeWireClient(t, client)
	deadline := <-observed.deadline
	if deadline.Before(before.Add(10*time.Second)) || deadline.After(time.Now().Add(10*time.Second)) {
		t.Fatal("unlimited worker hello lost its finite control deadline")
	}
	if err = closeClient(); err != nil {
		t.Fatal(err)
	}
	if err = <-server; err != nil {
		t.Fatal(err)
	}
}

func unlimitedGuardianFixture(t *testing.T, stop func() (bool, error)) *lifetimeGuardianTestFixture {
	t.Helper()
	return newLifetimeGuardianTestFixtureWithOptionsAndContext(t, t.Context(), time.Now().Add(48*time.Hour), nil,
		lifetime.KindProcessGroup, lifetime.AssuranceCooperative, nil,
		func(scope *lifetimeGuardianTestScope) { scope.emptyResults = []bool{false, true} },
		func(options *Options) {
			options.Deadline, options.Unlimited = time.Time{}, true
			options.StartupDeadline = time.Now().Add(UnlimitedStartupTimeout)
			options.Now, options.UserStop = time.Now, stop
			if stop != nil {
				options.StopPoll = time.Minute
			}
		})
}

func TestUnlimitedGuardianBoundsMissingWorkerHello(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := unlimitedGuardianFixture(t, nil)
		synctest.Wait()
		time.Sleep(UnlimitedStartupTimeout + time.Second)
		synctest.Wait()
		fixture.worker.release()
		if err := fixture.waitRun(t); !errors.Is(err, errLifetimeGuardianStartupDeadline) {
			t.Fatalf("missing hello: %v", err)
		}
		_, terminated, _ := fixture.scope.counts()
		if terminated != 1 {
			t.Fatalf("missing hello termination=%d", terminated)
		}
	})
}

func TestUnlimitedGuardianWaitsPastFormerDefaultsAndStillCancels(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var requested atomic.Bool
		fixture := unlimitedGuardianFixture(t, func() (bool, error) { return requested.Load(), nil })
		fixture.callMust(t, "hello", fixture.message(lifetimeMessageHello, 0, 0))
		fixture.callMust(t, "begin", fixture.message(lifetimeMessageBegin, 1, 0))
		fixture.callMust(t, "started", fixture.message(lifetimeMessageStarted, 1, 101))
		time.Sleep(24 * time.Hour)
		synctest.Wait()
		_, terminated, _ := fixture.scope.counts()
		if terminated != 0 {
			t.Fatal("unlimited execution expired while idle")
		}
		select {
		case <-fixture.runExited:
			t.Fatal("unlimited guardian exited while idle")
		default:
		}
		requested.Store(true)
		time.Sleep(time.Minute)
		synctest.Wait()
		fixture.worker.release()
		if err := fixture.waitRun(t); !errors.Is(err, errLifetimeGuardianStopRequested) {
			t.Fatalf("user stop: %v", err)
		}
		causes := fixture.records.terminationCauses()
		if len(causes) != 1 || causes[0] != task.LifetimeTerminationRequested {
			t.Fatalf("stop causes=%v", causes)
		}
		_, terminated, _ = fixture.scope.counts()
		if terminated != 1 {
			t.Fatalf("native termination=%d", terminated)
		}
	})
}
