//go:build darwin

package main

import (
	"os"
	"syscall"
	"unsafe"
)

func readPassword(file *os.File) ([]byte, error) {
	original := syscall.Termios{}
	if err := ioctlTermios(file.Fd(), syscall.TIOCGETA, &original); err != nil {
		return nil, err
	}
	private := original
	private.Lflag &^= syscall.ECHO
	if err := ioctlTermios(file.Fd(), syscall.TIOCSETA, &private); err != nil {
		return nil, err
	}
	defer ioctlTermios(file.Fd(), syscall.TIOCSETA, &original)
	return readSecretLine(file)
}

func ioctlTermios(fd uintptr, request uintptr, value *syscall.Termios) error {
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, fd, request, uintptr(unsafe.Pointer(value)), 0, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
