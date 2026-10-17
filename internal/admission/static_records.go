package admission

import (
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

type StaticOptions struct {
	Catalog  commonprovider.Catalog
	Profiles Profiles
}

type PreflightStage uint8

const (
	PreflightStatic PreflightStage = iota
	PreflightPlacement
	PreflightReady
)

// PreflightResult is a detached static observation. It conveys no permission
// to launch and never claims supervised runtime or live-provider acceptance.
type PreflightResult struct {
	Description *commonprovider.Description
	Stage       PreflightStage
}
