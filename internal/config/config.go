package config

import (
	"bufio"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const (
	DefaultAnthropicModel           = "claude-haiku-4-5-20251001"
	AnthropicModelAlias             = "claude-haiku-4-5"
	DefaultOpenAIModel              = "gpt-5.6-luna"
	DefaultOpenAITranscriptionModel = "gpt-4o-transcribe"
	DefaultSoftMaxSize              = int64(16_777_216)
	DefaultTelegramAPIBase          = "https://api.telegram.org"
	LLMProviderAnthropic            = "anthropic"
	LLMProviderOpenAI               = "openai"
	AnchorModeGuide                 = "guide"
	AnchorModeSnapshot              = "snapshot"
	VoiceInputModePath              = "path"
	VoiceInputModeTranscribe        = "transcribe"
)

type Config struct {
	EnvPath                    string
	TelegramBotToken           string
	TelegramAPIBase            string
	TelegramAllowedUserID      int64
	TelegramChatID             int64
	LLMProvider                string
	AnthropicAPIKey            string
	AnthropicModel             string
	OpenAIAPIKey               string
	OpenAIModel                string
	OpenAITranscriptionModel   string
	VoiceInputMode             string
	Home                       string
	Workdir                    string
	TmuxSession                string
	AnchorMode                 string
	SnapshotBrowser            string
	SnapshotTheme              string
	SnapshotStatusCommand      string
	AttachmentSoftMaxBytes     int64
	TelegramPollTimeoutSeconds int
}

type ModeCapabilities struct {
	GuideConfigured bool
	SnapshotReady   bool
}

func DefaultEnvPath() string {
	return ExpandPath("~/.engram/.env")
}

func Load(path string) (Config, error) {
	if strings.TrimSpace(path) == "" {
		path = DefaultEnvPath()
	}
	path = ExpandPath(path)
	if err := validateEnvFileMetadata(path); err != nil {
		return Config{}, err
	}
	values, err := parseEnvFile(path)
	if err != nil {
		return Config{}, err
	}
	softMax, err := parseInt64Default(values["ENGRAM_ATTACHMENT_SOFT_MAX_BYTES"], "ENGRAM_ATTACHMENT_SOFT_MAX_BYTES", DefaultSoftMaxSize)
	if err != nil {
		return Config{}, err
	}
	pollTimeout, err := parseInt64Default(values["TELEGRAM_POLL_TIMEOUT_SECONDS"], "TELEGRAM_POLL_TIMEOUT_SECONDS", 50)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		EnvPath:                    path,
		TelegramBotToken:           values["TELEGRAM_BOT_TOKEN"],
		TelegramAPIBase:            firstNonEmpty(values["TELEGRAM_API_BASE"], DefaultTelegramAPIBase),
		LLMProvider:                strings.ToLower(firstNonEmpty(values["LLM_PROVIDER"], LLMProviderAnthropic)),
		AnthropicAPIKey:            values["ANTHROPIC_API_KEY"],
		AnthropicModel:             firstNonEmpty(values["ANTHROPIC_MODEL"], DefaultAnthropicModel),
		OpenAIAPIKey:               values["OPENAI_API_KEY"],
		OpenAIModel:                firstNonEmpty(values["OPENAI_MODEL"], DefaultOpenAIModel),
		OpenAITranscriptionModel:   firstNonEmpty(values["OPENAI_TRANSCRIPTION_MODEL"], DefaultOpenAITranscriptionModel),
		VoiceInputMode:             strings.ToLower(firstNonEmpty(values["VOICE_INPUT_MODE"], VoiceInputModePath)),
		Home:                       ExpandPath(firstNonEmpty(values["ENGRAM_HOME"], "~/.engram")),
		Workdir:                    ExpandPath(firstNonEmpty(values["ENGRAM_WORKDIR"], "~")),
		TmuxSession:                values["ENGRAM_TMUX_SESSION"],
		AnchorMode:                 firstNonEmpty(values["ENGRAM_ANCHOR_MODE"], AnchorModeGuide),
		SnapshotBrowser:            ExpandPath(values["ENGRAM_SNAPSHOT_BROWSER"]),
		SnapshotTheme:              firstNonEmpty(values["ENGRAM_SNAPSHOT_THEME"], "terminal"),
		SnapshotStatusCommand:      strings.TrimSpace(values["ENGRAM_SNAPSHOT_STATUS_COMMAND"]),
		AttachmentSoftMaxBytes:     softMax,
		TelegramPollTimeoutSeconds: int(pollTimeout),
	}
	if cfg.TelegramAllowedUserID, err = parseOptionalInt64(values["TELEGRAM_ALLOWED_USER_ID"], "TELEGRAM_ALLOWED_USER_ID"); err != nil {
		return Config{}, err
	}
	if cfg.TelegramChatID, err = parseOptionalInt64(firstNonEmpty(values["TELEGRAM_CHAT_ID"], values["TELEGRAM_GROUP_CHAT_ID"]), "TELEGRAM_CHAT_ID"); err != nil {
		return Config{}, err
	}
	if cfg.TelegramChatID == 0 {
		cfg.TelegramChatID = cfg.TelegramAllowedUserID
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	var missing []string
	if strings.TrimSpace(c.TelegramBotToken) == "" {
		missing = append(missing, "TELEGRAM_BOT_TOKEN")
	}
	if c.TelegramAllowedUserID == 0 {
		missing = append(missing, "TELEGRAM_ALLOWED_USER_ID")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required config: %s", strings.Join(missing, ", "))
	}
	switch c.EffectiveLLMProvider() {
	case LLMProviderAnthropic:
		if c.GuideConfigured() && c.AnthropicModel != DefaultAnthropicModel && c.AnthropicModel != AnthropicModelAlias {
			return fmt.Errorf("ANTHROPIC_MODEL must be %s or %s", DefaultAnthropicModel, AnthropicModelAlias)
		}
	case LLMProviderOpenAI:
		if c.GuideConfigured() && c.OpenAIModel != DefaultOpenAIModel {
			return fmt.Errorf("OPENAI_MODEL must be %s", DefaultOpenAIModel)
		}
	default:
		return fmt.Errorf("LLM_PROVIDER must be anthropic or openai")
	}
	switch c.EffectiveAnchorMode() {
	case AnchorModeGuide, AnchorModeSnapshot:
	default:
		return fmt.Errorf("ENGRAM_ANCHOR_MODE must be guide or snapshot")
	}
	if c.TelegramChatID == 0 {
		return fmt.Errorf("TELEGRAM_CHAT_ID resolved to zero")
	}
	switch c.EffectiveVoiceInputMode() {
	case VoiceInputModePath:
	case VoiceInputModeTranscribe:
		if strings.TrimSpace(c.OpenAIAPIKey) == "" {
			return fmt.Errorf("VOICE_INPUT_MODE=transcribe requires OPENAI_API_KEY")
		}
	default:
		return fmt.Errorf("VOICE_INPUT_MODE must be path or transcribe")
	}
	if c.VoiceTranscriptionConfigured() && c.OpenAITranscriptionModel != DefaultOpenAITranscriptionModel {
		return fmt.Errorf("OPENAI_TRANSCRIPTION_MODEL must be %s", DefaultOpenAITranscriptionModel)
	}
	if c.AttachmentSoftMaxBytes <= 0 {
		return fmt.Errorf("ENGRAM_ATTACHMENT_SOFT_MAX_BYTES must be positive")
	}
	switch c.SnapshotTheme {
	case "terminal", "contrast-dark", "contrast-light":
	default:
		return fmt.Errorf("ENGRAM_SNAPSHOT_THEME must be terminal, contrast-dark, or contrast-light")
	}
	if c.TelegramPollTimeoutSeconds <= 0 {
		return fmt.Errorf("TELEGRAM_POLL_TIMEOUT_SECONDS must be positive")
	}
	if strings.ContainsAny(strings.TrimSpace(c.TmuxSession), ":.") {
		return fmt.Errorf("ENGRAM_TMUX_SESSION must not contain ':' or '.'")
	}
	if err := validateTelegramAPIBase(c.EffectiveTelegramAPIBase()); err != nil {
		return err
	}
	return nil
}

func (c Config) EffectiveAnchorMode() string {
	if strings.TrimSpace(c.AnchorMode) == "" {
		return AnchorModeGuide
	}
	return strings.TrimSpace(c.AnchorMode)
}

// ResolveAnchorMode prefers a usable persisted choice and treats the
// environment setting as the startup fallback.
func (c Config) ResolveAnchorMode(persisted string, capabilities ModeCapabilities) (string, error) {
	for _, mode := range []string{strings.TrimSpace(persisted), c.EffectiveAnchorMode()} {
		switch mode {
		case AnchorModeGuide:
			if capabilities.GuideConfigured {
				return mode, nil
			}
		case AnchorModeSnapshot:
			if capabilities.SnapshotReady {
				return mode, nil
			}
		}
	}
	return "", fmt.Errorf("no available anchor mode (guide requires a configured conversational provider; snapshot requires probed Chromium)")
}

func (c Config) SnapshotAnchors() bool { return c.EffectiveAnchorMode() == AnchorModeSnapshot }

func (c Config) EffectiveLLMProvider() string {
	return strings.ToLower(firstNonEmpty(strings.TrimSpace(c.LLMProvider), LLMProviderAnthropic))
}

func (c Config) GuideConfigured() bool {
	switch c.EffectiveLLMProvider() {
	case LLMProviderAnthropic:
		return strings.TrimSpace(c.AnthropicAPIKey) != ""
	case LLMProviderOpenAI:
		return strings.TrimSpace(c.OpenAIAPIKey) != ""
	default:
		return false
	}
}

func (c Config) GuideModel() string {
	switch c.EffectiveLLMProvider() {
	case LLMProviderAnthropic:
		return c.AnthropicModel
	case LLMProviderOpenAI:
		return c.OpenAIModel
	default:
		return ""
	}
}

func (c Config) EffectiveVoiceInputMode() string {
	return strings.ToLower(firstNonEmpty(strings.TrimSpace(c.VoiceInputMode), VoiceInputModePath))
}

func (c Config) VoiceTranscriptionConfigured() bool {
	return c.EffectiveVoiceInputMode() == VoiceInputModeTranscribe && strings.TrimSpace(c.OpenAIAPIKey) != ""
}

func (c Config) EffectiveTelegramAPIBase() string {
	return strings.TrimRight(firstNonEmpty(strings.TrimSpace(c.TelegramAPIBase), DefaultTelegramAPIBase), "/")
}

func (c Config) StatePath() string     { return filepath.Join(c.Home, "state.json") }
func (c Config) AuditPath() string     { return filepath.Join(c.Home, "audit.jsonl") }
func (c Config) TemplatePath() string  { return filepath.Join(c.Home, "templates.json") }
func (c Config) LockDir() string       { return filepath.Join(c.Home, "locks") }
func (c Config) AttachmentDir() string { return filepath.Join(c.ArtifactDir(), "attachments") }
func (c Config) ArtifactDir() string   { return artifactRoot() }

func validateTelegramAPIBase(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return fmt.Errorf("TELEGRAM_API_BASE must be an absolute HTTP(S) URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || strings.Contains(value, "#") {
		return fmt.Errorf("TELEGRAM_API_BASE must not contain userinfo, a query, or a fragment")
	}
	return nil
}

func artifactRoot() string {
	if runtimeDir, ok := privateRuntimeBase(os.Getenv("XDG_RUNTIME_DIR")); ok {
		return filepath.Join(runtimeDir, "engram")
	}
	tempDir := canonicalDir(os.TempDir())
	return filepath.Join(tempDir, "engram-"+strconv.Itoa(os.Getuid()))
}

func privateRuntimeBase(path string) (string, bool) {
	if path == "" || !filepath.IsAbs(path) {
		return "", false
	}
	path = filepath.Clean(path)
	if err := rejectSymlinkComponents(path); err != nil {
		return "", false
	}
	if err := validatePrivateDir(path); err != nil {
		return "", false
	}
	if err := syscall.Access(path, 2); err != nil {
		return "", false
	}
	return path, true
}

func canonicalDir(path string) string {
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

func ExpandPath(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func parseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open env file %s: %w", path, err)
	}
	defer f.Close()
	out := make(map[string]string)
	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected KEY=VALUE", path, lineNo)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("%s:%d: empty key", path, lineNo)
		}
		out[key] = unquote(strings.TrimSpace(value))
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func unquote(value string) string {
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}

func parseOptionalInt64(raw string, key string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", key, err)
	}
	return n, nil
}

