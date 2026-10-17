package policy

import "slices"

// KnownLayer validates the same role vocabulary used by dependency policy.
func KnownLayer(role string) bool { return layerRank(role) >= 0 }

func layerRank(role string) int {
	return slices.Index([]string{"pure", "runtime", "application", "tool", "test"}, role)
}

func allowedLayerDependency(from, to string) bool {
	// Roles run from inner data/rules to outer composition. An import may stay
	// within its role or point inward; an unknown role grants no permission.
	source, target := layerRank(from), layerRank(to)
	return source >= 0 && target >= 0 && target <= source
}
