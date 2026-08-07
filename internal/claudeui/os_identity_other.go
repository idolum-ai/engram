//go:build !darwin && !linux

package claudeui

import (
	"fmt"
	"runtime"
	"time"
)

func (OSExecutableResolver) Resolve(int) (string, error) {
	return "", fmt.Errorf("running executable resolution is unsupported on %s", runtime.GOOS)
}

func (OSProcessStartResolver) Resolve(int) (time.Time, string, error) {
	return time.Time{}, "", fmt.Errorf("process start resolution is unsupported on %s", runtime.GOOS)
}
