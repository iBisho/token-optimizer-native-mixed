//go:build windows

package main

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const detachedProcess = 0x00000008

func hookPlatformSupported() bool { return true }

// privateFile: the record lives under the user's own profile; Windows ACLs
// keep it private there (same trust as the launcher's interpreter cache).
func privateFile(string, fs.FileInfo) bool { return true }

func stdoutIsConsole() bool {
	if os.Stdout == nil {
		return false
	}
	var mode uint32
	return syscall.GetConsoleMode(syscall.Handle(os.Stdout.Fd()), &mode) == nil
}

func stdStream(f *os.File) *os.File {
	if f == nil || syscall.Handle(f.Fd()) == syscall.InvalidHandle || f.Fd() == 0 {
		return nil // exec passes NUL, like run.py's subprocess.DEVNULL
	}
	return f
}

// spawnWait starts path with args, detached from any console (no window can
// appear) and with this process's standard handles, as run.py starts its
// child. timeoutS > 0 kills the child after that many seconds (run.py's
// 120 s ceiling) and waits up to 5 s more. Returns the child's exit code.
func spawnWait(path string, args, env []string, timeoutS int) (int, error) {
	cmd := exec.Command(path, args...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdStream(os.Stdin), stdStream(os.Stdout), stdStream(os.Stderr)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	if timeoutS > 0 {
		select {
		case err = <-done:
		case <-time.After(time.Duration(timeoutS) * time.Second):
			_ = cmd.Process.Kill()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
			}
			return 0, nil
		}
	} else {
		err = <-done
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return 0, nil
}
