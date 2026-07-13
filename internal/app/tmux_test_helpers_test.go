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
