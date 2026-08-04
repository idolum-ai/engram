//go:build !darwin && !linux

package codexui

import (
	"fmt"
	"time"
)

func kernelProcessStart(int) (time.Time, string, error) {
	return time.Time{}, "", fmt.Errorf("kernel process identity is unsupported")
}
