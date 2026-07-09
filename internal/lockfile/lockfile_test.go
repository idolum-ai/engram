package lockfile

import (
	"os"
	"strings"
	"testing"
)

func TestAcquireRejectsDuplicateLiveLock(t *testing.T) {
	dir := t.TempDir()
	key := Key("token", "user", "group")
	first, err := Acquire(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := Acquire(dir, key); err == nil {
		second.Close()
		t.Fatal("second Acquire succeeded")
	}
}

func TestAcquireWritesMetadata(t *testing.T) {
	dir := t.TempDir()
	key := Key("token", "user", "dm")
	lock, err := Acquire(dir, key, Metadata{Details: map[string]string{"telegram_user_id": "123", "version": "test"}})
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	b, err := os.ReadFile(lock.Path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{`"pid"`, `"telegram_user_id": "123"`, `"version": "test"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("lock metadata missing %q:\n%s", want, got)
		}
	}
}
