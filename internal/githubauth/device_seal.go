package githubauth

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/idolum-ai/engram/internal/atomicfile"
)

const deviceSealKeyBytes = 32

func ensureDeviceSealKey(path string) ([]byte, error) {
	key, err := readDeviceSealKey(path)
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key = make([]byte, deviceSealKeyBytes)
	if _, err := rand.Read(key); err != nil {
		zeroBytes(key)
		return nil, fmt.Errorf("generate GitHub device seal: %w", err)
	}
	if err := atomicfile.Write(path, key); err != nil {
		zeroBytes(key)
		return nil, fmt.Errorf("save GitHub device seal: %w", err)
	}
	return key, nil
}

func readDeviceSealKey(path string) ([]byte, error) {
	file, err := os.OpenFile(filepath.Clean(path), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open GitHub device seal: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat GitHub device seal: %w", err)
	}
	identity, err := privateKeyFileIdentity(info)
	if err != nil || !identity.mode.IsRegular() || identity.mode.Perm()&0o077 != 0 || int(identity.uid) != os.Geteuid() || identity.linkCount != 1 || identity.size != deviceSealKeyBytes {
		return nil, fmt.Errorf("GitHub device seal must be a %d-byte private regular file with one link owned by uid %d", deviceSealKeyBytes, os.Geteuid())
	}
	key := make([]byte, deviceSealKeyBytes)
	if _, err := io.ReadFull(file, key); err != nil {
		zeroBytes(key)
		return nil, fmt.Errorf("read GitHub device seal: %w", err)
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		zeroBytes(key)
		return nil, fmt.Errorf("revalidate GitHub device seal pathname: %w", err)
	}
	pathIdentity, err := privateKeyFileIdentity(pathInfo)
	if err != nil || !identity.sameFile(pathIdentity) {
		zeroBytes(key)
		return nil, fmt.Errorf("GitHub device seal pathname changed while it was read")
	}
	return key, nil
}
