package app

import (
	"fmt"
	"strings"
)

func framedTmuxRecord(values ...string) string {
	var out strings.Builder
	for _, value := range values {
		fmt.Fprintf(&out, "%d:%s", len(value), value)
	}
	return out.String() + "\n"
}

func framedTmuxBindingRecord(values ...string) string {
	return framedTmuxRecord(append([]string{appTestServerID}, values...)...)
}

func framedStyledCaptureMetadata(command string) string {
	return framedTmuxRecord(appTestServerID, "@1", "%1", "71", "37", "build pane", "/tmp", command, "1", "0")
}
