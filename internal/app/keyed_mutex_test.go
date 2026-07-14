package app

import (
	"testing"
	"time"
)

func TestKeyedMutexSerializesOneKeyAndReleasesRegistryEntry(t *testing.T) {
	var set keyedMutexSet
	first := set.handle(7)
	first.Lock()
	second := set.handle(7)
	acquired := make(chan struct{})
	done := make(chan struct{})
	go func() {
		second.Lock()
		close(acquired)
		second.Unlock()
		close(done)
	}()
	select {
	case <-acquired:
		t.Fatal("second handle acquired the same key concurrently")
	case <-time.After(20 * time.Millisecond):
	}
	first.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("second handle did not acquire after release")
	}
	set.mu.Lock()
	defer set.mu.Unlock()
	if len(set.entries) != 0 {
		t.Fatalf("keyed mutex retained %d idle entries", len(set.entries))
	}
}
