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

func TestDetectorFindsCodexBelowTmuxPaneAndCachesVersion(t *testing.T) {
	runner := &fakeCommandRunner{
		ps: stringsJoinLines(
			"100 1 bash -bash",
			"110 100 node node /opt/codex/bin/codex resume session-id",
			"120 110 codex /opt/codex/bin/codex resume session-id",
			"130 120 codex-code-mode-host /opt/codex/bin/codex-code-mode-host",
			"900 1 codex /elsewhere/codex"),
	}
	versions := &fakeVersionResolver{version: SupportedVersion}
	detector := &Detector{Runner: runner, Versions: versions, cache: make(map[string]Runtime)}
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
	if !reflect.DeepEqual(versions.calls, []string{"/opt/codex/bin/codex"}) {
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
	detector := &Detector{Runner: runner, Versions: &fakeVersionResolver{version: "0.145.0"}, cache: make(map[string]Runtime)}
	got, err := detector.Detect(context.Background(), 100, "node")
	if err != nil || !got.Detected || got.Supported || got.Version != "0.145.0" {
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
