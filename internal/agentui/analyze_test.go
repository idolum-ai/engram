package agentui

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type replayCase struct {
	Name        string      `json:"name"`
	Source      string      `json:"source"`
	Observation Observation `json:"observation"`
	Want        struct {
		Applied           bool         `json:"applied"`
		Model             string       `json:"model"`
		Effort            string       `json:"effort"`
		Mode              string       `json:"mode"`
		Activity          Activity     `json:"activity"`
		Contains          []string     `json:"contains"`
		Excludes          []string     `json:"excludes"`
		ConversationExact string       `json:"conversation_exact"`
		Roles             map[Role]int `json:"roles"`
	} `json:"want"`
}

func TestReplayCorpus(t *testing.T) {
	contents, err := os.ReadFile("testdata/replay_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []replayCase
	if err := json.Unmarshal(contents, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 12 {
		t.Fatalf("replay corpus contains %d cases, want at least 12", len(cases))
	}
	for _, test := range cases {
		t.Run(test.Name, func(t *testing.T) {
			got := Analyze(test.Observation)
			if got.Applied != test.Want.Applied || got.Model != test.Want.Model || got.Effort != test.Want.Effort || got.Mode != test.Want.Mode || got.Activity != test.Want.Activity {
				t.Fatalf("analysis metadata = %#v; source: %s", got, test.Source)
			}
			if test.Want.ConversationExact != "" && got.Conversation != test.Want.ConversationExact {
				t.Fatalf("conversation = %q, want exact %q", got.Conversation, test.Want.ConversationExact)
			}
			for _, wanted := range test.Want.Contains {
				if !strings.Contains(got.Conversation, wanted) {
					t.Errorf("conversation omitted %q: %q", wanted, got.Conversation)
				}
			}
			for _, unwanted := range test.Want.Excludes {
				if strings.Contains(got.Conversation, unwanted) {
					t.Errorf("conversation retained %q: %q", unwanted, got.Conversation)
				}
			}
			roles := make(map[Role]int)
			for _, region := range got.Regions {
				roles[region.Role] += region.EndLine - region.StartLine + 1
			}
			for role, want := range test.Want.Roles {
				if roles[role] != want {
					t.Errorf("role %q covers %d lines, want %d; regions: %#v", role, roles[role], want, got.Regions)
				}
			}
		})
	}
}

func TestAnalyzeBoundsWorkAndPreservesOversizedFrame(t *testing.T) {
	lines := make([]string, maxFrameRows+1)
	for index := range lines {
		lines[index] = "line"
	}
	lines[len(lines)-1] = "gpt-5.6-sol high · /work"
	input := strings.Join(lines, "\n")
	got := Analyze(Observation{Current: Frame{Text: input}})
	if got.Applied || got.Conversation != input || len(got.Regions) != 0 {
		t.Fatalf("oversized analysis = %#v", got)
	}
}

func TestAnalyzeDoesNotUseTemporalEvidenceAcrossFrameIdentityChange(t *testing.T) {
	current := Frame{Text: "result\nIndexing files (3s)\ngpt-5.6-sol high · /work", CurrentCommand: "codex", Columns: 80}
	previous := Frame{Text: "result\nIndexing files (2s)\ngpt-5.6-sol high · /work", CurrentCommand: "claude", Columns: 80}
	got := Analyze(Observation{Current: current, Previous: &previous})
	if got.Applied || got.Activity != ActivityUnknown || got.Conversation != current.Text {
		t.Fatalf("incompatible temporal analysis = %#v", got)
	}
}

func TestAnalyzeKeepsChromeOnlyFrameWhole(t *testing.T) {
	input := "›\n\ngpt-5.6-sol high · /work"
	got := Analyze(Observation{Current: Frame{Text: input}})
	if got.Applied || got.Conversation != input {
		t.Fatalf("chrome-only analysis = %#v", got)
	}
}

func TestAnalyzeDoesNotTrustAlternateScreenAlone(t *testing.T) {
	input := "report follows\ngpt-5.6-sol high · /tmp"
	got := Analyze(Observation{Current: Frame{Text: input, AlternateScreen: "on"}})
	if got.Applied || got.Conversation != input {
		t.Fatalf("alternate-screen decoy analysis = %#v", got)
	}
}

func TestAnalyzeDoesNotTreatBareModelFamilyAsIdentity(t *testing.T) {
	input := "report follows\nsonnet · ~ · main"
	got := Analyze(Observation{Current: Frame{Text: input}})
	if got.Applied || got.Conversation != input {
		t.Fatalf("bare-family decoy analysis = %#v", got)
	}
}

func TestAnalyzeFindsFooterAboveTrailingTerminalRows(t *testing.T) {
	input := "› do the work\n\n• Working (1s • esc to interrupt)\n\n› Try /help\n\ngpt-5.6-sol high · /work" + strings.Repeat("\n", 12)
	got := Analyze(Observation{Current: Frame{Text: input, AlternateScreen: "on"}})
	if !got.Applied || got.Activity != ActivityActive || strings.Contains(got.Conversation, "Try /help") || !strings.Contains(got.Conversation, "do the work") {
		t.Fatalf("trailing-row analysis = %#v", got)
	}
}
