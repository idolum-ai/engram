package app

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestQueueRefreshCoalescesWhileRunning(t *testing.T) {
	app := &App{
		summaryQueued:  map[int]bool{},
		summaryRunning: map[int]bool{},
		summaryForce:   map[int]bool{},
		sleepHook:      func(time.Duration) {},
	}
	done := make(chan int, 2)
	calls := 0
	app.refreshHook = func(ctx context.Context, id int, force bool) {
		calls++
		if calls == 1 {
			app.queueRefresh(id, true, 0)
		}
		done <- calls
	}

	app.queueRefresh(7, true, 0)
	<-done
	<-done

	app.summaryMu.Lock()
	defer app.summaryMu.Unlock()
	if calls != 2 {
		t.Fatalf("refresh calls = %d, want 2", calls)
	}
	if len(app.summaryQueued) != 0 || len(app.summaryRunning) != 0 || len(app.summaryForce) != 0 {
		t.Fatalf("queues not drained: queued=%#v running=%#v force=%#v", app.summaryQueued, app.summaryRunning, app.summaryForce)
	}
}

func TestValidateDownloadPathRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := dir + "/file.txt"
	link := dir + "/link.txt"
	if err := os.WriteFile(target, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := validateDownloadPath(link); err == nil {
		t.Fatal("validateDownloadPath accepted symlink")
	}
}
