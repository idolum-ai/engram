package agentcompat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParallelReviewedProviderDeclarationsMatchContracts(t *testing.T) {
	for _, provider := range []Provider{ProviderCodex, ProviderClaude} {
		data, err := os.ReadFile(filepath.Join("testdata", string(provider), "compatibility.json"))
		if err != nil {
			t.Fatal(err)
		}
		var value struct {
			Provider            Provider `json:"provider"`
			Process             string   `json:"process_contract"`
			Binding             string   `json:"binding_contract"`
			Screen              string   `json:"screen_contract"`
			Transcript          string   `json:"transcript_contract"`
			ContainsLiveContent bool     `json:"contains_live_content"`
		}
		if err := json.Unmarshal(data, &value); err != nil {
			t.Fatal(err)
		}
		wantProcess, wantBinding, wantScreen, wantTranscript := CodexProcessContract, CodexBindingContract, CodexScreenContract, CodexTranscriptContract
		if provider == ProviderClaude {
			wantProcess, wantBinding, wantScreen, wantTranscript = ClaudeProcessContract, ClaudeBindingContract, ClaudeScreenContract, ClaudeTranscriptContract
		}
		if value.Provider != provider || value.Process != wantProcess || value.Binding != wantBinding || value.Screen != wantScreen || value.Transcript != wantTranscript || value.ContainsLiveContent {
			t.Fatalf("%s declaration = %#v", provider, value)
		}
		for _, name := range []string{"frame.sanitized.txt", "transcript-inventory.json"} {
			fixture, err := os.ReadFile(filepath.Join("testdata", string(provider), name))
			if err != nil {
				t.Fatal(err)
			}
			text := string(fixture)
			for _, forbidden := range []string{"/Users/", "/home/", "@example.", "019f7607-c8b0-74b3-87ca-64a7e6e7ede0", "private task", "agent-name"} {
				if strings.Contains(text, forbidden) {
					t.Fatalf("%s/%s contains live-looking data %q", provider, name, forbidden)
				}
			}
		}
	}
}

func TestCompatibilityAxesRemainIndependent(t *testing.T) {
	got := NormalizeCompatibility(Compatibility{
		Provider:   ProviderClaude,
		Process:    Axis{State: StateProven, Contract: "claude-process-v1", Version: "2.1.225"},
		Binding:    Axis{State: StateMissing, Contract: "claude-session-start-v1", Reason: ReasonBindingMissing},
		Screen:     Axis{State: StateLiteral, Contract: "claude-screen-v1", Version: "2.1.225", Reason: ReasonScreenVersionUnknown},
		Transcript: Axis{State: StateEligible, Contract: "claude-transcript-v1", Reason: ReasonTranscriptEligible},
	})
	if got.Process.State != StateProven || got.Binding.State != StateMissing || got.Screen.State != StateLiteral || got.Transcript.State != StateEligible {
		t.Fatalf("independent compatibility axes collapsed: %#v", got)
	}
}

func TestPresentationBoundsPrivateAndEphemeralValues(t *testing.T) {
	got := NormalizePresentation(Presentation{
		Model:       Value{Value: strings.Repeat("m", 200), Provenance: ProvenanceHook},
		Effort:      Value{Value: "high\nsecret", Provenance: ProvenanceVisibleUI},
		Interaction: Value{Value: "manual", Provenance: ProvenanceVisibleUI},
		Activity:    strings.Repeat("a", 80), LastTurnSeconds: int((MaxDuration + time.Second) / time.Second),
		AgentTotal: 3, AgentActive: 1,
	})
	if len(got.Model.Value) != MaxValueBytes || got.Effort.Value != "" || got.Interaction.Value != "manual" || len(got.Activity) != MaxActivityBytes || got.LastTurnSeconds != 0 || got.AgentTotal != 3 || got.AgentActive != 1 {
		t.Fatalf("normalized presentation = %#v", got)
	}
}

func TestViewportRequiresProcessBoundPublication(t *testing.T) {
	if got := NormalizeViewport(Viewport{Applied: true, Contract: "claude-screen-v1", Boundary: "startup", StartLine: 4}); got.Applied {
		t.Fatalf("viewport without runtime identity survived: %#v", got)
	}
	got := NormalizeViewport(Viewport{Applied: true, Contract: "claude-screen-v1", RuntimeIdentity: strings.Repeat("a", 64), TmuxIdentity: strings.Repeat("b", 64), Boundary: "startup", StartLine: 4, AlternateScreen: "1", CopyMode: "0"})
	if !got.Applied || got.StartLine != 4 {
		t.Fatalf("valid viewport rejected: %#v", got)
	}
}
