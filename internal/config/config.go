package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	DefaultModel       = "claude-haiku-4-5-20251001"
	DefaultModelAlias  = "claude-haiku-4-5"
	DefaultSoftMaxSize = int64(52_428_800)
)

type Config struct {
	EnvPath                    string
	TelegramBotToken           string
	TelegramAllowedUserID      int64
	TelegramChatID             int64
	LLMProvider                string
	AnthropicAPIKey            string
	AnthropicModel             string
	Home                       string
	Workdir                    string
	TmuxSession                string
	AttachmentSoftMaxBytes     int64
	TelegramPollTimeoutSeconds int
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
		LLMProvider:                firstNonEmpty(values["LLM_PROVIDER"], "anthropic"),
		AnthropicAPIKey:            values["ANTHROPIC_API_KEY"],
		AnthropicModel:             firstNonEmpty(values["ANTHROPIC_MODEL"], DefaultModel),
		Home:                       ExpandPath(firstNonEmpty(values["ENGRAM_HOME"], "~/.engram")),
		Workdir:                    ExpandPath(firstNonEmpty(values["ENGRAM_WORKDIR"], "~")),
		TmuxSession:                values["ENGRAM_TMUX_SESSION"],
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
	if strings.TrimSpace(c.AnthropicAPIKey) == "" {
		missing = append(missing, "ANTHROPIC_API_KEY")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required config: %s", strings.Join(missing, ", "))
	}
	if c.LLMProvider != "anthropic" {
		return fmt.Errorf("LLM_PROVIDER must be anthropic")
	}
	if c.TelegramChatID == 0 {
		return fmt.Errorf("TELEGRAM_CHAT_ID resolved to zero")
	}
	if c.AnthropicModel != DefaultModel && c.AnthropicModel != DefaultModelAlias {
		return fmt.Errorf("ANTHROPIC_MODEL must be %s or %s", DefaultModel, DefaultModelAlias)
	}
	if c.AttachmentSoftMaxBytes <= 0 {
		return fmt.Errorf("ENGRAM_ATTACHMENT_SOFT_MAX_BYTES must be positive")
	}
	if c.TelegramPollTimeoutSeconds <= 0 {
		return fmt.Errorf("TELEGRAM_POLL_TIMEOUT_SECONDS must be positive")
	}
	return nil
}

func (c Config) StatePath() string       { return filepath.Join(c.Home, "state.json") }
func (c Config) AuditPath() string       { return filepath.Join(c.Home, "audit.jsonl") }
func (c Config) LockDir() string         { return filepath.Join(c.Home, "locks") }
func (c Config) AttachmentDir() string   { return filepath.Join(os.TempDir(), "engram", "attachments") }
func (c Config) ArtifactDir() string     { return filepath.Join(os.TempDir(), "engram") }
func (c Config) TelegramAPIBase() string { return "https://api.telegram.org/bot" + c.TelegramBotToken }

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
	for _, dir := range []string{cfg.Home, cfg.LockDir(), cfg.AttachmentDir(), cfg.ArtifactDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
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
