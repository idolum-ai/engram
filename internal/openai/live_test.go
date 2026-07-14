package openai

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/guide"
)

func TestLiveLunaCompatibility(t *testing.T) {
	if os.Getenv("ENGRAM_LIVE_LUNA_TEST") != "1" {
		t.Skip("set ENGRAM_LIVE_LUNA_TEST=1 to make one live OpenAI request")
	}
	apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if apiKey == "" {
		t.Fatal("OPENAI_API_KEY is required")
	}
	model := strings.TrimSpace(os.Getenv("OPENAI_MODEL"))
	if model == "" {
		model = testModel
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	output, err := New(apiKey, model).Converse(ctx, guide.Input{
		SessionID:   1,
		VisibleText: "$ go test ./...\n--- FAIL: TestStore (0.01s)\n    store_test.go:42: got 2, want 3",
	})
	if err != nil {
		t.Fatal(err)
	}
	if output == "" || len(strings.Fields(output)) > guide.MaxWords {
		t.Fatalf("invalid bounded output: %q", output)
	}
}
