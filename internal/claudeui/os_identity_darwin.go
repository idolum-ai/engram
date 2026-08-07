//go:build darwin

package claudeui

import (
	"bytes"
	"fmt"
	"syscall"
	"time"
	"unsafe"
)

const (
	procInfoCallPIDInfo = 2
	procPIDTBSDInfo     = 3
	procPIDPathInfo     = 11
	procPIDPathMax      = 4096
)

type darwinProcBSDInfo struct {
	Flags, Status, XStatus, PID, PPID    uint32
	UID, GID, RUID, RGID, SVUID, SVGID   uint32
	Reserved                             uint32
	Command                              [16]byte
	Name                                 [32]byte
	NFiles, PGID, PJobC, TTYDev, TTYPGID uint32
	Nice                                 int32
	StartSeconds, StartMicroseconds      uint64
}

func (OSExecutableResolver) Resolve(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("invalid process ID")
	}
	buffer := make([]byte, procPIDPathMax)
	read, _, errno := syscall.Syscall6(syscall.SYS_PROC_INFO, procInfoCallPIDInfo, uintptr(pid), procPIDPathInfo, 0, uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)))
	if errno != 0 || read > uintptr(len(buffer)) {
		return "", fmt.Errorf("read process executable path: bytes=%d errno=%v", read, errno)
	}
	end := bytes.IndexByte(buffer, 0)
	if end < 0 {
		end = len(buffer)
	}
	path := string(buffer[:end])
	if path == "" {
		return "", fmt.Errorf("empty process executable path")
	}
	return path, nil
}

func (OSProcessStartResolver) Resolve(pid int) (time.Time, string, error) {
	if pid <= 0 {
		return time.Time{}, "", fmt.Errorf("invalid process ID")
	}
	var info darwinProcBSDInfo
	size := unsafe.Sizeof(info)
	read, _, errno := syscall.Syscall6(syscall.SYS_PROC_INFO, procInfoCallPIDInfo, uintptr(pid), procPIDTBSDInfo, 0, uintptr(unsafe.Pointer(&info)), size)
	if errno != 0 || read != size || info.PID != uint32(pid) || info.StartSeconds == 0 || info.StartMicroseconds >= 1_000_000 {
		return time.Time{}, "", fmt.Errorf("read process start identity")
	}
	started := time.Unix(int64(info.StartSeconds), int64(info.StartMicroseconds)*1_000).UTC()
	return started, fmt.Sprintf("darwin:%d:%06d", info.StartSeconds, info.StartMicroseconds), nil
}
