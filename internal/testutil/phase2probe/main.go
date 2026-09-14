// Command phase2probe exercises the actual configuration boundary without any
// supervisor connection or task authority. It is never included in a release.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	result, err := inspect(args)
	if err != nil {
		if _, writeErr := fmt.Fprintln(stderr, err); writeErr != nil {
			return 1
		}
		return 2
	}
	if err = json.NewEncoder(stdout).Encode(result); err != nil {
		if _, writeErr := fmt.Fprintln(stderr, err); writeErr != nil {
			return 1
		}
		return 1
	}
	return 0
}

type report struct {
	SchemaVersion int                   `json:"schema_version"`
	ConfigSHA256  string                `json:"config_sha256"`
	Parsed        *pueue.Config         `json:"parsed"`
	Resolved      *pueue.ResolvedConfig `json:"resolved,omitempty"`
	ResolvedHash  string                `json:"resolved_sha256,omitempty"`
}

func inspect(args []string) (report, error) {
	if len(args) != 2 && len(args) != 3 {
		return report{}, errors.New("usage: phase2probe parse CONFIG | isolate CONFIG BASE")
	}
	if (args[0] != "parse" || len(args) != 2) && (args[0] != "isolate" || len(args) != 3) {
		return report{}, errors.New("invalid inspection command")
	}
	result, err := parse(args[1])
	if err != nil || args[0] == "parse" {
		return result, err
	}
	context, err := pueue.CurrentResolutionContext()
	if err != nil {
		return report{}, err
	}
	resolved, err := pueue.ResolveConfig(result.Parsed, context)
	if err != nil {
		return report{}, err
	}
	if err = resolved.ValidateIsolation(args[2]); err != nil {
		return report{}, err
	}
	result.Resolved = &resolved
	result.ResolvedHash, err = resolved.Digest()
	return result, err
}

func parse(path string) (report, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return report{}, errors.New("config must be a clean absolute path")
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return report{}, err
	}
	if canonical != path {
		return report{}, errors.New("config must be a canonical path")
	}
	data, err := readRegular(path, pueue.MaxControlBytes)
	if err != nil {
		return report{}, err
	}
	config, err := pueue.ParseConfig(data)
	if err != nil {
		return report{}, err
	}
	result := report{SchemaVersion: 1, ConfigSHA256: task.ComputeSHA256(data), Parsed: config}
	return result, nil
}

func readRegular(path string, limit int64) (data []byte, err error) {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("config must be a regular file")
	}
	return task.ReadBounded(f, limit)
}
