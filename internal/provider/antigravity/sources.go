package antigravity

import (
	"fmt"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

type sourceBytes = commonprovider.SourceBytes

func readPolicySource(path string) (sourceBytes, error) {
	source, err := commonprovider.ReadPolicySource(path)
	if err != nil {
		return source, fmt.Errorf("%w: %w", ErrUnsupportedProfile, err)
	}
	return source, nil
}
