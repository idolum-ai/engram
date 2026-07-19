//go:build darwin

package app

import (
	"encoding/binary"
	"fmt"
	"syscall"
)

func readHostBootID() string {
	value, err := syscall.Sysctl("kern.boottime")
	if err != nil {
		return ""
	}
	return darwinBootID([]byte(value))
}

func darwinBootID(value []byte) string {
	if len(value) < 12 {
		return ""
	}
	seconds := int64(binary.NativeEndian.Uint64(value[:8]))
	microseconds := int32(binary.NativeEndian.Uint32(value[8:12]))
	if seconds <= 0 || microseconds < 0 || microseconds >= 1_000_000 {
		return ""
	}
	return fmt.Sprintf("darwin:%d.%06d", seconds, microseconds)
}
