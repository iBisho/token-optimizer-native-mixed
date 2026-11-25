//go:build !windows

package main

import (
	"fmt"
	"os"
)

// runProcs is Windows-only: elsewhere daemon.py uses ps, which is fast.
func runProcs(args []string) int {
	fmt.Fprintln(os.Stderr, "toidx procs: Windows only")
	return 2
}
