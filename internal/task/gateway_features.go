package task

// These executable capabilities are public connector contracts. They are not
// persisted root-required features and do not change historical task formats.
const (
	WorkspaceFilesystemFeature    = "workspace-filesystem-v1"
	NativeExecutionOptionsFeature = "native-execution-options-v1"
	ProjectTokenSHA256Feature     = "project-token-sha256-v1"
	GuardedContinuationFeature    = "guarded-continuation-v1"
	ScopedModelDiscoveryFeature   = "scoped-model-discovery-v1"
	OriginBindingFeature          = "origin-binding-v1"
)
