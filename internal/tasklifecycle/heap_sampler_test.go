package tasklifecycle_test

import (
	"testing"
	"testing/synctest"
	"time"
)

// The heap sampler's owner is the one goroutine that calls stop. That owner
// closes quit once; the worker alone writes peak and then closes done; stop
// joins done before it reads peak. The quit channel reference never changes
// while the worker may still select on it.

// TestHeapSamplerStopKeepsItsQuitChannelForTheWorker drives stop against a
// controlled worker that, like the real one, observes quit through the
// sampler. The worker captures the original channel first, so a stop that
// replaces the field cannot strand it, and the test fails on the identity
// assertion instead of hanging. The test owns an abort channel and joins the
// worker on every path.
func TestHeapSamplerStopKeepsItsQuitChannelForTheWorker(t *testing.T) {
	const knownPeak = 4242
	sampler := &heapSampler{quit: make(chan struct{}), done: make(chan struct{})}
	original := sampler.quit
	abort := make(chan struct{})
	workerReturned := make(chan struct{})
	go func() {
		defer close(workerReturned)
		defer close(sampler.done)
		select {
		case <-original:
		case <-abort:
		}
		sampler.peak = knownPeak
	}()
	guard := time.AfterFunc(10*time.Second, func() { close(abort) })
	first := sampler.stop()
	second := sampler.stop()
	guardFired := !guard.Stop()
	if guardFired {
		// Stop does not wait for the callback; its last step closes abort.
		<-abort
		t.Error("stop did not release the worker before the hang guard")
	} else {
		close(abort)
	}
	<-workerReturned

	if first != knownPeak || second != knownPeak {
		t.Fatalf("stop returned %d then %d, want the worker's peak %d both times", first, second, knownPeak)
	}
	if sampler.quit != original {
		t.Fatal("stop replaced the quit channel the worker selects on")
	}
	select {
	case <-original:
	default:
		t.Fatal("stop did not close the quit channel")
	}
}

// TestHeapSamplerStartStopIsJoinedAndIdempotent runs the real worker. The
// long interval means no tick is expected; synctest.Wait only establishes that
// the worker is blocked in its select before stop runs. No peak value or
// latency is asserted.
func TestHeapSamplerStartStopIsJoinedAndIdempotent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sampler := startHeapSampler(time.Hour)
		quit := sampler.quit
		synctest.Wait()

		first := sampler.stop()
		select {
		case <-sampler.done:
		default:
			t.Fatal("stop returned before the sampler goroutine finished")
		}
		second := sampler.stop()
		if first != second {
			t.Fatalf("second stop returned %d, want the first result %d", second, first)
		}
		if sampler.quit != quit {
			t.Fatal("stop replaced the quit channel")
		}
	})
}