func parseInt64Default(raw string, key string, def int64) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", key, err)
	}
	return n, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func EnsureDirs(cfg Config) error {
	if err := ensurePrivateHome(cfg.Home); err != nil {
		return err
	}
	if err := ensurePrivateDir(cfg.LockDir()); err != nil {
		return err
	}
	for _, dir := range []string{cfg.ArtifactDir(), cfg.AttachmentDir()} {
		if err := ensurePrivateDir(dir); err != nil {
			return err
		}
	}
	return nil
}

func ensurePrivateHome(path string) error {
	return ensurePrivateDirWithRepair(path, true)
}

func ensurePrivateDir(path string) error {
	return ensurePrivateDirWithRepair(path, false)
}

func ensurePrivateDirWithRepair(path string, repairPermissions bool) error {
	resolvedPath, err := resolvedPrivateDirPath(path)
	if err != nil {
		return fmt.Errorf("unsafe parent for private directory %s: %w", path, err)
	}
	if err := os.Mkdir(resolvedPath, 0o700); err != nil && !os.IsExist(err) {
		return fmt.Errorf("create private directory %s: %w", path, err)
	}
	if err := validatePrivateDirPath(resolvedPath, repairPermissions); err != nil {
		return fmt.Errorf("unsafe private directory %s: %w", path, err)
	}
	return nil
}

