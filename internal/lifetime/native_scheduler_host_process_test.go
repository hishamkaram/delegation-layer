//go:build linux || darwin

package lifetime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"time"
)

type nativeSchedulerCommand struct {
	cmd        *exec.Cmd
	identity   ProcessRef
	done       chan struct{}
	waitErr    error
	captureErr error
	stdout     *os.File
	stderr     *os.File
	stdoutName string
}

func (f *nativeSchedulerFixture) start(arguments []string, host bool) (*nativeSchedulerCommand, error) {
	name := fmt.Sprintf("command-%03d", len(f.commands)+1)
	command := &nativeSchedulerCommand{done: make(chan struct{}), stdoutName: name + ".stdout"}
	var err error
	command.stdout, err = f.directory.root.OpenFile(command.stdoutName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	command.stderr, err = f.directory.root.OpenFile(name+".stderr", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, errors.Join(err, command.stdout.Close())
	}
	command.cmd = exec.Command(f.cli, append([]string{"--root", f.root}, arguments...)...)
	command.cmd.Dir = f.directory.path
	command.cmd.Env = os.Environ() // Preserve the caller's placement markers; never repair global configuration.
	command.cmd.Stdout, command.cmd.Stderr = command.stdout, command.stderr
	if host {
		if err = ConfigureIndependentSession(command.cmd); err != nil {
			return nil, errors.Join(err, command.stdout.Close(), command.stderr.Close())
		}
	}
	f.commands = append(f.commands, command)
	if host {
		f.hosts = append(f.hosts, command)
	}
	if err = command.cmd.Start(); err != nil {
		command.waitErr = err
		command.captureErr = errors.Join(command.stdout.Close(), command.stderr.Close())
		close(command.done)
		return command, errors.Join(err, command.captureErr)
	}
	command.identity, err = ObserveStartedProcess(command.cmd.Process.Pid)
	// Register before observation, then retain exactly one Wait even if identity
	// observation failed. Signal delivery is never attempted without that identity.
	go command.join()
	return command, err
}

func (c *nativeSchedulerCommand) join() {
	c.waitErr = c.cmd.Wait()
	c.captureErr = errors.Join(c.stdout.Close(), c.stderr.Close())
	close(c.done)
}

func (c *nativeSchedulerCommand) closed() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

func (c *nativeSchedulerCommand) wait(ctx context.Context) error {
	if c.closed() {
		return errors.Join(c.waitErr, c.captureErr)
	}
	select {
	case <-c.done:
		return errors.Join(c.waitErr, c.captureErr)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *nativeSchedulerFixture) control(ctx context.Context, arguments []string, expected ...int) (nativeSchedulerResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nativeSchedulerResponse{}, err
	}
	command, err := f.start(append(slices.Clone(arguments), "--json"), false)
	if err != nil {
		return nativeSchedulerResponse{}, err
	}
	err = command.wait(ctx)
	if !command.closed() || command.captureErr != nil {
		return nativeSchedulerResponse{}, err
	}
	if err != nil {
		var exited *exec.ExitError
		if !errors.As(err, &exited) {
			return nativeSchedulerResponse{}, err
		}
	}
	code := command.cmd.ProcessState.ExitCode()
	if !slices.Contains(expected, code) {
		return nativeSchedulerResponse{}, fmt.Errorf("candidate control exited %d; inspect its private capture", code)
	}
	response, err := f.decode(command.stdoutName)
	if err == nil && (response.Error != "" || (len(response.Failure) > 0 && !bytes.Equal(response.Failure, []byte("null")))) {
		err = fmt.Errorf("candidate control reported a concrete fault; inspect its private capture")
	}
	return response, err
}

func (f *nativeSchedulerFixture) decode(name string) (response nativeSchedulerResponse, resultErr error) {
	file, err := f.directory.root.Open(name)
	if err != nil {
		return response, err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	info, err := file.Stat()
	const limit = 1024 * 1024
	if err != nil {
		return response, err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > limit {
		return response, fmt.Errorf("candidate response is not a bounded JSON file")
	}
	decoder := json.NewDecoder(io.LimitReader(file, limit+1))
	if err = decoder.Decode(&response); err != nil {
		return response, err
	}
	var trailing json.RawMessage
	if err = decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return response, errors.Join(errors.New("candidate returned trailing JSON"), err)
	}
	return response, nil
}
