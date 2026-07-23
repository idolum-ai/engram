package keyseqeval_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/anthropic"
	"github.com/idolum-ai/engram/internal/keyseq"
	"github.com/idolum-ai/engram/internal/openai"
)

type fixture struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Expected    keyseq.Proposal `json:"expected"`
}

func TestLiveKeyInterpretation(t *testing.T) {
	provider := strings.ToLower(strings.TrimSpace(os.Getenv("ENGRAM_LIVE_KEYSEQ_EVAL")))
	if provider == "" {
		t.Skip("set ENGRAM_LIVE_KEYSEQ_EVAL to anthropic, openai, or all")
	}
	fixtures := loadFixtures(t)
	trials := 1
	if raw := strings.TrimSpace(os.Getenv("ENGRAM_LIVE_KEYSEQ_EVAL_TRIALS")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 5 {
			t.Fatalf("ENGRAM_LIVE_KEYSEQ_EVAL_TRIALS must be from 1 through 5")
		}
		trials = value
	}
	if provider == "anthropic" || provider == "all" {
		key := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
		if key == "" {
			t.Fatal("ANTHROPIC_API_KEY is required")
		}
		runProvider(t, "anthropic", anthropic.New(key, "claude-haiku-4-5-20251001"), fixtures, trials)
	}
	if provider == "openai" || provider == "all" {
		key := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
		if key == "" {
			t.Fatal("OPENAI_API_KEY is required")
		}
		runProvider(t, "openai", openai.New(key, "gpt-5.6-luna"), fixtures, trials)
	}
	if provider != "anthropic" && provider != "openai" && provider != "all" {
		t.Fatalf("unknown ENGRAM_LIVE_KEYSEQ_EVAL provider %q", provider)
	}
}

func runProvider(t *testing.T, name string, interpreter keyseq.Interpreter, fixtures []fixture, trials int) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		exact := 0
		safeMisses := 0
		total := len(fixtures) * trials
		for _, fixture := range fixtures {
			t.Run(fixture.Name, func(t *testing.T) {
				for trial := 1; trial <= trials; trial++ {
					ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
					got, err := interpreter.InterpretKeys(ctx, fixture.Description)
					cancel()
					if err != nil {
						if fixture.Expected.Kind == keyseq.KindClarification && errors.Is(err, keyseq.ErrInvalidProposal) {
							safeMisses++
							t.Logf("trial %d: safe deterministic rejection instead of clarification", trial)
							continue
						}
						t.Fatalf("trial %d: %v", trial, err)
					}
					got, err = keyseq.Validate(got)
					if err != nil {
						t.Fatalf("trial %d: invalid proposal: %v", trial, err)
					}
					want, err := keyseq.Validate(fixture.Expected)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(got, want) {
						if want.Kind == keyseq.KindSequence && got.Kind == keyseq.KindClarification {
							safeMisses++
							t.Logf("trial %d: safe clarification instead of expected sequence", trial)
							continue
						}
						t.Errorf("trial %d:\ngot  %#v\nwant %#v", trial, got, want)
						continue
					}
					exact++
				}
			})
		}
		if exact*100 < total*80 {
			t.Errorf("exact interpretation rate = %d/%d; want at least 80%% (safe misses=%d)", exact, total, safeMisses)
		}
	})
}

func loadFixtures(t *testing.T) []fixture {
	t.Helper()
	raw, err := os.ReadFile("../keyseq/testdata/interpretation_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []fixture
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	return fixtures
}
