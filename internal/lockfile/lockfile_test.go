package lockfile

import "testing"

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
