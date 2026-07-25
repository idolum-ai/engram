package claudeui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

type fakeRunner struct {
	processes string
	started   string
	err       error
	calls     [][]string
}

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if r.err != nil {
		return "", r.err
	}
	if len(args) >= 2 && args[0] == "-axo" {
		return r.processes, nil
	}
	return r.started, nil
}

type fakeVersionResolver struct {
	version string
	err     error
	calls   []string
}

type fakeExecutableResolver struct {
	path  string
	err   error
	calls []int
}

func (r *fakeExecutableResolver) Resolve(pid int) (string, error) {
	r.calls = append(r.calls, pid)
	return r.path, r.err
}

func (r *fakeVersionResolver) Resolve(path string) (string, error) {
	r.calls = append(r.calls, path)
	return r.version, r.err
}

func TestDetectorResolvesRelativeClaudeProcessToRunningExecutable(t *testing.T) {
	const executable = "/home/example/.local/share/claude/versions/2.1.219"
	runner := &fakeRunner{
		processes: "100 1 bash -bash\n110 100 claude claude --dangerously-skip-permissions\n",
		started:   "Thu Jul 24 22:11:23 2026\n",
	}
	executables := &fakeExecutableResolver{path: executable}
	detector := &Detector{
		Runner: runner, Executables: executables,
		Versions: &fakeVersionResolver{version: SupportedVersion},
	}
	got, err := detector.Detect(context.Background(), 100, "claude")
	if err != nil || !got.Supported || len(got.Identity) != 64 || !reflect.DeepEqual(executables.calls, []int{110}) {
		t.Fatalf("runtime = %#v, executable calls=%#v err=%v", got, executables.calls, err)
	}
}

func TestDetectorIdentifiesSupportedVersionedClaudeProcess(t *testing.T) {
	const executable = "/home/example/.local/share/claude/versions/2.1.219"
	runner := &fakeRunner{
		processes: strings.Join([]string{
			"100 1 bash -bash",
			"110 100 2.1.219 " + executable,
			"120 110 helper /opt/helper",
			"",
		}, "\n"),
		started: "Thu Jul 24 22:11:23 2026\n",
	}
	versions := &fakeVersionResolver{version: SupportedVersion}
	detector := &Detector{Runner: runner, Versions: versions}

	got, err := detector.Detect(context.Background(), 100, "claude")
	if err != nil || !got.Detected || !got.Supported || got.Version != SupportedVersion || len(got.Identity) != 64 {
		t.Fatalf("runtime = %#v, err=%v", got, err)
	}
	if !reflect.DeepEqual(versions.calls, []string{executable}) {
		t.Fatalf("version calls = %#v", versions.calls)
	}
	if len(runner.calls) != 2 || !reflect.DeepEqual(runner.calls[1], []string{"ps", "-o", "lstart=", "-p", "110"}) {
		t.Fatalf("runner calls = %#v", runner.calls)
	}
}

func TestDetectorProcessIdentityChangesAcrossRelaunch(t *testing.T) {
	const executable = "/home/example/.local/share/claude/versions/2.1.219"
	runner := &fakeRunner{processes: "100 1 bash -bash\n110 100 2.1.219 " + executable + "\n", started: "Thu Jul 24 22:11:23 2026\n"}
	detector := &Detector{Runner: runner, Versions: &fakeVersionResolver{version: SupportedVersion}}
	first, err := detector.Detect(context.Background(), 100, "claude")
	if err != nil {
		t.Fatal(err)
	}
	runner.processes = "100 1 bash -bash\n210 100 2.1.219 " + executable + "\n"
	runner.started = "Fri Jul 25 09:00:00 2026\n"
	second, err := detector.Detect(context.Background(), 100, "claude")
	if err != nil || first.Identity == second.Identity {
		t.Fatalf("first=%#v second=%#v err=%v", first, second, err)
	}
}

