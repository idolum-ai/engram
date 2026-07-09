package lockfile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type Lock struct {
	file *os.File
	Path string
}

type Metadata struct {
	PID        int               `json:"pid"`
	Hostname   string            `json:"hostname,omitempty"`
	AcquiredAt string            `json:"acquired_at"`
	Details    map[string]string `json:"details,omitempty"`
}

func Key(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func Acquire(dir string, key string, metadata ...Metadata) (*Lock, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, key+".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
			return nil, fmt.Errorf("another Engram process already holds %s%s", path, lockDetails(path))
		}
		return nil, err
	}
	_ = f.Truncate(0)
	meta := defaultMetadata()
	if len(metadata) > 0 {
		meta = metadata[0]
		if meta.PID == 0 {
			meta.PID = os.Getpid()
		}
		if meta.AcquiredAt == "" {
			meta.AcquiredAt = time.Now().UTC().Format(time.RFC3339)
		}
		if meta.Hostname == "" {
			meta.Hostname, _ = os.Hostname()
		}
	}
	if b, err := json.MarshalIndent(meta, "", "  "); err == nil {
		_, _ = f.Write(append(b, '\n'))
	} else {
		_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	}
	return &Lock{file: f, Path: path}, nil
}

func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	return l.file.Close()
}

func defaultMetadata() Metadata {
	host, _ := os.Hostname()
	return Metadata{
		PID:        os.Getpid(),
		Hostname:   host,
		AcquiredAt: time.Now().UTC().Format(time.RFC3339),
	}
}

func lockDetails(path string) string {
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return ""
	}
	return ": " + string(b)
}
