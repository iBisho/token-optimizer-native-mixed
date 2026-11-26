//go:build !windows

package main

import (
	"errors"
	"io/fs"
)

// The hook front door is Windows-only for now: on POSIX, run.py's runtime
// detection scans the process tree (OpenCode ancestors), which is not ported.
func hookPlatformSupported() bool { return false }

func privateFile(string, fs.FileInfo) bool { return false }

func stdoutIsConsole() bool { return false }

func spawnWait(string, []string, []string, int) (int, error) {
	return 0, errors.New("hook front door: unsupported platform")
}
