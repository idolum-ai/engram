package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/idolum-ai/engram/internal/atomicfile"
)

const serviceStatusFileEnv = "ENGRAM_SERVICE_STATUS_FILE"

// publishServiceIdentity lets the service manager's status command identify
// the build from the running process itself, rather than from a plist, unit, or
// installed binary that may have changed since the process started.
func publishServiceIdentity(path, build string, pid int, started time.Time) (func(), error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || !filepath.IsAbs(path) {
		return nil, fmt.Errorf("service status file must be an absolute path")
	}
	build = strings.TrimSpace(build)
	if pid <= 0 || build == "" || strings.ContainsAny(build, "\r\n") {
		return nil, fmt.Errorf("invalid service identity")
	}
	data := []byte(fmt.Sprintf("pid=%d\nbuild=%s\nstarted=%s\n", pid, build, started.UTC().Format(time.RFC3339Nano)))
	if err := atomicfile.Write(path, data); err != nil {
		return nil, fmt.Errorf("publish service identity: %w", err)
	}
	return func() {
		current, err := os.ReadFile(path)
		if err == nil && bytes.Equal(current, data) {
			_ = os.Remove(path)
		}
	}, nil
}
