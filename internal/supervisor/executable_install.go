package supervisor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

func InstallStateRunner(root, source string) (_ string, resultErr error) {
	base := filepath.Join(root, ".supervisor")
	if err := ensureSupervisorDirectory(root, base); err != nil {
		return "", err
	}
	digest, err := stateExecutableSourceDigest(source)
	if err != nil {
		return "", err
	}
	name := stateRunnerName + "-" + digest
	destination := filepath.Join(base, name)
	if filepath.Clean(source) == destination {
		if _, err = protectedStateExecutableDigest(destination); err != nil {
			return "", err
		}
		return destination, nil
	}
	if err = installImmutableStateExecutable(base, source, destination, digest); err != nil {
		return "", err
	}
	return destination, nil
}

func installImmutableStateExecutable(base, source, destination, digest string) (resultErr error) {
	matched, err := stateExecutableMatches(destination, digest)
	if err != nil {
		return err
	}
	if matched {
		return nil
	}
	stagePath, err := prepareStateExecutableStage(base, source, digest)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, removeStateExecutableStage(base, stagePath, syncRunnerDirectory))
	}()
	if err = publishStateExecutableStage(stagePath, destination, digest); err != nil {
		return err
	}
	return syncRunnerDirectory(base)
}

func removeStateExecutableStage(base, stagePath string, syncDirectory func(string) error) error {
	if err := os.Remove(stagePath); err != nil {
		return err
	}
	return syncDirectory(base)
}

func prepareStateExecutableStage(base, source, digest string) (stagePath string, resultErr error) {
	input, err := os.Open(source)
	if err != nil {
		return "", err
	}
	stage, err := os.CreateTemp(base, ".delegate-run-")
	if err != nil {
		return "", errors.Join(err, input.Close())
	}
	tempPath := stage.Name()
	stageOpen := true
	defer func() {
		if stageOpen {
			resultErr = errors.Join(resultErr, stage.Close())
		}
		resultErr = errors.Join(resultErr, input.Close())
		if resultErr != nil {
			resultErr = errors.Join(resultErr, os.Remove(tempPath))
			stagePath = ""
		}
	}()
	if _, err = io.Copy(stage, input); err != nil {
		return "", err
	}
	if err = stage.Chmod(0o700); err != nil {
		return "", err
	}
	if err = taskdir.BarrierFile(stage); err != nil {
		return "", err
	}
	if err = stage.Close(); err != nil {
		stageOpen = false
		return "", err
	}
	stageOpen = false
	stageDigest, err := protectedStateExecutableDigest(tempPath)
	if err != nil || stageDigest != digest {
		return "", errors.Join(fmt.Errorf("%w: runner changed while being installed", pueue.ErrConfiguration), err)
	}
	return tempPath, nil
}

func publishStateExecutableStage(stagePath, destination, digest string) error {
	if err := os.Link(stagePath, destination); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		if matched, matchErr := stateExecutableMatches(destination, digest); matchErr != nil {
			return matchErr
		} else if !matched {
			return fmt.Errorf("%w: immutable state runner path contains different executable bytes", pueue.ErrConfiguration)
		}
	}
	return nil
}

func stateExecutableSourceDigest(source string) (string, error) {
	info, err := os.Lstat(source)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%w: state-root executable source must be an executable regular file", pueue.ErrConfiguration)
	}
	return commonprovider.FingerprintExecutable(source)
}

func protectedStateExecutableDigest(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o700 {
		return "", fmt.Errorf("%w: state-root executable must be a protected regular file with mode 0700", pueue.ErrConfiguration)
	}
	return commonprovider.FingerprintExecutable(path)
}

func stateExecutableMatches(destination, sourceDigest string) (bool, error) {
	destinationDigest, err := protectedStateExecutableDigest(destination)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return destinationDigest == sourceDigest, nil
}

func ensureSupervisorDirectory(root, base string) error {
	info, err := os.Lstat(base)
	if errors.Is(err, os.ErrNotExist) {
		if err = os.Mkdir(base, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		if err = syncRunnerDirectory(root); err != nil {
			return err
		}
		info, err = os.Lstat(base)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: private supervisor directory is not a protected directory", pueue.ErrConfiguration)
	}
	return nil
}

func syncRunnerDirectory(path string) (resultErr error) {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	return unix.Fsync(int(directory.Fd()))
}
