package redact

import (
	"strings"
	"testing"
)

func TestSecretsRedactsCommonSecretShapes(t *testing.T) {
	raw := strings.Join([]string{
		`Authorization: Bearer bearer-secret-value`,
		`ANTHROPIC_API_KEY=sk-ant-secret-value`,
		`TELEGRAM_BOT_TOKEN=123:telegram-secret`,
		`{"password":"pw-secret-value","token":"json-token-value"}`,
		`github_pat_1234567890abcdef`,
		`https://example.test/file?X-Amz-Signature=signed-secret-value&ok=1`,
		"-----BEGIN PRIVATE KEY-----\nabc123privatekeymaterial\n-----END PRIVATE KEY-----",
	}, "\n")

	got := Secrets(raw)
	for _, leaked := range []string{
		"bearer-secret-value",
		"sk-ant-secret-value",
		"123:telegram-secret",
		"pw-secret-value",
		"json-token-value",
		"github_pat_1234567890abcdef",
		"signed-secret-value",
		"abc123privatekeymaterial",
	} {
		if strings.Contains(got, leaked) {
			t.Fatalf("Secrets leaked %q in:\n%s", leaked, got)
		}
	}
	for _, want := range []string{"<redacted", "Authorization: Bearer <redacted>", "ANTHROPIC_API_KEY=<redacted>"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Secrets output missing %q:\n%s", want, got)
		}
	}
}
