package keyseq

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseCanonicalizesSequence(t *testing.T) {
	got, err := Parse(`{
		"kind":"sequence",
		"events":[
			{"key":"up","modifiers":[],"count":3},
			{"key":"c","modifiers":["control"],"count":1},
			{"key":"f4","modifiers":["alt"],"count":1}
		]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	want := Proposal{Kind: KindSequence, Events: []Event{
		{Key: KeyUp, Count: 3},
		{Key: KeyC, Modifiers: []Modifier{ModifierControl}, Count: 1},
		{Key: KeyF4, Modifiers: []Modifier{ModifierAlt}, Count: 1},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("proposal = %#v, want %#v", got, want)
	}
}

func TestParseAcceptsClarificationWithoutProviderProse(t *testing.T) {
	got, err := Parse(`{"kind":"clarification","events":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != KindClarification || len(got.Events) != 0 {
		t.Fatalf("proposal = %#v", got)
	}
}

func TestParseNormalizesStructuredEnumCasing(t *testing.T) {
	got, err := Parse(`{"kind":"Sequence","events":[{"key":"ENTER","modifiers":[],"count":1}]}`)
	if err != nil {
		t.Fatal(err)
	}
	want := Proposal{Kind: KindSequence, Events: []Event{{Key: KeyEnter, Count: 1}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("proposal = %#v, want %#v", got, want)
	}
}

func TestParseRejectsUntrustedShapes(t *testing.T) {
	tests := map[string]string{
		"markdown":           "```json\n{\"kind\":\"clarification\",\"events\":[]}\n```",
		"trailing":           `{"kind":"clarification","events":[]} ignored`,
		"unknown field":      `{"kind":"clarification","events":[],"message":"press enter"}`,
		"missing kind":       `{"events":[]}`,
		"sequence empty":     `{"kind":"sequence","events":[]}`,
		"unknown key":        `{"kind":"sequence","events":[{"key":"launch","modifiers":[],"count":1}]}`,
		"unknown modifier":   `{"kind":"sequence","events":[{"key":"c","modifiers":["command"],"count":1}]}`,
		"duplicate modifier": `{"kind":"sequence","events":[{"key":"c","modifiers":["control","control"],"count":1}]}`,
		"zero count":         `{"kind":"sequence","events":[{"key":"enter","modifiers":[],"count":0}]}`,
		"large count":        `{"kind":"sequence","events":[{"key":"enter","modifiers":[],"count":33}]}`,
		"shift digit":        `{"kind":"sequence","events":[{"key":"1","modifiers":["shift"],"count":1}]}`,
		"combined modifiers": `{"kind":"sequence","events":[{"key":"c","modifiers":["control","shift"],"count":1}]}`,
		"control enter":      `{"kind":"sequence","events":[{"key":"enter","modifiers":["control"],"count":1}]}`,
		"alt arrow":          `{"kind":"sequence","events":[{"key":"up","modifiers":["alt"],"count":1}]}`,
		"shift enter":        `{"kind":"sequence","events":[{"key":"enter","modifiers":["shift"],"count":1}]}`,
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(input); err == nil {
				t.Fatalf("Parse(%q) succeeded", input)
			} else if !errors.Is(err, ErrInvalidProposal) {
				t.Fatalf("Parse(%q) error = %v, want ErrInvalidProposal", input, err)
			}
		})
	}
}

func TestParseDiscardsInertEventsFromClarification(t *testing.T) {
	got, err := Parse(`{"kind":"clarification","events":[{"key":"enter","modifiers":[],"count":0}]}`)
	if err != nil {
		t.Fatal(err)
	}
	want := Proposal{Kind: KindClarification}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("proposal = %#v, want %#v", got, want)
	}
}

func TestCompileBoundsTotalDelayedEscapeGesture(t *testing.T) {
	accepted, err := Compile(Proposal{Kind: KindSequence, Events: []Event{{
		Key: KeyEscape, Count: MaxDelayedEscapeTransitions + 1,
	}}})
	if err != nil || len(accepted.Groups) != MaxDelayedEscapeTransitions+1 {
		t.Fatalf("maximum delayed gesture = %#v err=%v", accepted, err)
	}
	if _, err := Compile(Proposal{Kind: KindSequence, Events: []Event{{
		Key: KeyEscape, Count: MaxDelayedEscapeTransitions + 2,
	}}}); err == nil {
		t.Fatal("plan exceeding delayed Escape budget compiled")
	}
}

