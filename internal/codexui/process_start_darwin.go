//go:build darwin

package codexui

import (
	"fmt"
	"syscall"
	"time"
	"unsafe"
)

const (
	procInfoCallPIDInfo = 2
	procPIDTBSDInfo     = 3
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

func kernelProcessStart(pid int) (time.Time, string, error) {
	if pid <= 0 {
		return time.Time{}, "", fmt.Errorf("invalid process id")
	}
	var info darwinProcBSDInfo
	size := unsafe.Sizeof(info)
	read, _, errno := syscall.Syscall6(
		syscall.SYS_PROC_INFO,
		procInfoCallPIDInfo,
		uintptr(pid),
		procPIDTBSDInfo,
		0,
		uintptr(unsafe.Pointer(&info)),
		size,
	)
	if errno != 0 || read != size || info.PID != uint32(pid) || info.StartSeconds == 0 || info.StartMicroseconds >= 1_000_000 {
		return time.Time{}, "", fmt.Errorf("read process start identity")
	}
	started := time.Unix(int64(info.StartSeconds), int64(info.StartMicroseconds)*1_000).UTC()
	return started, fmt.Sprintf("darwin:%d:%06d", info.StartSeconds, info.StartMicroseconds), nil
}
