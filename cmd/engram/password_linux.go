//go:build linux

package main

import (
	"os"
	"syscall"
	"unsafe"
)

func readPassword(file *os.File) ([]byte, error) {
	original := syscall.Termios{}
	if err := ioctlTermios(file.Fd(), syscall.TCGETS, &original); err != nil {
		return nil, err
	}
	private := original
	private.Lflag &^= syscall.ECHO
	if err := ioctlTermios(file.Fd(), syscall.TCSETS, &private); err != nil {
		return nil, err
	}
	defer ioctlTermios(file.Fd(), syscall.TCSETS, &original)
	return readSecretLine(file)
}

func ioctlTermios(fd uintptr, request uintptr, value *syscall.Termios) error {
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, fd, request, uintptr(unsafe.Pointer(value)), 0, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
