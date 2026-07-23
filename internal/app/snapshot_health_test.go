package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type scriptedSnapshotProber struct {
	mu      sync.Mutex
	results []error
	calls   int
}

func (p *scriptedSnapshotProber) Probe(context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if len(p.results) == 0 {
		return "/usr/bin/chromium", nil
	}
	err := p.results[0]
	p.results = p.results[1:]
	if err != nil {
		return "", err
	}
	return "/usr/bin/chromium", nil
}

func (p *scriptedSnapshotProber) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func TestSnapshotHealthRetriesStartupFailureAndRecovers(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 23, 1, 2, 3, 0, time.UTC)
	prober := &scriptedSnapshotProber{results: []error{nil}}
	app := &App{
		SnapshotProber:        prober,
		snapshotProbeError:    "browser pipe remained open",
		snapshotProbeAt:       now,
		snapshotNextProbe:     now.Add(snapshotProbeInitialDelay),
		snapshotProbeFailures: 1,
		snapshotNow:           func() time.Time { return now },
	}

	if app.recoverSnapshots(context.Background(), now.Add(snapshotProbeInitialDelay-time.Millisecond)) {
		t.Fatal("snapshot recovery ignored retry deadline")
	}
	if got := prober.callCount(); got != 0 {
		t.Fatalf("probe calls before deadline = %d, want 0", got)
	}
	if !app.recoverSnapshots(context.Background(), now.Add(snapshotProbeInitialDelay)) {
		t.Fatal("snapshot recovery did not report restored availability")
	}
	if got := prober.callCount(); got != 1 {
		t.Fatalf("probe calls = %d, want 1", got)
	}
	if !app.snapshotAvailable() {
		t.Fatal("snapshot renderer remained unavailable after successful probe")
	}
	if got := app.snapshotStatus(); !strings.HasPrefix(got, "ready") {
		t.Fatalf("snapshot status = %q, want ready", got)
	}
}

func TestSnapshotHealthReportsFailureAndBacksOff(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 23, 1, 2, 3, 0, time.UTC)
	prober := &scriptedSnapshotProber{results: []error{errors.New("context deadline exceeded"), nil}}
	app := &App{SnapshotProber: prober, snapshotNow: func() time.Time { return now }}

	if app.recoverSnapshots(context.Background(), now) {
		t.Fatal("failed probe reported recovery")
	}
	status := app.snapshotStatus()
	for _, want := range []string{"unavailable", "context deadline exceeded", "retry"} {
		if !strings.Contains(status, want) {
			t.Fatalf("snapshot status %q missing %q", status, want)
		}
	}
	if app.recoverSnapshots(context.Background(), now.Add(snapshotProbeInitialDelay-time.Millisecond)) {
		t.Fatal("snapshot recovery ignored failure backoff")
	}
	if got := prober.callCount(); got != 1 {
		t.Fatalf("probe calls during backoff = %d, want 1", got)
	}
	if !app.recoverSnapshots(context.Background(), now.Add(snapshotProbeInitialDelay)) {
		t.Fatal("snapshot recovery did not retry after backoff")
	}
}

func TestSnapshotHealthAllowsOnlyOneConcurrentProbe(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	prober := snapshotProbeFunc(func(context.Context) (string, error) {
		close(started)
		<-release
		return "/usr/bin/chromium", nil
	})
	now := time.Now()
	app := &App{SnapshotProber: prober, snapshotNow: func() time.Time { return now }}
	done := make(chan bool, 1)
	go func() { done <- app.recoverSnapshots(context.Background(), now) }()
	<-started
	if app.recoverSnapshots(context.Background(), now) {
		t.Fatal("concurrent caller reported a second recovery")
	}
	close(release)
	if !<-done {
		t.Fatal("single in-flight probe did not recover")
	}
}

type snapshotProbeFunc func(context.Context) (string, error)

func (fn snapshotProbeFunc) Probe(ctx context.Context) (string, error) { return fn(ctx) }
