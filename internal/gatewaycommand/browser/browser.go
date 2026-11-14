// Package browser requests desktop URL activation without inheriting a browser
// process into Native's command lifetime. Login remains the OAuth owner's job.
package browser

import (
	"context"
	"errors"
	"net/url"
	"runtime"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/gatewayservice"
	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
)

var errActivation = errors.New("browser_activation_not_confirmed")

// Open requests activation within one five-second budget. A failed or timed-out
// request does not prove the browser stayed closed; callers retain the same login.
func Open(ctx context.Context, owner string, environment []string, uri string) error {
	if ctx == nil || !validURI(uri) {
		return errActivation
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if runtime.GOOS == "linux" {
		return openPortal(bounded, environment, uri)
	}
	executable, args, err := command(runtime.GOOS, uri)
	if err != nil {
		return err
	}
	executable, err = verifiedexec.LocatePath(executable)
	if err != nil {
		return err
	}
	runner, err := gatewayservice.NewNativeRunner(owner, environment)
	if err != nil {
		return err
	}
	result, err := runner.Run(bounded, executable, args)
	if err != nil || result.ExitCode != 0 {
		return errors.Join(errActivation, err)
	}
	return nil
}

func validURI(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && len(raw) <= 8192 && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Opaque == "" && u.Fragment == ""
}

func command(platform, uri string) (string, []string, error) {
	if !validURI(uri) || platform != "darwin" {
		return "", nil, errActivation
	}
	return "/usr/bin/open", []string{uri}, nil
}
