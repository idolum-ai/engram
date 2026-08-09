package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestArtifactDirPrefersPrivateXDGRunTimeDir(t *testing.T) {
	runtimeDir := filepath.Join(canonicalTestTempDir(t), "runtime")
	if err := os.Mkdir(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "unused-tmp"))

	cfg := Config{Home: filepath.Join(t.TempDir(), "home")}
	if got, want := cfg.ArtifactDir(), filepath.Join(runtimeDir, "engram"); got != want {
		t.Fatalf("ArtifactDir = %q, want %q", got, want)
	}
	if err := EnsureDirs(cfg); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{cfg.ArtifactDir(), cfg.AttachmentDir()} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("private dir %s has mode %v", path, info.Mode())
		}
	}
}

func TestLoadAgentOptionsReadsNoCredentialsAndEnforcesBounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("TELEGRAM_BOT_TOKEN=private\nOPENAI_API_KEY=private-too\nENGRAM_CODEX_CONTEXT_TURNS=3\nENGRAM_CLAUDE_CONTEXT_TURNS=4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadAgentOptions(path)
	if err != nil || got.CodexContextTurns != 3 || got.ClaudeContextTurns != 4 {
		t.Fatalf("options=%#v err=%v", got, err)
	}
	if strings.Contains(fmt.Sprintf("%#v", got), "private") {
		t.Fatalf("options retained credentials: %#v", got)
	}
	if err := os.WriteFile(path, []byte("ENGRAM_CLAUDE_CONTEXT_TURNS=9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAgentOptions(path); err == nil {
		t.Fatal("out-of-range context option accepted")
	}
}

func TestGitHubBrokerSocketPathIsStableAndInstanceScoped(t *testing.T) {
	runtimeDir := filepath.Join(canonicalTestTempDir(t), "runtime")
	if err := os.Mkdir(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	first := Config{
		Home:                  "/home/example/.engram-one",
		TelegramBotToken:      "token-one",
		TelegramAllowedUserID: 42,
		TelegramChatID:        42,
	}
	same := first
	same.TelegramOperatorUserIDs = []int64{77, 88}
	otherHome := first
	otherHome.Home = "/home/example/.engram-two"
	otherTelegram := first
	otherTelegram.TelegramBotToken = "token-two"

	if first.GitHubBrokerSocketPath() != same.GitHubBrokerSocketPath() {
		t.Fatal("same Engram instance produced unstable GitHub broker socket paths")
	}
	if first.GitHubBrokerSocketPath() == otherHome.GitHubBrokerSocketPath() {
		t.Fatal("different ENGRAM_HOME values shared a GitHub broker socket")
	}
	if first.GitHubBrokerSocketPath() == otherTelegram.GitHubBrokerSocketPath() {
		t.Fatal("different Telegram identities shared a GitHub broker socket")
	}
	if strings.Contains(first.GitHubBrokerSocketPath(), first.TelegramBotToken) {
		t.Fatal("GitHub broker socket path exposed the Telegram bot token")
	}
}

func TestLoadCanonicalizesTelegramOperatorsAndRequiresGroup(t *testing.T) {
	env := filepath.Join(t.TempDir(), ".env")
	body := "TELEGRAM_BOT_TOKEN=tg-token\n" +
		"TELEGRAM_ALLOWED_USER_ID=42\n" +
		"TELEGRAM_OPERATOR_USER_IDS=0009, 7,9,42,0007\n" +
		"TELEGRAM_CHAT_ID=-1001234567890\n"
	if err := os.WriteFile(env, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(env)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(cfg.TelegramOperatorUserIDs); got != "[7 9]" {
		t.Fatalf("canonical operator IDs = %s", got)
	}
	if !cfg.TelegramMultiUser() || !cfg.IsTelegramAdministrator(42) || !cfg.IsTelegramOperator(7) || cfg.IsTelegramOperator(42) || cfg.IsTelegramUserAllowed(8) {
		t.Fatalf("Telegram roles do not match canonical config: %#v", cfg)
	}
	operatorSocket := cfg.GitHubBrokerSocketPath()
	cfg.TelegramOperatorUserIDs = []int64{7, 10, 11}
	if cfg.GitHubBrokerSocketPath() != operatorSocket {
		t.Fatal("GitHub broker socket changed with the operator list")
	}
}

func TestLoadRejectsInvalidTelegramMultiUserConfiguration(t *testing.T) {
	tooManyOperators := make([]string, MaxTelegramOperatorUserIDs+1)
	for index := range tooManyOperators {
		tooManyOperators[index] = strconv.Itoa(index + 100)
	}
	tests := []struct {
		name      string
		admin     string
		operators string
		chat      string
	}{
		{name: "negative administrator", admin: "-42"},
		{name: "zero operator", admin: "42", operators: "0", chat: "-1001"},
		{name: "negative operator", admin: "42", operators: "-7", chat: "-1001"},
		{name: "signed operator", admin: "42", operators: "+7", chat: "-1001"},
		{name: "empty element", admin: "42", operators: "7,,9", chat: "-1001"},
		{name: "trailing comma", admin: "42", operators: "7,", chat: "-1001"},
		{name: "space separated", admin: "42", operators: "7 9", chat: "-1001"},
		{name: "missing group", admin: "42", operators: "7"},
		{name: "private chat", admin: "42", operators: "7", chat: "42"},
		{name: "positive other chat", admin: "42", operators: "7", chat: "99"},
		{name: "too many operators", admin: "42", operators: strings.Join(tooManyOperators, ","), chat: "-1001"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := filepath.Join(t.TempDir(), ".env")
			body := "TELEGRAM_BOT_TOKEN=tg-token\nTELEGRAM_ALLOWED_USER_ID=" + test.admin + "\n"
			if test.operators != "" {
				body += "TELEGRAM_OPERATOR_USER_IDS=" + test.operators + "\n"
			}
			if test.chat != "" {
				body += "TELEGRAM_CHAT_ID=" + test.chat + "\n"
			}
			if err := os.WriteFile(env, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(env); err == nil {
				t.Fatal("invalid Telegram multi-user configuration was accepted")
			}
		})
	}
}

func TestTelegramRedactionSecretsExcludeNumericRoutingIdentifiers(t *testing.T) {
	cfg := Config{
		TelegramBotToken:        "token-value",
		TelegramAllowedUserID:   42000001,
		TelegramOperatorUserIDs: []int64{77000001, 88000001},
		TelegramChatID:          -1001234567890,
	}
	got := strings.Join(cfg.RedactionSecrets(), " ")
	if !strings.Contains(got, "token-value") {
		t.Fatalf("redaction secrets omitted bot credential: %q", got)
	}
	for _, want := range []string{"42000001", "77000001", "88000001", "-1001234567890"} {
		if strings.Contains(got, want) {
			t.Fatalf("redaction secrets included numeric routing identifier %q: %q", want, got)
		}
	}
}

func TestPollingLockDirIsInvariantAcrossRuntimeEnvironments(t *testing.T) {
	cfg := Config{Home: filepath.Join(t.TempDir(), "first-home")}
	want := filepath.Join(canonicalDir("/tmp"), "engram-"+strconv.Itoa(os.Getuid()), "locks")
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(t.TempDir(), "runtime-one"))
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "tmp-one"))
	first := cfg.PollingLockDir()
	cfg.Home = filepath.Join(t.TempDir(), "second-home")
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(t.TempDir(), "runtime-two"))
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "tmp-two"))
	if second := cfg.PollingLockDir(); first != want || second != want {
		t.Fatalf("polling lock dirs = %q and %q, want %q", first, second, want)
	}
}

