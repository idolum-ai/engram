package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteReplacesPrivateFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.json")
	if err := Write(path, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, []byte("second")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "second" || info.Mode().Perm() != 0o600 {
		t.Fatalf("data=%q mode=%o", data, info.Mode().Perm())
	}
}

func TestReachedReplacement(t *testing.T) {
	t.Parallel()
	cause := errors.New("sync failed")
	if ReachedReplacement(&WriteError{Err: cause}) {
		t.Fatal("pre-replacement error reported replacement")
	}
	if !ReachedReplacement(&WriteError{Err: cause, Replaced: true}) {
		t.Fatal("post-replacement error did not report replacement")
	}
}