func TestParseBoundsExpandedEvents(t *testing.T) {
	var events []string
	for range MaxExpandedEvents + 1 {
		events = append(events, `{"key":"up","modifiers":[],"count":1}`)
	}
	input := `{"kind":"sequence","events":[` + strings.Join(events, ",") + `]}`
	if _, err := Parse(input); err == nil {
		t.Fatal("oversized sequence accepted")
	}
}

func TestCompileProducesOnlyCanonicalTmuxKeys(t *testing.T) {
	proposal := Proposal{Kind: KindSequence, Events: []Event{
		{Key: KeyUp, Count: 3},
		{Key: KeyEnter, Count: 2},
		{Key: KeyC, Modifiers: []Modifier{ModifierControl}, Count: 1},
		{Key: KeyF4, Modifiers: []Modifier{ModifierAlt}, Count: 1},
	}}
	plan, err := Compile(proposal)
	if err != nil {
		t.Fatal(err)
	}
	want := Plan{Groups: []Group{{Keys: []string{"Up", "Up", "Up", "Enter", "Enter", "C-c", "M-F4"}}}, EventCount: 7}
	if !reflect.DeepEqual(plan, want) {
		t.Fatalf("plan = %#v, want %#v", plan, want)
	}
}

func TestCompilePreservesSeparatedEscapeGesture(t *testing.T) {
	plan, err := Compile(Proposal{Kind: KindSequence, Events: []Event{
		{Key: KeyEscape, Count: 2},
		{Key: KeyUp, Count: 1},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := Plan{Groups: []Group{
		{Keys: []string{"Escape"}, DelayAfter: 500 * time.Millisecond},
		{Keys: []string{"Escape", "Up"}},
	}, EventCount: 3}
	if !reflect.DeepEqual(plan, want) {
		t.Fatalf("plan = %#v, want %#v", plan, want)
	}
}

func TestValidateCanonicalizesAdjacentEquivalentEvents(t *testing.T) {
	got, err := Validate(Proposal{Kind: KindSequence, Events: []Event{
		{Key: KeyEscape, Count: 1},
		{Key: KeyEscape, Count: 1},
		{Key: KeyC, Modifiers: []Modifier{ModifierControl}, Count: 1},
		{Key: KeyC, Modifiers: []Modifier{ModifierControl}, Count: 2},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := Proposal{Kind: KindSequence, Events: []Event{
		{Key: KeyEscape, Count: 2},
		{Key: KeyC, Modifiers: []Modifier{ModifierControl}, Count: 3},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("proposal = %#v, want %#v", got, want)
	}
}

func TestJSONSchemaConstrainsProviderShapeBeforeDeterministicBounds(t *testing.T) {
	schema := JSONSchema()
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema properties = %#v", schema["properties"])
	}
	events, ok := properties["events"].(map[string]any)
	if !ok {
		t.Fatalf("event schema = %#v", properties["events"])
	}
	items := events["items"].(map[string]any)
	eventProperties := items["properties"].(map[string]any)
	count := eventProperties["count"].(map[string]any)
	if count["type"] != "integer" {
		t.Fatalf("count schema = %#v", count)
	}
}

func TestFormatIsExactAndPhoneReadable(t *testing.T) {
	got := Format(Proposal{Kind: KindSequence, Events: []Event{
		{Key: KeyUp, Count: 3},
		{Key: KeyEnter, Count: 2},
		{Key: KeyC, Modifiers: []Modifier{ModifierControl}, Count: 1},
		{Key: KeyF4, Modifiers: []Modifier{ModifierAlt}, Count: 1},
	}})
	if got != "↑ ×3  Enter ×2\nCtrl+C  Alt+F4" {
		t.Fatalf("format = %q", got)
	}
}

func TestBuildPromptContainsOnlyDescriptionEnvelope(t *testing.T) {
	got := BuildPrompt("up three times, then enter")
	if got != `KEY_DESCRIPTION_JSON
{"description":"up three times, then enter"}` {
		t.Fatalf("prompt = %q", got)
	}
}
