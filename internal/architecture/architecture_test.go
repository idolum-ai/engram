package architecture

import (
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDarwinServiceInstallerValidatesWithoutImplicitActivation(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "fake-bin")
	if err := os.Mkdir(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	callLog := filepath.Join(root, "calls")
	writeExecutable := func(name, body string) string {
		t.Helper()
		path := filepath.Join(binDir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	writeExecutable("uname", `echo Darwin`)
	writeExecutable("plutil", `echo "plutil $*" >>"$SERVICE_CALL_LOG"`)
	writeExecutable("tmux", `exit 0`)
	writeExecutable("launchctl", `echo "launchctl $*" >>"$SERVICE_CALL_LOG"
if [ "${1:-}" = bootout ]; then
  rm -f "$SERVICE_STATE"
  exit 0
fi
if [ "${1:-}" = bootstrap ]; then
  grep -q ENGRAM_SERVICE_STATUS_FILE "${3:-}" || exit 3
  touch "$SERVICE_STATE"
  printf 'pid=4242\nbuild=Engram v10 running-build\n' >"$HOME/.engram/service.identity"
  exit 0
fi
if [ "${1:-}" = print ] && [ -f "$SERVICE_STATE" ]; then
  echo '  state = running'
  echo '  pid = 4242'
  exit 0
fi
exit 1`)
	binary := writeExecutable("engram", `if [ "${1:-}" = version ]; then echo 'Engram v9 test-build'; else exit 2; fi`)
	envPath := filepath.Join(root, ".engram", ".env")
	if err := os.MkdirAll(filepath.Dir(envPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envPath, []byte("TELEGRAM_BOT_TOKEN=not-read\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(repoRoot(t), "scripts", "user-service.sh")
	run := func(action string) {
		t.Helper()
		command := exec.Command("bash", script, action, binary, envPath)
		command.Env = append(os.Environ(), "HOME="+root, "PATH="+binDir+":"+os.Getenv("PATH"), "SERVICE_CALL_LOG="+callLog, "SERVICE_STATE="+filepath.Join(root, "launchctl.state"))
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", action, err, output)
		}
	}
	run("install")
	calls, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(calls), "plutil -lint") || strings.Contains(string(calls), "launchctl") {
		t.Fatalf("install calls = %s", calls)
	}
	plistPath := filepath.Join(root, "Library", "LaunchAgents", "ai.idolum.engram.plist")
	plist, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"ENGRAM_SERVICE_STATUS_FILE", "service.identity", "AbandonProcessGroup", binary, envPath} {
		if !strings.Contains(string(plist), required) {
			t.Fatalf("plist omitted %q:\n%s", required, plist)
		}
	}
	if !strings.Contains(string(plist), "<key>PATH</key>") || !strings.Contains(string(plist), binDir) {
		t.Fatalf("plist omitted discovered tmux PATH: %s", plist)
	}
	legacyPlist := `<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>Label</key><string>ai.idolum.engram</string></dict></plist>`
	if err := os.WriteFile(plistPath, []byte(legacyPlist), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nif [ \"${1:-}\" = version ]; then echo 'Engram v10 replacement'; else exit 2; fi\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	run("restart")
	calls, err = os.ReadFile(callLog)
	if err != nil || !strings.Contains(string(calls), "launchctl bootout") || !strings.Contains(string(calls), "launchctl bootstrap") {
		t.Fatalf("restart calls = %s err=%v", calls, err)
	}
	refreshed, err := os.ReadFile(plistPath)
	if err != nil || !strings.Contains(string(refreshed), "ENGRAM_SERVICE_STATUS_FILE") {
		t.Fatalf("restart did not migrate legacy plist: err=%v\n%s", err, refreshed)
	}
	command := exec.Command("bash", script, "status", binary, envPath)
	command.Env = append(os.Environ(), "HOME="+root, "PATH="+binDir+":"+os.Getenv("PATH"), "SERVICE_CALL_LOG="+callLog, "SERVICE_STATE="+filepath.Join(root, "launchctl.state"))
	output, err := command.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "running build = Engram v10 running-build") || strings.Contains(string(output), "v9 test-build") {
		t.Fatalf("status did not identify running build: err=%v\n%s", err, output)
	}
}

func TestLinuxServiceRestartMigratesLegacyUnitAndUsesRunningIdentity(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "fake-bin")
	if err := os.Mkdir(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeExecutable := func(name, body string) string {
		t.Helper()
		path := filepath.Join(binDir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	writeExecutable("uname", `echo Linux`)
	writeExecutable("tmux", `exit 0`)
	writeExecutable("systemctl", `case "${2:-}" in
  daemon-reload)
    grep -q ENGRAM_SERVICE_STATUS_FILE "$HOME/.config/systemd/user/engram.service" || exit 3
    ;;
  restart)
    grep -q ENGRAM_SERVICE_STATUS_FILE "$HOME/.config/systemd/user/engram.service" || exit 3
    printf 'pid=5252\nbuild=engram v11 live-linux\n' >"$HOME/.engram/service.identity"
    ;;
  status) echo 'active (running)' ;;
  show) echo 5252 ;;
esac`)
	binary := writeExecutable("engram", `if [ "${1:-}" = version ]; then echo 'installed-build-must-not-be-used'; else exit 2; fi`)
	identityDir := filepath.Join(root, ".engram")
	if err := os.MkdirAll(identityDir, 0o700); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(identityDir, ".env")
	if err := os.WriteFile(envPath, []byte("TELEGRAM_BOT_TOKEN=not-read\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	unitDir := filepath.Join(root, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	unitPath := filepath.Join(unitDir, "engram.service")
	if err := os.WriteFile(unitPath, []byte("[Service]\nExecStart="+binary+" run --env "+envPath+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(repoRoot(t), "scripts", "user-service.sh")
	command := exec.Command("bash", script, "restart", binary, envPath)
	command.Env = append(os.Environ(), "HOME="+root, "PATH="+binDir+":"+os.Getenv("PATH"))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("restart did not migrate legacy Linux unit: %v\n%s", err, output)
	}
	unit, err := os.ReadFile(unitPath)
	if err != nil || !strings.Contains(string(unit), "ENGRAM_SERVICE_STATUS_FILE") || !strings.Contains(string(unit), "PATH=") {
		t.Fatalf("restart did not refresh Linux unit: err=%v\n%s", err, unit)
	}
	command = exec.Command("bash", script, "status", binary, envPath)
	command.Env = append(os.Environ(), "HOME="+root, "PATH="+binDir+":"+os.Getenv("PATH"))
	output, err := command.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "running build = engram v11 live-linux") || strings.Contains(string(output), "installed-build-must-not-be-used") {
		t.Fatalf("Linux status did not identify running build: err=%v\n%s", err, output)
	}
}

func TestPackageImportBoundaries(t *testing.T) {
	t.Parallel()

	rules := []struct {
		dir       string
		forbidden []string
	}{
		{
			dir: "internal/telegram",
			forbidden: []string{
				"github.com/idolum-ai/engram/internal/app",
				"github.com/idolum-ai/engram/internal/anthropic",
				"github.com/idolum-ai/engram/internal/guide",
				"github.com/idolum-ai/engram/internal/openai",
				"github.com/idolum-ai/engram/internal/state",
				"github.com/idolum-ai/engram/internal/tmux",
			},
		},
		{
			dir: "internal/tmux",
			forbidden: []string{
				"github.com/idolum-ai/engram/internal/app",
				"github.com/idolum-ai/engram/internal/telegram",
				"github.com/idolum-ai/engram/internal/state",
			},
		},
		{
			dir: "internal/anthropic",
			forbidden: []string{
				"github.com/idolum-ai/engram/internal/app",
				"github.com/idolum-ai/engram/internal/telegram",
				"github.com/idolum-ai/engram/internal/state",
				"github.com/idolum-ai/engram/internal/tmux",
			},
		},
		{
			dir: "internal/openai",
			forbidden: []string{
				"github.com/idolum-ai/engram/internal/app",
				"github.com/idolum-ai/engram/internal/telegram",
				"github.com/idolum-ai/engram/internal/state",
				"github.com/idolum-ai/engram/internal/tmux",
			},
		},
		{
			dir: "internal/guide",
			forbidden: []string{
				"github.com/idolum-ai/engram/internal/anthropic",
				"github.com/idolum-ai/engram/internal/app",
				"github.com/idolum-ai/engram/internal/openai",
				"github.com/idolum-ai/engram/internal/state",
				"github.com/idolum-ai/engram/internal/telegram",
				"github.com/idolum-ai/engram/internal/tmux",
			},
		},
		{
			dir: "internal/keyseq",
			forbidden: []string{
				"github.com/idolum-ai/engram/internal/anthropic",
				"github.com/idolum-ai/engram/internal/app",
				"github.com/idolum-ai/engram/internal/guide",
				"github.com/idolum-ai/engram/internal/openai",
				"github.com/idolum-ai/engram/internal/state",
				"github.com/idolum-ai/engram/internal/telegram",
				"github.com/idolum-ai/engram/internal/tmux",
			},
		},
		{
			dir: "internal/commands",
			forbidden: []string{
				"github.com/idolum-ai/engram/internal/app",
				"github.com/idolum-ai/engram/internal/telegram",
				"github.com/idolum-ai/engram/internal/state",
				"github.com/idolum-ai/engram/internal/tmux",
			},
		},
		{
			dir: "internal/terminalshot",
			forbidden: []string{
				"github.com/idolum-ai/engram/internal/app",
				"github.com/idolum-ai/engram/internal/telegram",
				"github.com/idolum-ai/engram/internal/state",
				"github.com/idolum-ai/engram/internal/tmux",
			},
		},
		{
			dir: "internal/mechanics",
			forbidden: []string{
				"github.com/idolum-ai/engram/internal/app",
				"github.com/idolum-ai/engram/internal/anthropic",
				"github.com/idolum-ai/engram/internal/guide",
				"github.com/idolum-ai/engram/internal/inspect",
				"github.com/idolum-ai/engram/internal/openai",
				"github.com/idolum-ai/engram/internal/state",
				"github.com/idolum-ai/engram/internal/telegram",
				"github.com/idolum-ai/engram/internal/terminalshot",
			},
		},
		{
			dir: "internal/inspect",
			forbidden: []string{
				"github.com/idolum-ai/engram/internal/app",
				"github.com/idolum-ai/engram/internal/anthropic",
				"github.com/idolum-ai/engram/internal/guide",
				"github.com/idolum-ai/engram/internal/openai",
				"github.com/idolum-ai/engram/internal/telegram",
				"github.com/idolum-ai/engram/internal/terminalshot",
			},
		},
	}

	root := repoRoot(t)
	for _, rule := range rules {
		rule := rule
		t.Run(rule.dir, func(t *testing.T) {
			t.Parallel()
			forbidden := map[string]bool{}
			for _, imp := range rule.forbidden {
				forbidden[imp] = true
			}
			assertNoForbiddenImports(t, filepath.Join(root, rule.dir), forbidden)
		})
	}
}

func TestUserServiceLifecycleContracts(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "user-service.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, required := range []string{
		"ai.idolum.engram", "plutil -lint", "launchctl bootstrap", "launchctl bootout",
		"AbandonProcessGroup", "KillMode=process", "ENGRAM_SERVICE_STATUS_FILE", "service.identity", "service_path", "lines <= 1000",
		"systemctl --user enable --now engram.service",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("service lifecycle omits %q", required)
		}
	}
	installStart := strings.Index(text, "install_darwin()")
	installEnd := strings.Index(text, "install_linux()")
	if installStart < 0 || installEnd < installStart || strings.Contains(text[installStart:installEnd], "launchctl bootstrap") {
		t.Fatal("Darwin installation must not implicitly activate or restart the service")
	}
}

func assertNoForbiddenImports(t *testing.T, dir string, forbidden map[string]bool) {
	t.Helper()

	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no Go files in %s", dir)
	}
	fset := token.NewFileSet()
	for _, path := range files {
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse imports %s: %v", path, err)
		}
		for _, spec := range file.Imports {
			imp := strings.Trim(spec.Path.Value, "\"")
			if forbidden[imp] {
				t.Fatalf("%s imports forbidden package %s", path, imp)
			}
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(wd, "go.mod")); err == nil {
			return wd
		}
		parent := filepath.Dir(wd)
		if parent == wd {
			t.Fatal("could not find repo root")
		}
		wd = parent
	}
}
