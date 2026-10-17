package supervisor

func CloseOwnerMaintenance(owner *ClientOwner, cause error) {
	if owner == nil {
		return
	}
	CloseMaintenance(owner.Release(), cause)
}
