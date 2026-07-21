package atomicfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
)

type WriteError struct {
	Err      error
	Replaced bool
}

func (e *WriteError) Error() string { return e.Err.Error() }
func (e *WriteError) Unwrap() error { return e.Err }

func ReachedReplacement(err error) bool {
	var target *WriteError
	return errors.As(err, &target) && target.Replaced
}

func Write(path string, data []byte) error {
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return &WriteError{Err: fmt.Errorf("create private temporary file: %w", err)}
	}
	temporary := file.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(temporary)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return &WriteError{Err: errors.Join(fmt.Errorf("chmod private temporary file: %w", err), file.Close())}
	}
	if _, err := file.Write(data); err != nil {
		return &WriteError{Err: errors.Join(fmt.Errorf("write private temporary file: %w", err), file.Close())}
	}
	if err := file.Sync(); err != nil {
		return &WriteError{Err: errors.Join(fmt.Errorf("sync private temporary file: %w", err), file.Close())}
	}
	if err := file.Close(); err != nil {
		return &WriteError{Err: fmt.Errorf("close private temporary file: %w", err)}
	}
	if err := os.Rename(temporary, path); err != nil {
		return &WriteError{Err: fmt.Errorf("replace private file: %w", err)}
	}
	renamed = true
	if err := SyncDir(dir); err != nil {
		return &WriteError{Err: err, Replaced: true}
	}
	return nil
}

// SyncDir makes a preceding rename durable when the host filesystem supports
// directory descriptor synchronization.
func SyncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open private file directory for sync: %w", err)
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if runtime.GOOS == "darwin" && (errors.Is(syncErr, syscall.EINVAL) || errors.Is(syncErr, syscall.ENOTSUP)) {
		syncErr = nil
	}
	if err := errors.Join(syncErr, closeErr); err != nil {
		return fmt.Errorf("sync private file directory: %w", err)
	}
	return nil
}
