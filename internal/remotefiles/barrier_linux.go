//go:build linux

package remotefiles

import "os"

func barrierFile(file *os.File) error { return file.Sync() }
