package admission

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/config"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func SelectAutoProvider(options StaticOptions, root, rootID, taskID, cwd string, requested task.TaskConfig, brief []byte) (string, error) {
	if requested.Permission == "" {
		requested.Permission = config.DefaultMode
	}
	providers := options.Catalog.Descriptions()
	var reasons []string
	considered := 0
	authenticationBlocked := true
	for _, description := range providers {
		if requested.Interactive && !slices.Contains(description.SupportedOptions, commonprovider.OptionInteractive) {
			continue
		}
		if !slices.Contains(description.SupportedModes, requested.Permission) {
			continue
		}
		considered++
		request, err := BuildRequest(rootID, taskID, description.ID, cwd, requested, brief, nil)
		if err != nil {
			return "", err
		}
		err = ValidateStatic(options, root, request)
		if err != nil {
			if !errors.Is(err, commonprovider.ErrAuthenticationBlocked) {
				authenticationBlocked = false
			}
			reasons = append(reasons, boundedSelectionReason(description.ID, err))
			continue
		}
		return description.ID, nil
	}
	if len(reasons) == 0 {
		return "", fmt.Errorf("%w: no provider supports permission %s", errNoReadyProvider, requested.Permission)
	}
	if considered > 0 && authenticationBlocked {
		return "", fmt.Errorf("%w: %w: %s", commonprovider.ErrAuthenticationBlocked, errNoReadyProvider, strings.Join(reasons, "; "))
	}
	return "", fmt.Errorf("%w: %s", errNoReadyProvider, strings.Join(reasons, "; "))
}
