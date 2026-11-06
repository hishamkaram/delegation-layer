package task

// ExecutableIdentity distinguishes current installation coordinates from
// historical executable digests. It does not attest executable contents.
type ExecutableIdentity string

const ExecutableIdentityInstalledPathV1 ExecutableIdentity = "installed-path-v1"

func (identity ExecutableIdentity) Validate() error {
	switch identity {
	case "", ExecutableIdentityInstalledPathV1:
		return nil
	default:
		return ErrInvalidEnum
	}
}
