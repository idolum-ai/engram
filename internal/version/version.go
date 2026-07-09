package version

import "runtime"

var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

func String() string {
	return "engram " + Version + " commit=" + Commit + " date=" + Date + " go=" + runtime.Version()
}
