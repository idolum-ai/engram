//go:build !linux && !darwin

package githubauth

import "os"

func privateKeyPlatformMetadata(os.FileInfo) (privateKeyFilePlatformMetadata, bool) {
	return privateKeyFilePlatformMetadata{}, false
}
