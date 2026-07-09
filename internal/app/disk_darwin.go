//go:build darwin

package app

import "syscall"

type syscallStatfs = syscall.Statfs_t

func statfs(path string, out *syscallStatfs) error {
	return syscall.Statfs(path, out)
}