func validatePrivateDir(path string) error {
	resolvedPath, err := resolvedPrivateDirPath(path)
	if err != nil {
		return err
	}
	return validatePrivateDirPath(resolvedPath, false)
}

func resolvedPrivateDirPath(path string) (string, error) {
	path = filepath.Clean(path)
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	if err := rejectSymlinkComponents(parent); err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(path)), nil
}

func validatePrivateDirPath(path string, repairPermissions bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("must not be a symlink")
	}
	if !info.IsDir() {
		return fmt.Errorf("must be a directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot determine directory owner")
	}
	if stat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("must be owned by uid %d", os.Getuid())
	}
	if info.Mode().Perm() == 0o700 {
		return nil
	}
	if !repairPermissions {
		return fmt.Errorf("permissions must be 0700, got %04o", info.Mode().Perm())
	}
	return tightenPrivateDir(path)
}

func tightenPrivateDir(path string) error {
	dir, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
	if err != nil {
		return fmt.Errorf("open directory without following symlinks: %w", err)
	}
	if err := dir.Chmod(0o700); err != nil {
		return errors.Join(fmt.Errorf("set permissions to 0700: %w", err), dir.Close())
	}
	info, statErr := dir.Stat()
	closeErr := dir.Close()
	if err := errors.Join(statErr, closeErr); err != nil {
		return fmt.Errorf("verify permissions: %w", err)
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("permissions remain %04o after tightening", info.Mode().Perm())
	}
	return nil
}

func rejectSymlinkComponents(path string) error {
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink path component: %s", current)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
	}
}

func validateEnvFileMetadata(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("env file must be a regular file: %s", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("env file permissions must not grant group/other access: %s", path)
	}
	return nil
}
