//go:build !darwin && !linux

package claudecontext

import "os"

func ownedByCurrentUser(os.FileInfo) bool { return false }