func TestDetectorFailsClosedForAmbiguousUnsupportedAndUnrelatedProcesses(t *testing.T) {
	const executable = "/home/example/.local/share/claude/versions/2.1.220"
	tests := []struct {
		name       string
		processes  string
		foreground string
		version    string
		detected   bool
	}{
		{
			name: "ambiguous nearest",
			processes: "100 1 bash -bash\n110 100 2.1.219 /a/claude/versions/2.1.219\n" +
				"120 100 2.1.219 /b/claude/versions/2.1.219\n",
			foreground: "claude", version: SupportedVersion, detected: true,
		},
		{
			name:       "unsupported",
			processes:  "100 1 bash -bash\n110 100 2.1.220 " + executable + "\n",
			foreground: "claude", version: "2.1.220", detected: true,
		},
		{
			name:       "unrelated foreground",
			processes:  "100 1 bash -bash\n110 100 2.1.219 /a/claude/versions/2.1.219\n",
			foreground: "vim", version: SupportedVersion,
		},
		{
			name:       "unrelated process tree",
			processes:  "100 1 bash -bash\n210 200 2.1.219 /a/claude/versions/2.1.219\n",
			foreground: "claude", version: SupportedVersion,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeRunner{processes: test.processes, started: "Thu Jul 24 22:11:23 2026\n"}
			versions := &fakeVersionResolver{version: test.version}
			got, err := (&Detector{Runner: runner, Executables: &fakeExecutableResolver{}, Versions: versions}).Detect(context.Background(), 100, test.foreground)
			if err != nil || got.Detected != test.detected {
				t.Fatalf("runtime = %#v, err=%v", got, err)
			}
			if test.name == "ambiguous nearest" && (got.Supported || len(versions.calls) != 0) {
				t.Fatalf("ambiguous runtime = %#v, version calls=%#v", got, versions.calls)
			}
			if test.name == "unsupported" && (got.Supported || got.Version != "2.1.220") {
				t.Fatalf("unsupported runtime = %#v", got)
			}
		})
	}
}

func TestDetectorFailsClosedWhenStartTimeOrVersionCannotBeRead(t *testing.T) {
	const executable = "/home/example/.local/share/claude/versions/2.1.219"
	runner := &fakeRunner{processes: "100 1 bash -bash\n110 100 2.1.219 " + executable + "\n"}
	detector := &Detector{Runner: runner, Executables: &fakeExecutableResolver{}, Versions: &fakeVersionResolver{version: SupportedVersion}}
	got, err := detector.Detect(context.Background(), 100, "claude")
	if err == nil || !got.Detected || got.Identity != "" {
		t.Fatalf("empty start runtime = %#v, err=%v", got, err)
	}
	detector.Versions = &fakeVersionResolver{err: errors.New("missing metadata")}
	got, err = detector.Detect(context.Background(), 100, "claude")
	if err == nil || !got.Detected {
		t.Fatalf("version failure runtime = %#v, err=%v", got, err)
	}
}

func TestPathVersionResolverUsesResolvedVersionedInstallation(t *testing.T) {
	root := t.TempDir()
	versionDir := filepath.Join(root, "share", "claude", "versions")
	if err := os.MkdirAll(versionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(versionDir, SupportedVersion)
	if err := os.WriteFile(executable, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(binDir, "claude")
	if err := os.Symlink(executable, launcher); err != nil {
		t.Fatal(err)
	}
	got, err := (PathVersionResolver{}).Resolve(launcher)
	if err != nil || got != SupportedVersion {
		t.Fatalf("version = %q, err=%v", got, err)
	}
}

func TestClaudeExecutableRejectsRelativeAndPathLookalikes(t *testing.T) {
	for _, process := range []process{
		{comm: "claude", args: "claude"},
		{comm: "/tmp/2.1.219", args: "/tmp/2.1.219"},
		{comm: "/tmp/claude/versions/not-a-version", args: "/tmp/claude/versions/not-a-version"},
	} {
		if got := claudeExecutable(process); got != "" {
			t.Fatalf("lookalike executable = %q for %#v", got, process)
		}
	}
}

func TestDetectorAgainstRunningClaudeProcess(t *testing.T) {
	value := os.Getenv("ENGRAM_CLAUDEUI_INTEGRATION_PID")
	if value == "" {
		t.Skip("set ENGRAM_CLAUDEUI_INTEGRATION_PID to a tmux pane root PID")
	}
	pid, err := strconv.Atoi(value)
	if err != nil || pid <= 0 {
		t.Fatalf("invalid ENGRAM_CLAUDEUI_INTEGRATION_PID %q", value)
	}
	got, err := NewDetector().Detect(context.Background(), pid, "claude")
	if err != nil || !got.Detected || !got.Supported || got.Version != SupportedVersion || len(got.Identity) != 64 {
		t.Fatalf("live runtime = %#v, err=%v", got, err)
	}
}
