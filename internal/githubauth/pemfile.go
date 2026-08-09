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
	Fingerprint    string
	device         uint64
	inode          uint64
	size           int64
	modTimeNano    int64
	mode           os.FileMode
	uid            uint32
	gid            uint32
	linkCount      uint64
	changeTimeSec  int64
	changeTimeNano int64
	changeTimeOK   bool
}

func (i PrivateKeyFileIdentity) Equal(other PrivateKeyFileIdentity) bool {
	return i.Fingerprint == other.Fingerprint && i.sameFile(other)
}

func (i PrivateKeyFileIdentity) sameFile(other PrivateKeyFileIdentity) bool {
	return i.device == other.device &&
		i.inode == other.inode &&
		i.size == other.size &&
		i.modTimeNano == other.modTimeNano &&
		i.mode == other.mode &&
		i.uid == other.uid &&
		i.gid == other.gid &&
		i.linkCount == other.linkCount &&
		i.changeTimeSec == other.changeTimeSec &&
		i.changeTimeNano == other.changeTimeNano &&
		i.changeTimeOK == other.changeTimeOK
}

// ReadPrivateKeyFile securely reads and validates one live GitHub App private
// key source. The caller must zero the returned bytes.
func ReadPrivateKeyFile(path string) ([]byte, PrivateKeyFileIdentity, error) {
	return readPrivateKeyFile(path, nil)
}

func readPrivateKeyFile(path string, afterRead func()) ([]byte, PrivateKeyFileIdentity, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." {
		return nil, PrivateKeyFileIdentity{}, fmt.Errorf("GitHub App private key path is empty")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, PrivateKeyFileIdentity{}, fmt.Errorf("open GitHub App private key: %w", err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, PrivateKeyFileIdentity{}, fmt.Errorf("stat GitHub App private key: %w", err)
	}
	identity, err := privateKeyFileIdentity(info)
	if err != nil {
		return nil, PrivateKeyFileIdentity{}, err
	}
	if err := validatePrivateKeyFileIdentity(identity); err != nil {
		return nil, PrivateKeyFileIdentity{}, err
	}

	// The validated size is bounded and stable metadata. Allocate once so
	// growing read buffers cannot leave abandoned copies of credential bytes.
	data := make([]byte, int(identity.size))
	_, err = io.ReadFull(file, data)
	if err != nil {
		zeroBytes(data)
		return nil, PrivateKeyFileIdentity{}, fmt.Errorf("read GitHub App private key: %w", err)
	}
	if afterRead != nil {
		afterRead()
	}
	if len(data) == 0 || len(data) > maxPrivateKeySize || int64(len(data)) != identity.size {
		zeroBytes(data)
		return nil, PrivateKeyFileIdentity{}, fmt.Errorf("GitHub App private key must be between 1 and %d bytes and remain stable while read", maxPrivateKeySize)
	}

	pathInfo, err := os.Lstat(path)
	if err != nil {
		zeroBytes(data)
		return nil, PrivateKeyFileIdentity{}, fmt.Errorf("revalidate GitHub App private key pathname: %w", err)
	}
	pathIdentity, err := privateKeyFileIdentity(pathInfo)
	if err != nil || !identity.sameFile(pathIdentity) {
		zeroBytes(data)
		return nil, PrivateKeyFileIdentity{}, fmt.Errorf("GitHub App private key pathname changed while it was read")
	}

	fingerprint, err := PrivateKeyFingerprint(data)
	if err != nil {
		zeroBytes(data)
		return nil, PrivateKeyFileIdentity{}, err
	}
	identity.Fingerprint = fingerprint
	return data, identity, nil
}

func privateKeyFileIdentity(info os.FileInfo) (PrivateKeyFileIdentity, error) {
	platform, ok := privateKeyPlatformMetadata(info)
	if !ok {
		return PrivateKeyFileIdentity{}, fmt.Errorf("GitHub App private key file identity is unavailable")
	}
	return PrivateKeyFileIdentity{
		device:         platform.device,
		inode:          platform.inode,
		size:           info.Size(),
		modTimeNano:    info.ModTime().UnixNano(),
		mode:           info.Mode(),
		uid:            platform.uid,
		gid:            platform.gid,
		linkCount:      platform.linkCount,
		changeTimeSec:  platform.changeTimeSec,
		changeTimeNano: platform.changeTimeNano,
		changeTimeOK:   platform.changeTimeOK,
	}, nil
}

func validatePrivateKeyFileIdentity(identity PrivateKeyFileIdentity) error {
	if !identity.mode.IsRegular() || identity.mode.Perm()&0o077 != 0 || int(identity.uid) != os.Geteuid() {
		return fmt.Errorf("GitHub App private key must be a private regular file owned by uid %d", os.Geteuid())
	}
	if identity.linkCount != 1 {
		return fmt.Errorf("GitHub App private key must have exactly one hard link")
	}
	if identity.size <= 0 || identity.size > maxPrivateKeySize {
		return fmt.Errorf("GitHub App private key must be between 1 and %d bytes", maxPrivateKeySize)
	}
	return nil
}

type privateKeyFilePlatformMetadata struct {
	device         uint64
	inode          uint64
	uid            uint32
	gid            uint32
	linkCount      uint64
	changeTimeSec  int64
	changeTimeNano int64
	changeTimeOK   bool
}
