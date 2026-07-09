package redact

import (
	"regexp"
	"strings"
)

var patterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`(?i)(Authorization:\s*Bearer\s+)[^\s"']+`), `${1}<redacted>`},
	{regexp.MustCompile(`(?i)\b([A-Z0-9_]*(?:API[_-]?KEY|TOKEN|SECRET|PASSWORD)[A-Z0-9_]*\s*=\s*)[^\s"']+`), `${1}<redacted>`},
	{regexp.MustCompile(`(?i)\b(password|token|secret|api[_-]?key)["']?\s*:\s*["'][^"']+["']`), `${1}: "<redacted>"`},
	{regexp.MustCompile(`github_pat_[A-Za-z0-9_]+`), `<redacted:github_token>`},
	{regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]+`), `<redacted:anthropic_key>`},
	{regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`), `<redacted:private_key>`},
	{regexp.MustCompile(`([?&](?:X-Amz-Signature|signature|token|access_token|api_key)=)[^&\s]+`), `${1}<redacted>`},
}

func Secrets(text string, secrets ...string) string {
	out := text
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if len(secret) < 6 {
			continue
		}
		out = strings.ReplaceAll(out, secret, "<redacted>")
	}
	for _, pattern := range patterns {
		out = pattern.re.ReplaceAllString(out, pattern.repl)
	}
	return out
}