func TestArtifactDirFallsBackForUnsafeXDGRunTimeDir(t *testing.T) {
	parent := t.TempDir()
	runtimeDir := filepath.Join(parent, "runtime")
	if err := os.Mkdir(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tempDir := filepath.Join(parent, "tmp")
	if err := os.Mkdir(tempDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("TMPDIR", tempDir)

	cfg := Config{}
	want := filepath.Join(canonicalDir(tempDir), "engram-"+strconv.Itoa(os.Getuid()))
	if got := cfg.ArtifactDir(); got != want {
		t.Fatalf("ArtifactDir = %q, want fallback %q", got, want)
	}
}

func TestArtifactDirFallsBackForSymlinkedXDGRunTimeDir(t *testing.T) {
	parent := t.TempDir()
	realRuntime := filepath.Join(parent, "runtime-real")
	if err := os.Mkdir(realRuntime, 0o700); err != nil {
		t.Fatal(err)
	}
	runtimeLink := filepath.Join(parent, "runtime-link")
	if err := os.Symlink(realRuntime, runtimeLink); err != nil {
		t.Fatal(err)
	}
	tempDir := filepath.Join(parent, "tmp")
	if err := os.Mkdir(tempDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", runtimeLink)
	t.Setenv("TMPDIR", tempDir)

	want := filepath.Join(canonicalDir(tempDir), "engram-"+strconv.Itoa(os.Getuid()))
	if got := (Config{}).ArtifactDir(); got != want {
		t.Fatalf("ArtifactDir = %q, want fallback %q", got, want)
	}
}

func TestArtifactDirFallsBackForSymlinkedXDGAncestor(t *testing.T) {
	parent := t.TempDir()
	realParent := filepath.Join(parent, "real")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(realParent, "runtime"), 0o700); err != nil {
		t.Fatal(err)
	}
	linkedParent := filepath.Join(parent, "linked")
	if err := os.Symlink(realParent, linkedParent); err != nil {
		t.Fatal(err)
	}
	tempDir := filepath.Join(parent, "tmp")
	if err := os.Mkdir(tempDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(linkedParent, "runtime"))
	t.Setenv("TMPDIR", tempDir)

	want := filepath.Join(canonicalDir(tempDir), "engram-"+strconv.Itoa(os.Getuid()))
	if got := (Config{}).ArtifactDir(); got != want {
		t.Fatalf("ArtifactDir = %q, want fallback %q", got, want)
	}
}

func canonicalTestTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestEnsureDirsRejectsUnsafePreexistingArtifactRoot(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(string) error
	}{
		{name: "symlink", setup: func(path string) error {
			target := filepath.Join(filepath.Dir(path), "attacker-dir")
			if err := os.Mkdir(target, 0o700); err != nil {
				return err
			}
			return os.Symlink(target, path)
		}},
		{name: "regular file", setup: func(path string) error { return os.WriteFile(path, []byte("occupied"), 0o600) }},
		{name: "weak permissions", setup: func(path string) error { return os.Mkdir(path, 0o755) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			parent := t.TempDir()
			tempDir := filepath.Join(parent, "tmp")
			if err := os.Mkdir(tempDir, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("XDG_RUNTIME_DIR", "")
			t.Setenv("TMPDIR", tempDir)
			cfg := Config{Home: filepath.Join(parent, "home")}
			if err := test.setup(cfg.ArtifactDir()); err != nil {
				t.Fatal(err)
			}
			if err := EnsureDirs(cfg); err == nil {
				t.Fatal("EnsureDirs accepted unsafe artifact root")
			}
		})
	}
}

func TestEnsureDirsTightensOwnerOwnedHome(t *testing.T) {
	parent := canonicalTestTempDir(t)
	home := filepath.Join(parent, "home")
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDirs(Config{Home: home}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(home)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("migrated ENGRAM_HOME permissions = %04o, want 0700", got)
	}
}

func TestEnsureDirsAcceptsResolvedHomeParent(t *testing.T) {
	root := canonicalTestTempDir(t)
	realParent := filepath.Join(root, "real")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	aliasParent := filepath.Join(root, "alias")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("TMPDIR", filepath.Join(root, "tmp"))
	if err := os.Mkdir(os.Getenv("TMPDIR"), 0o700); err != nil {
		t.Fatal(err)
	}

	home := filepath.Join(aliasParent, "home")
	if err := EnsureDirs(Config{Home: home}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(filepath.Join(realParent, "home"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("resolved ENGRAM_HOME metadata = %v", info.Mode())
	}
}

func TestEnsureDirsCreatesNestedCustomHome(t *testing.T) {
	root := canonicalTestTempDir(t)
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("TMPDIR", filepath.Join(root, "tmp"))
	if err := os.Mkdir(os.Getenv("TMPDIR"), 0o700); err != nil {
		t.Fatal(err)
	}

	home := filepath.Join(root, "missing", "nested", "home")
	if err := EnsureDirs(Config{Home: home}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(root, "missing"),
		filepath.Join(root, "missing", "nested"),
		home,
		filepath.Join(home, "locks"),
	} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
			t.Fatalf("private path %s has mode %v", path, info.Mode())
		}
	}
}

func TestEnsureDirsRejectsUnsafeHome(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(string) error
	}{
		{name: "symlink", setup: func(path string) error {
			target := filepath.Join(filepath.Dir(path), "real-home")
			if err := os.Mkdir(target, 0o700); err != nil {
				return err
			}
			return os.Symlink(target, path)
		}},
		{name: "regular file", setup: func(path string) error { return os.WriteFile(path, []byte("occupied"), 0o600) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			parent := canonicalTestTempDir(t)
			home := filepath.Join(parent, "home")
			if err := test.setup(home); err != nil {
				t.Fatal(err)
			}
			if err := EnsureDirs(Config{Home: home}); err == nil {
				t.Fatal("EnsureDirs accepted unsafe ENGRAM_HOME")
			}
		})
	}
}

func TestLoadValidatesAndDefaults(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	if err := os.WriteFile(env, []byte(`
TELEGRAM_BOT_TOKEN=tg-token
TELEGRAM_ALLOWED_USER_ID=123
LLM_PROVIDER=anthropic
ANTHROPIC_API_KEY=anthropic-key
ANTHROPIC_MODEL=claude-haiku-4-5-20251001
ENGRAM_TMUX_SESSION=main
ENGRAM_TMUX_SIZE=120x50
ENGRAM_SNAPSHOT_BROWSER=/opt/chromium
ENGRAM_SNAPSHOT_THEME=contrast-dark
ENGRAM_CODEX_CONTEXT_TURNS=4
ENGRAM_CLAUDE_CONTEXT_TURNS=3
ENGRAM_SNAPSHOT_STATUS_COMMAND=df -kP . | awk 'END {printf "disk %.1fG free\n", $4 / 1048576}'
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(env)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TelegramAllowedUserID != 123 || cfg.TelegramChatID != 123 {
		t.Fatalf("ids = %d %d", cfg.TelegramAllowedUserID, cfg.TelegramChatID)
	}
	if cfg.EffectiveTelegramAPIBase() != DefaultTelegramAPIBase {
		t.Fatalf("Telegram API base = %q", cfg.EffectiveTelegramAPIBase())
	}
	if cfg.Home == "" || cfg.Workdir == "" || cfg.AttachmentSoftMaxBytes != DefaultSoftMaxSize {
		t.Fatalf("defaults not applied: %#v", cfg)
	}
	if cfg.EffectiveGitHubGrantMaxDuration() != DefaultGitHubGrantMaxDuration {
		t.Fatalf("GitHub grant maximum = %s", cfg.EffectiveGitHubGrantMaxDuration())
	}
	if cfg.TmuxSession != "main" {
		t.Fatalf("TmuxSession = %q, want main", cfg.TmuxSession)
	}
	if columns, rows, sizeErr := cfg.EffectiveTmuxSize(); sizeErr != nil || columns != 120 || rows != 50 {
		t.Fatalf("TmuxSize = %dx%d, error = %v", columns, rows, sizeErr)
	}
	if cfg.SnapshotBrowser != "/opt/chromium" {
		t.Fatalf("SnapshotBrowser = %q, want /opt/chromium", cfg.SnapshotBrowser)
	}
	if cfg.SnapshotTheme != "contrast-dark" {
		t.Fatalf("SnapshotTheme = %q, want contrast-dark", cfg.SnapshotTheme)
	}
	if cfg.CodexContextTurns != 4 {
		t.Fatalf("CodexContextTurns = %d, want 4", cfg.CodexContextTurns)
	}
	if cfg.ClaudeContextTurns != 3 {
		t.Fatalf("ClaudeContextTurns = %d, want 3", cfg.ClaudeContextTurns)
	}
	if cfg.SnapshotStatusCommand != `df -kP . | awk 'END {printf "disk %.1fG free\n", $4 / 1048576}'` {
		t.Fatalf("SnapshotStatusCommand = %q", cfg.SnapshotStatusCommand)
	}
	if cfg.OpenAITranscriptionModel != DefaultOpenAITranscriptionModel {
		t.Fatalf("OpenAITranscriptionModel = %q, want %q", cfg.OpenAITranscriptionModel, DefaultOpenAITranscriptionModel)
	}
	if cfg.EffectiveVoiceInputMode() != VoiceInputModePath || cfg.VoiceTranscriptionConfigured() {
		t.Fatalf("voice defaults = mode:%q transcription:%v", cfg.EffectiveVoiceInputMode(), cfg.VoiceTranscriptionConfigured())
	}
}

func TestLoadBoundsTmuxSize(t *testing.T) {
	for _, value := range []string{"0x48", "100x0", "401x48", "100x401", "100", "100X48", "100x48x2"} {
		t.Run(value, func(t *testing.T) {
			env := filepath.Join(t.TempDir(), ".env")
			body := "TELEGRAM_BOT_TOKEN=tg-token\nTELEGRAM_ALLOWED_USER_ID=123\nENGRAM_TMUX_SIZE=" + value + "\n"
			if err := os.WriteFile(env, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(env); err == nil {
				t.Fatalf("Load accepted ENGRAM_TMUX_SIZE=%s", value)
			}
		})
	}

	cfg := Config{}
	columns, rows, err := cfg.EffectiveTmuxSize()
	if err != nil || columns != 100 || rows != 48 {
		t.Fatalf("default tmux size = %dx%d, error = %v", columns, rows, err)
	}
}

func TestLoadBoundsCodexContextOptIn(t *testing.T) {
	for _, variable := range []string{"ENGRAM_CODEX_CONTEXT_TURNS", "ENGRAM_CLAUDE_CONTEXT_TURNS"} {
		for _, value := range []string{"-1", "9", "not-a-number"} {
			t.Run(variable+"="+value, func(t *testing.T) {
				dir := t.TempDir()
				env := filepath.Join(dir, ".env")
				body := "TELEGRAM_BOT_TOKEN=tg-token\nTELEGRAM_ALLOWED_USER_ID=123\n" + variable + "=" + value + "\n"
				if err := os.WriteFile(env, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
				if _, err := Load(env); err == nil {
					t.Fatalf("Load accepted %s=%s", variable, value)
				}
			})
		}
	}
}

func TestLoadBoundsRenewableGitHubGrantMaximum(t *testing.T) {
	for _, test := range []struct {
		value string
		valid bool
	}{
		{value: "4h", valid: true},
		{value: "0s"},
		{value: "25h"},
		{value: "forever"},
	} {
		t.Run(test.value, func(t *testing.T) {
			env := filepath.Join(t.TempDir(), ".env")
			body := "TELEGRAM_BOT_TOKEN=tg-token\nTELEGRAM_ALLOWED_USER_ID=123\nENGRAM_GITHUB_GRANT_MAX_DURATION=" + test.value + "\n"
			if err := os.WriteFile(env, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(env)
			if test.valid {
				if err != nil || cfg.EffectiveGitHubGrantMaxDuration() != 4*time.Hour {
					t.Fatalf("Load duration=%s error=%v", cfg.EffectiveGitHubGrantMaxDuration(), err)
				}
			} else if err == nil {
				t.Fatal("invalid grant maximum was accepted")
			}
		})
	}
}

func TestLoadRecordsConfiguredGitHubAppPEMWithoutReadingIt(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	configured := filepath.Join(dir, "missing-github-app.pem")
	body := "TELEGRAM_BOT_TOKEN=tg-token\nTELEGRAM_ALLOWED_USER_ID=123\nENGRAM_GITHUB_APP_PEM_ALIAS=idolum\nENGRAM_GITHUB_APP_PEM_PATH=" + configured + "\n"
	if err := os.WriteFile(env, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(env)
	if err != nil {
		t.Fatalf("optional unavailable PEM prevented config load: %v", err)
	}
	if cfg.GitHubAppPEMPath != configured {
		t.Fatalf("GitHubAppPEMPath = %q, want %q", cfg.GitHubAppPEMPath, configured)
	}
	if cfg.GitHubAppPEMAlias != "idolum" {
		t.Fatalf("GitHubAppPEMAlias = %q, want idolum", cfg.GitHubAppPEMAlias)
	}
}

func TestLoadRequiresConfiguredGitHubAppPEMAliasAndPathTogether(t *testing.T) {
	for _, configured := range []string{
		"ENGRAM_GITHUB_APP_PEM_ALIAS=idolum\n",
		"ENGRAM_GITHUB_APP_PEM_PATH=/missing/app.pem\n",
	} {
		t.Run(strings.TrimSpace(configured), func(t *testing.T) {
			env := filepath.Join(t.TempDir(), ".env")
			body := "TELEGRAM_BOT_TOKEN=tg-token\nTELEGRAM_ALLOWED_USER_ID=123\n" + configured
			if err := os.WriteFile(env, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(env); err == nil || !strings.Contains(err.Error(), "must be set together") {
				t.Fatalf("Load error = %v", err)
			}
		})
	}
}

func TestLoadRejectsUnsupportedTranscriptionModelWhenEnabled(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	if err := os.WriteFile(env, []byte(`
TELEGRAM_BOT_TOKEN=tg-token
TELEGRAM_ALLOWED_USER_ID=123
ENGRAM_ANCHOR_MODE=snapshot
OPENAI_API_KEY=openai-key
VOICE_INPUT_MODE=transcribe
OPENAI_TRANSCRIPTION_MODEL=whisper-1
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(env); err == nil || !strings.Contains(err.Error(), "OPENAI_TRANSCRIPTION_MODEL") {
		t.Fatalf("Load error = %v", err)
	}
}

func TestLoadRequiresKeyForVoiceTranscription(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	if err := os.WriteFile(env, []byte(`
TELEGRAM_BOT_TOKEN=tg-token
TELEGRAM_ALLOWED_USER_ID=123
ENGRAM_ANCHOR_MODE=snapshot
VOICE_INPUT_MODE=transcribe
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(env); err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Fatalf("Load error = %v", err)
	}
}

func TestLoadRejectsUnknownVoiceInputMode(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	if err := os.WriteFile(env, []byte(`
TELEGRAM_BOT_TOKEN=tg-token
TELEGRAM_ALLOWED_USER_ID=123
ENGRAM_ANCHOR_MODE=snapshot
VOICE_INPUT_MODE=automatic
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(env); err == nil || !strings.Contains(err.Error(), "VOICE_INPUT_MODE") {
		t.Fatalf("Load error = %v", err)
	}
}

func TestLoadAcceptsCustomTelegramAPIBase(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	if err := os.WriteFile(env, []byte(`
TELEGRAM_BOT_TOKEN=tg-token
TELEGRAM_API_BASE=http://127.0.0.1:8081/telegram/
TELEGRAM_ALLOWED_USER_ID=123
ENGRAM_ANCHOR_MODE=snapshot
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(env)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.EffectiveTelegramAPIBase(); got != "http://127.0.0.1:8081/telegram" {
		t.Fatalf("Telegram API base = %q", got)
	}
}

func TestLoadRejectsUnsafeTelegramAPIBase(t *testing.T) {
	for _, apiBase := range []string{
		"api.telegram.test",
		"ftp://telegram.test",
		"https://user:pass@telegram.test",
		"https://telegram.test?token=secret",
		"https://telegram.test?",
		"https://telegram.test/#fragment",
		"https://telegram.test/#",
	} {
		t.Run(apiBase, func(t *testing.T) {
			dir := t.TempDir()
			env := filepath.Join(dir, ".env")
			body := "TELEGRAM_BOT_TOKEN=tg-token\nTELEGRAM_API_BASE=" + apiBase + "\nTELEGRAM_ALLOWED_USER_ID=123\nENGRAM_ANCHOR_MODE=snapshot\n"
			if err := os.WriteFile(env, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(env); err == nil {
				t.Fatalf("Load accepted TELEGRAM_API_BASE=%q", apiBase)
			}
		})
	}
}

func TestLoadDefaultsSnapshotTheme(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	if err := os.WriteFile(env, []byte(`
TELEGRAM_BOT_TOKEN=tg-token
TELEGRAM_ALLOWED_USER_ID=123
ANTHROPIC_API_KEY=anthropic-key
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(env)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SnapshotTheme != "terminal" {
		t.Fatalf("SnapshotTheme = %q, want terminal", cfg.SnapshotTheme)
	}
}

func TestLoadRejectsUnknownSnapshotTheme(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	if err := os.WriteFile(env, []byte(`
TELEGRAM_BOT_TOKEN=tg-token
TELEGRAM_ALLOWED_USER_ID=123
ANTHROPIC_API_KEY=anthropic-key
ENGRAM_SNAPSHOT_THEME=sepia
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(env); err == nil {
		t.Fatal("Load accepted unknown snapshot theme")
	}
}

func TestSnapshotAnchorModeDoesNotRequireGuideProvider(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	if err := os.WriteFile(env, []byte(`
TELEGRAM_BOT_TOKEN=tg-token
TELEGRAM_ALLOWED_USER_ID=123
ENGRAM_ANCHOR_MODE=snapshot
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(env)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.SnapshotAnchors() || cfg.AnthropicAPIKey != "" || cfg.OpenAIAPIKey != "" || cfg.GuideConfigured() {
		t.Fatalf("snapshot config = %#v", cfg)
	}
}

func TestLoadAllowsDefaultModeWithoutGuideForPersistedFallback(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	if err := os.WriteFile(env, []byte(`
TELEGRAM_BOT_TOKEN=tg-token
TELEGRAM_ALLOWED_USER_ID=123
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(env)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EffectiveAnchorMode() != AnchorModeGuide || cfg.GuideConfigured() {
		t.Fatalf("default config = %#v", cfg)
	}
}

func TestResolveAnchorModePrefersPersistedThenEnvironmentFallback(t *testing.T) {
	cfg := Config{AnchorMode: AnchorModeGuide}

	mode, err := cfg.ResolveAnchorMode(AnchorModeSnapshot, ModeCapabilities{GuideConfigured: true, SnapshotReady: true})
	if err != nil || mode != AnchorModeSnapshot {
		t.Fatalf("persisted resolution mode=%q err=%v", mode, err)
	}

	mode, err = cfg.ResolveAnchorMode(AnchorModeSnapshot, ModeCapabilities{GuideConfigured: true})
	if err != nil || mode != AnchorModeGuide {
		t.Fatalf("fallback resolution mode=%q err=%v", mode, err)
	}

	if _, err := cfg.ResolveAnchorMode(AnchorModeSnapshot, ModeCapabilities{}); err == nil {
		t.Fatal("resolution succeeded without an available mode")
	}

	mode, err = cfg.ResolveAnchorMode(AnchorModeSnapshot, ModeCapabilities{SnapshotConfigured: true})
	if err != nil || mode != AnchorModeSnapshot {
		t.Fatalf("degraded snapshot resolution mode=%q err=%v", mode, err)
	}
}

func TestLoadRejectsUnknownAnchorMode(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	if err := os.WriteFile(env, []byte(`
TELEGRAM_BOT_TOKEN=tg-token
TELEGRAM_ALLOWED_USER_ID=123
ENGRAM_ANCHOR_MODE=automatic
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(env); err == nil {
		t.Fatal("Load accepted unknown anchor mode")
	}
}

func TestLoadRejectsTmuxSessionSeparators(t *testing.T) {
	for _, name := range []string{"foo:bar", "foo.bar"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			env := filepath.Join(dir, ".env")
			body := "TELEGRAM_BOT_TOKEN=tg-token\nTELEGRAM_ALLOWED_USER_ID=123\nENGRAM_ANCHOR_MODE=snapshot\nENGRAM_TMUX_SESSION=" + name + "\n"
			if err := os.WriteFile(env, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(env); err == nil || !strings.Contains(err.Error(), "ENGRAM_TMUX_SESSION") {
				t.Fatalf("Load(%q) error = %v", name, err)
			}
		})
	}
}

func TestLoadRejectsNonHaiku(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	if err := os.WriteFile(env, []byte(`
TELEGRAM_BOT_TOKEN=tg-token
TELEGRAM_ALLOWED_USER_ID=123
LLM_PROVIDER=anthropic
ANTHROPIC_API_KEY=anthropic-key
ANTHROPIC_MODEL=claude-sonnet-4-20250514
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(env); err == nil {
		t.Fatal("Load accepted non-Haiku model")
	}
}

func TestLoadConfiguresOpenAILuna(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	if err := os.WriteFile(env, []byte(`
TELEGRAM_BOT_TOKEN=tg-token
TELEGRAM_ALLOWED_USER_ID=123
LLM_PROVIDER=openai
OPENAI_API_KEY=openai-key
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(env)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.GuideConfigured() || cfg.EffectiveLLMProvider() != LLMProviderOpenAI || cfg.GuideModel() != DefaultOpenAIModel || cfg.OpenAIAPIKey != "openai-key" {
		t.Fatalf("OpenAI config = %#v", cfg)
	}
}

func TestLoadRejectsUnassessedOpenAIModel(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	if err := os.WriteFile(env, []byte(`
TELEGRAM_BOT_TOKEN=tg-token
TELEGRAM_ALLOWED_USER_ID=123
LLM_PROVIDER=openai
OPENAI_API_KEY=openai-key
OPENAI_MODEL=gpt-5.4-nano
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(env); err == nil || !strings.Contains(err.Error(), "OPENAI_MODEL") {
		t.Fatalf("Load error = %v", err)
	}
}

func TestLoadRejectsUnknownLLMProviderEvenWithoutKey(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	if err := os.WriteFile(env, []byte(`
TELEGRAM_BOT_TOKEN=tg-token
TELEGRAM_ALLOWED_USER_ID=123
LLM_PROVIDER=local
ENGRAM_ANCHOR_MODE=snapshot
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(env); err == nil || !strings.Contains(err.Error(), "LLM_PROVIDER") {
		t.Fatalf("Load error = %v", err)
	}
}

func TestLoadRejectsMalformedNumericConfig(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	if err := os.WriteFile(env, []byte(`
TELEGRAM_BOT_TOKEN=tg-token
TELEGRAM_ALLOWED_USER_ID=123
ANTHROPIC_API_KEY=anthropic-key
ENGRAM_ATTACHMENT_SOFT_MAX_BYTES=not-a-number
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(env); err == nil {
		t.Fatal("Load accepted malformed numeric config")
	}
}

func TestLoadRejectsWeakEnvPermissions(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	if err := os.WriteFile(env, []byte(`
TELEGRAM_BOT_TOKEN=tg-token
TELEGRAM_ALLOWED_USER_ID=123
ANTHROPIC_API_KEY=anthropic-key
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(env); err == nil {
		t.Fatal("Load accepted weak env permissions")
	}
}
