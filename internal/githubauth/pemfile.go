package githubauth

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// PrivateKeyFileIdentity is non-secret metadata that binds a credential read
// to one file identity and one public key.
type PrivateKeyFileIdentity struct {
	Fingerprint string
	device      uint64
	inode       uint64
	size        int64
	modTimeNano int64
}

func (i PrivateKeyFileIdentity) Equal(other PrivateKeyFileIdentity) bool {
	return i.Fingerprint == other.Fingerprint &&
		i.device == other.device &&
		i.inode == other.inode &&
		i.size == other.size &&
		i.modTimeNano == other.modTimeNano
}

// ReadPrivateKeyFile securely reads and validates one live GitHub App private
// key source. The caller must zero the returned bytes.
func ReadPrivateKeyFile(path string) ([]byte, PrivateKeyFileIdentity, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." {
		return nil, PrivateKeyFileIdentity{}, fmt.Errorf("GitHub App private key path is empty")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, PrivateKeyFileIdentity{}, fmt.Errorf("open GitHub App private key: %w", err)
	}
	defer file.Close()

	before, err := privateKeyFileIdentity(file)
	if err != nil {
		return nil, PrivateKeyFileIdentity{}, err
	}
	data, err := io.ReadAll(io.LimitReader(file, maxPrivateKeySize+1))
	if err != nil {
		zeroBytes(data)
		return nil, PrivateKeyFileIdentity{}, fmt.Errorf("read GitHub App private key: %w", err)
	}
	if len(data) == 0 || len(data) > maxPrivateKeySize || int64(len(data)) != before.size {
		zeroBytes(data)
		return nil, PrivateKeyFileIdentity{}, fmt.Errorf("GitHub App private key must be between 1 and %d bytes and remain stable while read", maxPrivateKeySize)
	}
	after, err := privateKeyFileIdentity(file)
	if err != nil || !before.Equal(after) {
		zeroBytes(data)
		return nil, PrivateKeyFileIdentity{}, fmt.Errorf("GitHub App private key changed while it was read")
	}
	key, err := ParsePrivateKey(data)
	if err != nil {
		zeroBytes(data)
		return nil, PrivateKeyFileIdentity{}, err
	}
	fingerprint, err := PublicFingerprint(key)
	if err != nil {
		zeroBytes(data)
		return nil, PrivateKeyFileIdentity{}, err
	}
	before.Fingerprint = fingerprint
	return data, before, nil
}

func privateKeyFileIdentity(file *os.File) (PrivateKeyFileIdentity, error) {
	if err := validatePrivateFile(file, "GitHub App private key"); err != nil {
		return PrivateKeyFileIdentity{}, err
	}
	info, err := file.Stat()
	if err != nil {
		return PrivateKeyFileIdentity{}, fmt.Errorf("stat GitHub App private key: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return PrivateKeyFileIdentity{}, fmt.Errorf("GitHub App private key file identity is unavailable")
	}
	if info.Size() <= 0 || info.Size() > maxPrivateKeySize {
		return PrivateKeyFileIdentity{}, fmt.Errorf("GitHub App private key must be between 1 and %d bytes", maxPrivateKeySize)
	}
	return PrivateKeyFileIdentity{
		device:      uint64(stat.Dev),
		inode:       uint64(stat.Ino),
		size:        info.Size(),
		modTimeNano: info.ModTime().UnixNano(),
	}, nil
}
