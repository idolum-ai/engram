//go:build darwin

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
		inode:          stat.Ino,
		uid:            stat.Uid,
		gid:            stat.Gid,
		linkCount:      uint64(stat.Nlink),
		changeTimeSec:  stat.Ctimespec.Sec,
		changeTimeNano: stat.Ctimespec.Nsec,
		changeTimeOK:   true,
	}, true
}
