package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/terminalshot"
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

func TestSnapshotStatusBoundsOversizedBrowserDiagnostic(t *testing.T) {
	app, _, _ := newSafetyApp(t, state.TerminalOriginCreated)
	retryAt := time.Date(2026, 7, 23, 1, 2, 8, 0, time.UTC)
	app.snapshotProbeError = "browser stderr prefix: " + strings.Repeat("π", 10_000)
	app.snapshotNextProbe = retryAt
	status := app.statusText()
	if len(status) > 4096 || !utf8.ValidString(status) {
		t.Fatalf("status is not Telegram-safe: bytes=%d valid_utf8=%v", len(status), utf8.ValidString(status))
	}
	for _, want := range []string{"browser stderr prefix:", "...; retry after " + retryAt.Format(time.RFC3339)} {
		if !strings.Contains(status, want) {
			t.Fatalf("bounded status omitted %q: %q", want, status)
		}
	}
}

func TestSnapshotRenderSamplesGenerationAfterWaitingForSlot(t *testing.T) {
	now := time.Date(2026, 7, 23, 1, 2, 3, 0, time.UTC)
	app := &App{snapshotReady: true, snapshotGeneration: 7, renderSlots: make(chan struct{}, 1)}
	app.renderSlots <- struct{}{}
	renderer := &failingSnapshotRenderer{}
	dir := t.TempDir()
	result := make(chan bool, 1)
	go func() {
		generation, ok := app.acquireSnapshotRender(context.Background())
		if !ok {
			result <- false
			return
		}
		_, err := renderer.Render(context.Background(), terminalshot.Input{}, dir)
		changed := app.markSnapshotsUnavailable(err, now, generation)
		releaseSlot(app.renderSlots)
		result <- changed
	}()
	select {
	case <-result:
		t.Fatal("render reservation did not wait for the occupied slot")
	case <-time.After(50 * time.Millisecond):
	}
	app.snapshotHealthMu.Lock()
	app.snapshotReady = true
	app.snapshotGeneration = 8
	app.snapshotHealthMu.Unlock()
	<-app.renderSlots
	if changed := <-result; !changed || app.snapshotAvailable() {
		t.Fatalf("current generation failure changed=%v available=%v", changed, app.snapshotAvailable())
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

func TestSnapshotRecoveryRefreshesOnlyExpandedWatchedSessions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		session state.TerminalSession
		want    bool
	}{
		{name: "expanded", session: state.TerminalSession{State: state.TerminalRunning, WatchEnabled: true}, want: true},
		{name: "collapsed", session: state.TerminalSession{State: state.TerminalRunning, WatchEnabled: true, Collapsed: true}},
		{name: "unwatched", session: state.TerminalSession{State: state.TerminalRunning}},
		{name: "lost", session: state.TerminalSession{State: state.TerminalLost, WatchEnabled: true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := snapshotRecoveryEligible(test.session); got != test.want {
				t.Fatalf("snapshotRecoveryEligible() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestSnapshotRuntimeFailureIsBrowserSpecificAndGenerationSafe(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 23, 1, 2, 3, 0, time.UTC)
	app := &App{snapshotReady: true, snapshotGeneration: 7}
	if app.markSnapshotsUnavailable(errors.New("artifact directory is read-only"), now, 7) {
		t.Fatal("request-local render failure changed browser health")
	}
	if !app.snapshotAvailable() {
		t.Fatal("request-local render failure disabled snapshots")
	}
	browserErr := &terminalshot.BrowserError{Err: errors.New("browser exited")}
	if !app.markSnapshotsUnavailable(browserErr, now, 7) {
		t.Fatal("browser failure did not change browser health")
	}
	_, _, _, _, retryAt := app.snapshotHealth()
	if app.markSnapshotsUnavailable(browserErr, now.Add(time.Second), 7) {
		t.Fatal("second failure compounded an unavailable epoch")
	}
	_, _, _, _, secondRetryAt := app.snapshotHealth()
	if !secondRetryAt.Equal(retryAt) {
		t.Fatalf("duplicate failure moved retry deadline: first=%s second=%s", retryAt, secondRetryAt)
	}

	app.snapshotHealthMu.Lock()
	app.snapshotReady = true
	app.snapshotGeneration = 8
	app.snapshotHealthMu.Unlock()
	if app.markSnapshotsUnavailable(browserErr, now.Add(2*time.Second), 7) {
		t.Fatal("stale render failure superseded a newer successful probe")
	}
}

type snapshotProbeFunc func(context.Context) (string, error)

func (fn snapshotProbeFunc) Probe(ctx context.Context) (string, error) { return fn(ctx) }
