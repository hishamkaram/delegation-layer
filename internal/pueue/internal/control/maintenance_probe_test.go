package control

import (
	"sync"
	"sync/atomic"
)

type privateTerminalTriggerScenario struct {
	fixture         privateSupervisorTestFixture
	client          *Client
	base            string
	startedPath     string
	releasePath     string
	commandObserved chan struct{}
	statusCommands  atomic.Int64
	removeCommands  atomic.Int64
	releaseOnce     sync.Once
}

type requestedPrivateMaintenanceResult struct {
	generation uint64
	started    bool
	err        error
}

type requestedPrivateMaintenanceBatch struct {
	results        <-chan requestedPrivateMaintenanceResult
	requestsDone   <-chan struct{}
	contendersDone <-chan struct{}
	release        func()
}
