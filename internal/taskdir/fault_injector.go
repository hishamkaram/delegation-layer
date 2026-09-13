package taskdir

// FaultInjector defines per-object hooks to inject deterministic persistence errors
// without using mutable globals.
type FaultInjector interface {
	// OnStageWrite is invoked during stage file writing.
	// Returning shortCount < len(data) or non-nil err causes write failure.
	OnStageWrite(destPath string, data []byte) (shortCount int, err error)

	// OnStageBarrier is invoked before closing the stage file for linking.
	OnStageBarrier(destPath string) error

	// OnStageClose is invoked when closing the writable stage descriptor.
	OnStageClose(destPath string) error

	// OnLink is invoked before hardlinking the stage file to destPath.
	OnLink(stagePath, destPath string) error

	// OnPostLinkDirBarrier is invoked during the directory barrier after hard link.
	OnPostLinkDirBarrier(destPath string) error
	OnAfterLinkDirBarrier(destPath string) error

	// OnCleanupUnlink is invoked before unlinking the stage file.
	OnCleanupUnlink(stagePath string) error

	// OnCleanupDirBarrier is invoked during the directory barrier after stage cleanup.
	OnCleanupDirBarrier(dirPath string) error

	// OnDirBarrier is invoked during directory creation barriers.
	OnDirBarrier(path string) error

	// OnParentDirBarrier is invoked during parent directory creation barriers.
	OnParentDirBarrier(path string) error

	// OnRawWrite is invoked when writing raw files (stdout/stderr).
	OnRawWrite(path string) error

	// OnRawBarrier is invoked when syncing raw files.
	OnRawBarrier(path string) error

	// OnRawClose is invoked when closing raw files.
	OnRawClose(path string) error

	// OnRawDirBarrier is invoked when syncing the raw directory.
	OnRawDirBarrier(dirPath string) error
}

// BaseFaultInjector is a no-op implementation of FaultInjector.
type BaseFaultInjector struct{}

func (b *BaseFaultInjector) OnStageWrite(_ string, data []byte) (int, error) {
	return len(data), nil
}

func (b *BaseFaultInjector) OnStageBarrier(_ string) error {
	return nil
}

func (b *BaseFaultInjector) OnStageClose(_ string) error {
	return nil
}

func (b *BaseFaultInjector) OnLink(_, _ string) error {
	return nil
}

func (b *BaseFaultInjector) OnPostLinkDirBarrier(_ string) error {
	return nil
}

func (b *BaseFaultInjector) OnCleanupUnlink(_ string) error {
	return nil
}

func (b *BaseFaultInjector) OnCleanupDirBarrier(_ string) error {
	return nil
}

func (b *BaseFaultInjector) OnDirBarrier(_ string) error {
	return nil
}

func (b *BaseFaultInjector) OnParentDirBarrier(_ string) error {
	return nil
}

func (b *BaseFaultInjector) OnRawWrite(_ string) error {
	return nil
}

func (b *BaseFaultInjector) OnRawBarrier(_ string) error {
	return nil
}

func (b *BaseFaultInjector) OnRawClose(_ string) error {
	return nil
}

func (b *BaseFaultInjector) OnRawDirBarrier(_ string) error {
	return nil
}

func (b *BaseFaultInjector) OnAfterLinkDirBarrier(_ string) error { return nil }

// StageCreateInjector is a per-store fixture seam observing the chosen stage
// name immediately before O_EXCL creation. It does not override randomness.
type StageCreateInjector interface {
	OnBeforeStageCreate(stagePath, destPath string) error
}
