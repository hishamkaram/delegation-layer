package infrastructure

func ownerInitialOperationFixture(rootID string, kind InitialOperationKind) InitialOperation {
	operation := InitialOperation{Kind: kind}.normalized()
	switch kind {
	case InitialRecoverSaved, InitialPrepareContinuation:
		operation.Source = SavedBindingSource{RootID: rootID, Kind: SavedBindingTaskMetadata, OwnerID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BindingSHA256: ownerWireDigest}
	case InitialMigrateReferenced:
		operation.Migration = MigrationReference{RootID: rootID, RecordSHA256: ownerWireDigest}
	case "", InitialEnsureOwn, InitialMigrateOwn:
	}
	return operation
}
