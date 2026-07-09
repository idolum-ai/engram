package redact

import "strings"

func Secrets(text string, secrets ...string) string {
	out := text
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if len(secret) < 6 {
			continue
		}
		out = strings.ReplaceAll(out, secret, "<redacted>")
	}
	for _, marker := range []string{"TELEGRAM_BOT_TOKEN=", "ANTHROPIC_API_KEY="} {
		for {
			idx := strings.Index(out, marker)
			if idx < 0 {
				break
			}
			end := strings.IndexByte(out[idx:], '\n')
			if end < 0 {
				out = out[:idx+len(marker)] + "<redacted>"
				break
			}
			out = out[:idx+len(marker)] + "<redacted>" + out[idx+end:]
		}
	}
	return out
}
