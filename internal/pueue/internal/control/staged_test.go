package control

import (
	"sync"
	"sync/atomic"
)

type failFirstEnqueueObserver struct {
	fake           *fakeSupervisor
	hidden         string
	moved          bool
	failNext       bool
	observerErr    error
	enqueueEntries int
}

type enqueueStatusTransition struct {
	fake     *fakeSupervisor
	status   []byte
	entered  chan struct{}
	once     sync.Once
	entries  atomic.Int32
	writeErr error
}

type ordinaryRecoveryResult struct {
	observation Observation
	err         error
}

type inspectionRecoveryResult struct {
	observation InspectionObservation
	err         error
}

type stagedRecoveryExpectation uint8

const (
	expectStashedRecovery stagedRecoveryExpectation = iota
	expectQueuedRejection
	expectRejectedRecovery
)

type ordinaryStagedRecoveryCase struct {
	name        string
	state       State
	rowID       int64
	label       string
	scheduled   bool
	expectation stagedRecoveryExpectation
}

type inspectionStagedRecoveryCase struct {
	name        string
	state       State
	rowID       int64
	label       string
	group       string
	scheduled   bool
	expectation stagedRecoveryExpectation
}
