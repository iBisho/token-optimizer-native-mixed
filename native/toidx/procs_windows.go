//go:build windows

// "toidx procs": the Windows process table straight from the Win32 API, in
// the CSV shapes daemon.py used to get from three PowerShell pipelines
// (Get-Process, Get-CimInstance Win32_Process). Starting PowerShell costs
// 1-20 s on some machines; this takes milliseconds.
package main

import (
	"encoding/csv"
	"fmt"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32                      = syscall.NewLazyDLL("kernel32.dll")
	ntdll                         = syscall.NewLazyDLL("ntdll.dll")
	procCreateToolhelp32Snapshot  = kernel32.NewProc("CreateToolhelp32Snapshot")
	procProcess32FirstW           = kernel32.NewProc("Process32FirstW")
	procProcess32NextW            = kernel32.NewProc("Process32NextW")
	procProcessIdToSessionId      = kernel32.NewProc("ProcessIdToSessionId")
	procQueryFullProcessImageName = kernel32.NewProc("QueryFullProcessImageNameW")
	procNtQueryInformationProcess = ntdll.NewProc("NtQueryInformationProcess")
)

const (
	th32csSnapProcess              = 0x2
	processQueryLimitedInformation = 0x1000
	processCommandLineInformation  = 60
)

type processEntry32 struct {
	Size            uint32
	Usage           uint32
	ProcessID       uint32
	DefaultHeapID   uintptr
	ModuleID        uint32
	Threads         uint32
	ParentProcessID uint32
	PriClassBase    int32
	Flags           uint32
	ExeFile         [260]uint16
}

type proc struct {
	pid, ppid uint32
	name      string // image name with extension, e.g. claude.exe
}

func snapshot() ([]proc, error) {
	h, _, err := procCreateToolhelp32Snapshot.Call(th32csSnapProcess, 0)
	if syscall.Handle(h) == syscall.InvalidHandle {
		return nil, err
	}
	defer syscall.CloseHandle(syscall.Handle(h))
	var e processEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	var out []proc
	r, _, err := procProcess32FirstW.Call(h, uintptr(unsafe.Pointer(&e)))
	if r == 0 {
		return nil, err
	}
	for r != 0 {
		out = append(out, proc{e.ProcessID, e.ParentProcessID, syscall.UTF16ToString(e.ExeFile[:])})
		r, _, _ = procProcess32NextW.Call(h, uintptr(unsafe.Pointer(&e)))
	}
	return out, nil
}

func openLimited(pid uint32) (syscall.Handle, bool) {
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, pid)
	return h, err == nil
}

func sessionID(pid uint32) (uint32, bool) {
	var sid uint32
	r, _, _ := procProcessIdToSessionId.Call(uintptr(pid), uintptr(unsafe.Pointer(&sid)))
	return sid, r != 0
}

func startTimeUTC(h syscall.Handle) string {
	var c, e, k, u syscall.Filetime
	if syscall.GetProcessTimes(h, &c, &e, &k, &u) != nil {
		return ""
	}
	return time.Unix(0, c.Nanoseconds()).UTC().Format("2006-01-02T15:04:05Z")
}

func imagePath(h syscall.Handle) string {
	buf := make([]uint16, 32768)
	n := uint32(len(buf))
	r, _, _ := procQueryFullProcessImageName.Call(uintptr(h), 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)))
	if r == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:n])
}

type unicodeString struct {
	Length, MaximumLength uint16
	Buffer                *uint16
}

func commandLine(h syscall.Handle) string {
	size := uint32(4096)
	for i := 0; i < 4; i++ {
		buf := make([]byte, size)
		var ret uint32
		status, _, _ := procNtQueryInformationProcess.Call(uintptr(h), processCommandLineInformation,
			uintptr(unsafe.Pointer(&buf[0])), uintptr(size), uintptr(unsafe.Pointer(&ret)))
		if status == 0 {
			us := (*unicodeString)(unsafe.Pointer(&buf[0]))
			if us.Buffer == nil || us.Length == 0 {
				return ""
			}
			return syscall.UTF16ToString(unsafe.Slice(us.Buffer, us.Length/2))
		}
		if ret <= size { // not a size problem
			return ""
		}
		size = ret
	}
	return ""
}

func stem(name string) string {
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		return name[:i]
	}
	return name
}

// runProcs writes one CSV per requested view:
//
//	get-process PREFIX  Id,ProcessName,SessionId,StartTime   (ProcessName without extension LIKE PREFIX*)
//	cim PREFIX          ProcessId,ParentProcessId,ExecutablePath,CommandLine,CreationDate (Name LIKE PREFIX%)
//	names               ProcessId,ParentProcessId,Name        (every process)
func runProcs(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: toidx procs get-process PREFIX | cim PREFIX | names")
		return 2
	}
	list, err := snapshot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "snapshot:", err)
		return 1
	}
	w := csv.NewWriter(os.Stdout)
	w.UseCRLF = false
	switch args[0] {
	case "get-process":
		if len(args) != 2 {
			return 2
		}
		prefix := strings.ToLower(args[1])
		// Get-Process returns processes sorted by name, then id; callers keep that order.
		sort.SliceStable(list, func(i, j int) bool {
			a, b := strings.ToLower(stem(list[i].name)), strings.ToLower(stem(list[j].name))
			if a != b {
				return a < b
			}
			return list[i].pid < list[j].pid
		})
		w.Write([]string{"Id", "ProcessName", "SessionId", "StartTime"})
		for _, p := range list {
			name := stem(p.name)
			if !strings.HasPrefix(strings.ToLower(name), prefix) {
				continue
			}
			sid := ""
			if s, ok := sessionID(p.pid); ok {
				sid = fmt.Sprint(s)
			}
			start := ""
			if h, ok := openLimited(p.pid); ok {
				start = startTimeUTC(h)
				syscall.CloseHandle(h)
			}
			w.Write([]string{fmt.Sprint(p.pid), name, sid, start})
		}
	case "cim":
		if len(args) != 2 {
			return 2
		}
		prefix := strings.ToLower(args[1])
		w.Write([]string{"ProcessId", "ParentProcessId", "ExecutablePath", "CommandLine", "CreationDate"})
		for _, p := range list {
			if !strings.HasPrefix(strings.ToLower(p.name), prefix) {
				continue
			}
			path, cmd, start := "", "", ""
			if h, ok := openLimited(p.pid); ok {
				path, cmd, start = imagePath(h), commandLine(h), startTimeUTC(h)
				syscall.CloseHandle(h)
			}
			w.Write([]string{fmt.Sprint(p.pid), fmt.Sprint(p.ppid), path, cmd, start})
		}
	case "names":
		w.Write([]string{"ProcessId", "ParentProcessId", "Name"})
		for _, p := range list {
			w.Write([]string{fmt.Sprint(p.pid), fmt.Sprint(p.ppid), p.name})
		}
	default:
		return 2
	}
	w.Flush()
	if w.Error() != nil {
		return 1
	}
	return 0
}
