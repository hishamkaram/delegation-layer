//go:build linux

package pueue

import (
	"errors"
	"fmt"
	"os"
)

func privateBarrierFile(file *os.File) error {
	return file.Sync()
}

func privateBarrierDir(path string) (resultErr error) {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open private supervisor directory barrier: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	return privateBarrierFile(directory)
}
