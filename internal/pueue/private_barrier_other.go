//go:build !darwin && !linux

package pueue

import (
	"errors"
	"os"
)

func privateBarrierFile(*os.File) error {
	return errors.New("private supervisor durability is unsupported on this platform")
}

func privateBarrierDir(string) error {
	return errors.New("private supervisor durability is unsupported on this platform")
}
