//go:build linux

package githubauth

import (
	"os"
	"syscall"
)

func privateKeyPlatformMetadata(info os.FileInfo) (privateKeyFilePlatformMetadata, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return privateKeyFilePlatformMetadata{}, false
	}
	return privateKeyFilePlatformMetadata{
		device:         uint64(stat.Dev),
		inode:          uint64(stat.Ino),
		uid:            stat.Uid,
		gid:            stat.Gid,
		linkCount:      uint64(stat.Nlink),
		changeTimeSec:  stat.Ctim.Sec,
		changeTimeNano: stat.Ctim.Nsec,
		changeTimeOK:   true,
	}, true
}
