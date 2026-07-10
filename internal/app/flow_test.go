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
	app.refreshWG.Wait()

	app.summaryMu.Lock()
	defer app.summaryMu.Unlock()
	if calls != 2 {
		t.Fatalf("refresh calls = %d, want 2", calls)
	}
	if len(app.summaryQueued) != 0 || len(app.summaryRunning) != 0 || len(app.summaryForce) != 0 {
		t.Fatalf("queues not drained: queued=%#v running=%#v force=%#v", app.summaryQueued, app.summaryRunning, app.summaryForce)
	}
}

func TestQueueRefreshMovesQuietDeadlineAfterEachInput(t *testing.T) {
	app := &App{
		summaryQueued:  map[int]bool{7: true},
		summaryRunning: map[int]bool{},
		summaryForce:   map[int]bool{},
		summaryDue:     map[int]time.Time{7: time.Now()},
	}
	app.queueRefresh(7, true, summaryQuietPeriod)
	first := app.summaryDue[7]
	time.Sleep(time.Millisecond)
	app.queueRefresh(7, true, summaryQuietPeriod)
	second := app.summaryDue[7]
	if !second.After(first) {
		t.Fatalf("quiet deadline did not move: first=%s second=%s", first, second)
	}
	app.queueRefresh(7, true, 0)
	if !app.summaryDue[7].Before(second) {
		t.Fatalf("manual refresh did not bypass quiet deadline: due=%s previous=%s", app.summaryDue[7], second)
	}
}

func TestQueuedRefreshStopsWhenServiceContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	app := &App{
		runCtx:         ctx,
		summaryQueued:  map[int]bool{},
		summaryRunning: map[int]bool{},
		summaryForce:   map[int]bool{},
	}
	called := make(chan struct{}, 1)
	app.refreshHook = func(context.Context, int, bool) {
		called <- struct{}{}
	}

	app.queueRefresh(7, true, time.Hour)
	cancel()
	done := make(chan struct{})
	go func() {
		app.refreshWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("refresh worker did not stop after cancellation")
	}
	select {
	case <-called:
		t.Fatal("refresh ran after service cancellation")
	default:
	}

	app.summaryMu.Lock()
	defer app.summaryMu.Unlock()
	if len(app.summaryQueued) != 0 || len(app.summaryRunning) != 0 || len(app.summaryForce) != 0 {
		t.Fatalf("queues not cleared after cancellation: queued=%#v running=%#v force=%#v", app.summaryQueued, app.summaryRunning, app.summaryForce)
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
