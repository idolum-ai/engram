package codexui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type fakeCommandRunner struct {
	ps    string
	calls [][]string
}

func (f *fakeCommandRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if name == "ps" {
		return f.ps, nil
	}
	return "", fmt.Errorf("unexpected command %s %v", name, args)
}

type fakeVersionResolver struct {
	version string
	calls   []string
}

func (f *fakeVersionResolver) Resolve(executable string) (string, error) {
	f.calls = append(f.calls, executable)
	return f.version, nil
}

type selectiveVersionResolver struct {
	versions map[string]string
	errors   map[string]error
	calls    []string
}

func (f *selectiveVersionResolver) Resolve(executable string) (string, error) {
	f.calls = append(f.calls, executable)
	if err := f.errors[executable]; err != nil {
		return "", err
	}
	version, ok := f.versions[executable]
	if !ok {
		return "", fmt.Errorf("unexpected executable %s", executable)
	}
	return version, nil
}

func TestDetectorFindsCodexBelowTmuxPaneAndRevalidatesVersion(t *testing.T) {
	runner := &fakeCommandRunner{
		ps: stringsJoinLines(
			"100 1 bash -bash",
			"110 100 node node /opt/codex/bin/codex resume session-id",
			"120 110 codex /opt/codex/bin/codex resume session-id",
			"130 120 codex-code-mode-host /opt/codex/bin/codex-code-mode-host",
			"900 1 codex /elsewhere/codex"),
	}
	versions := &fakeVersionResolver{version: SupportedVersion}
	detector := &Detector{Runner: runner, Versions: versions}
	got, err := detector.Detect(context.Background(), 100, "node")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Detected || !got.Supported || got.Version != SupportedVersion {
		t.Fatalf("runtime = %#v", got)
	}
	if _, err := detector.Detect(context.Background(), 100, "node"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(versions.calls, []string{"/opt/codex/bin/codex", "/opt/codex/bin/codex"}) {
		t.Fatalf("version resolver calls = %#v", versions.calls)
	}
}

func TestDetectorPrefersNearestPackageLauncherOverNativeDescendantRegardlessOfProcessOrder(t *testing.T) {
	const (
		launcher = "/opt/homebrew/bin/codex"
		native   = "/opt/homebrew/lib/node_modules/@openai/codex/node_modules/@openai/codex-darwin-arm64/vendor/aarch64-apple-darwin/bin/codex"
	)
	runner := &fakeCommandRunner{ps: stringsJoinLines(
		"100 1 node node",
		"120 110 codex "+native,
		"110 100 node node "+launcher,
	)}
	versions := &selectiveVersionResolver{
		versions: map[string]string{launcher: SupportedVersion},
		errors:   map[string]error{native: fmt.Errorf("@openai/codex package metadata not found")},
	}
	detector := &Detector{Runner: runner, Versions: versions}
	got, err := detector.Detect(context.Background(), 100, "node")
	if err != nil || !got.Detected || !got.Supported || got.Version != SupportedVersion {
		t.Fatalf("runtime = %#v, err=%v", got, err)
	}
	if !reflect.DeepEqual(versions.calls, []string{launcher}) {
		t.Fatalf("version resolver calls = %#v", versions.calls)
	}
}

func TestDetectorFailsClosedWhenNearestCandidateHasNoPackageMetadata(t *testing.T) {
	const (
		unresolved = "/opt/custom/bin/codex"
		resolved   = "/opt/custom/libexec/codex"
	)
	runner := &fakeCommandRunner{ps: stringsJoinLines(
		"100 1 node node",
		"110 100 node node "+unresolved,
		"120 110 codex "+resolved,
	)}
	versions := &selectiveVersionResolver{
		versions: map[string]string{resolved: SupportedVersion},
		errors:   map[string]error{unresolved: fmt.Errorf("@openai/codex package metadata not found")},
	}
	detector := &Detector{Runner: runner, Versions: versions}
	got, err := detector.Detect(context.Background(), 100, "node")
	if err == nil || !got.Detected || got.Supported || got.Version != "" {
		t.Fatalf("runtime = %#v, err=%v", got, err)
	}
	if !reflect.DeepEqual(versions.calls, []string{unresolved}) {
		t.Fatalf("version resolver calls = %#v", versions.calls)
	}
}

func TestDetectorFallsBackForUnsupportedVersionAndUnrelatedProcess(t *testing.T) {
	runner := &fakeCommandRunner{
		ps: stringsJoinLines(
			"100 1 bash -bash",
			"110 100 node node /opt/codex/bin/codex",
			"200 1 node node server.js"),
	}
	detector := &Detector{Runner: runner, Versions: &fakeVersionResolver{version: "0.144.5"}}
	got, err := detector.Detect(context.Background(), 100, "node")
	if err != nil || !got.Detected || got.Supported || got.Version != "0.144.5" {
		t.Fatalf("unsupported runtime = %#v, err=%v", got, err)
	}
	got, err = detector.Detect(context.Background(), 200, "node")
	if err != nil || got.Detected {
		t.Fatalf("unrelated runtime = %#v, err=%v", got, err)
	}
	before := len(runner.calls)
	got, err = detector.Detect(context.Background(), 100, "bash")
	if err != nil || got.Detected || len(runner.calls) != before {
		t.Fatalf("shell foreground runtime = %#v, err=%v calls=%#v", got, err, runner.calls)
	}
}

func TestDetectorRejectsUnsupportedRelaunchAtSameExecutable(t *testing.T) {
	runner := &fakeCommandRunner{ps: stringsJoinLines(
		"100 1 bash -bash",
		"110 100 node node /opt/codex/bin/codex resume old-session",
	)}
	versions := &fakeVersionResolver{version: SupportedVersion}
	detector := &Detector{Runner: runner, Versions: versions}
	first, err := detector.Detect(context.Background(), 100, "node")
	if err != nil || !first.Supported {
		t.Fatalf("first runtime = %#v, err=%v", first, err)
	}
	runner.ps = stringsJoinLines(
		"100 1 bash -bash",
		"210 100 node node /opt/codex/bin/codex resume new-session",
	)
	versions.version = "0.145.0"
	second, err := detector.Detect(context.Background(), 100, "node")
	if err != nil || !second.Detected || second.Supported || second.Version != "0.145.0" {
		t.Fatalf("replacement runtime = %#v, err=%v", second, err)
	}
	if len(versions.calls) != 2 {
		t.Fatalf("version resolver calls = %#v", versions.calls)
	}
}

func TestCodexExecutableRejectsRelativeLookalikes(t *testing.T) {
	if got := codexExecutable(process{comm: "node", args: "node echo codex"}); got != "" {
		t.Fatalf("relative lookalike executable = %q", got)
	}
	if got := codexExecutable(process{comm: "codex", args: "codex resume id"}); got != "" {
		t.Fatalf("relative process executable = %q", got)
	}
}

func stringsJoinLines(lines ...string) string {
	var out string
	for _, line := range lines {
		out += line + "\n"
	}
	return out
}

func TestPackageVersionResolverReadsOnlyOpenAICodexMetadata(t *testing.T) {
	root := t.TempDir()
	packageDir := filepath.Join(root, "lib", "node_modules", "@openai", "codex")
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(filepath.Join(packageDir, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(packageDir, "bin", "codex.js")
	if err := os.WriteFile(launcher, []byte("#!/usr/bin/env node\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageDir, "package.json"), []byte(`{"name":"@openai/codex","version":"0.144.6"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(binDir, "codex")
	if err := os.Symlink(launcher, link); err != nil {
		t.Fatal(err)
	}
	got, err := (PackageVersionResolver{}).Resolve(link)
	if err != nil || got != SupportedVersion {
		t.Fatalf("Resolve() = %q, %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(packageDir, "package.json"), []byte(`{"name":"lookalike","version":"0.144.6"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (PackageVersionResolver{}).Resolve(link); err == nil {
		t.Fatal("resolver accepted non-OpenAI package metadata")
	}
}
