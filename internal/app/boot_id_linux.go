//go:build linux

package app

import (
	"os"
	"strings"

	"github.com/idolum-ai/engram/internal/recovery"
)

func readHostBootID() string {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	id := strings.TrimSpace(string(data))
	if !recovery.ValidSessionID(id) {
		return ""
	}
	return strings.ToLower(id)
}
