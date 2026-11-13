package infrastructure

func ownerInitialOperationFixture(rootID string, kind InitialOperationKind) InitialOperation {
	operation := InitialOperation{Kind: kind}.normalized()
	switch kind {
	case InitialRecoverSaved, InitialPrepareContinuation:
		operation.Source = SavedBindingSource{RootID: rootID, Kind: SavedBindingTaskMetadata, OwnerID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BindingSHA256: ownerWireDigest}
	case InitialRuntimeRestart:
		operation.RestartFrom = StartupReference{Sequence: 1, Epoch: rootID, IntentSHA256: ownerWireDigest}
	case "", InitialEnsureOwn, InitialMigrateOwn:
	}
	return operation
}
