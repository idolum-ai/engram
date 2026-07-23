package keyseq

import (
	"encoding/json"
	"os"
	"testing"
)

type interpretationFixture struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Expected    Proposal `json:"expected"`
}

func TestInterpretationFixturesAreClosedValidContracts(t *testing.T) {
	raw, err := os.ReadFile("testdata/interpretation_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []interpretationFixture
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	if len(fixtures) < 10 {
		t.Fatalf("fixture count = %d, want broad language and safety coverage", len(fixtures))
	}
	names := map[string]bool{}
	hasClarification := false
	hasSequence := false
	for _, fixture := range fixtures {
		if fixture.Name == "" || fixture.Description == "" || names[fixture.Name] {
			t.Fatalf("invalid fixture identity: %#v", fixture)
		}
		names[fixture.Name] = true
		expected, err := Validate(fixture.Expected)
		if err != nil {
			t.Fatalf("%s: invalid expected proposal: %v", fixture.Name, err)
		}
		switch expected.Kind {
		case KindSequence:
			hasSequence = true
			if _, err := Compile(expected); err != nil {
				t.Fatalf("%s: expected proposal does not compile: %v", fixture.Name, err)
			}
		case KindClarification:
			hasClarification = true
		}
	}
	if !hasSequence || !hasClarification {
		t.Fatalf("fixtures must cover both authority and clarification paths")
	}
}
