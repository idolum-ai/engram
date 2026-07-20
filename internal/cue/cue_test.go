package cue

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestExtractFeaturesMakesBoundedRegexes(t *testing.T) {
	t.Parallel()
	features := ExtractFeatures(Context{Text: strings.Join([]string{
		"Review https://github.com/idolum-ai/engram/pull/38.",
		"All 164 modules passed in 20 seconds.",
	}, "\n")})
	if len(features) == 0 || len(features) > MaxFeaturesPerFrame {
		t.Fatalf("features = %#v", features)
	}
	urlPattern := features[0]
	if !strings.Contains(urlPattern, `pull/[0-9]+`) {
		t.Fatalf("URL feature = %q", urlPattern)
	}
	expression := regexp.MustCompile(urlPattern)
	if !expression.MatchString("https://github.com/idolum-ai/engram/pull/99") {
		t.Fatalf("URL feature %q did not match another PR", urlPattern)
	}
	if expression.MatchString("https://github.com/idolum-ai/other/pull/99") {
		t.Fatalf("URL feature %q generalized the repository", urlPattern)
	}
}

func TestObserveRetainsHashesUntilAssociationRepeats(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "cues.json")
	store := openTestStore(t, path)
	context := Context{Text: "Pull request https://github.com/idolum-ai/engram/pull/38 is ready for review."}
	prompt := "Review this pull request and report concrete findings."

	candidate, err := store.Observe(context, prompt, time.Unix(1, 0))
	if err != nil || candidate != nil {
		t.Fatalf("first Observe() candidate=%#v error=%v", candidate, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), prompt) || strings.Contains(string(data), "github.com") {
		t.Fatalf("first observation persisted plaintext:\n%s", data)
	}
	var persisted persistedState
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted.Observations) != 1 || len(persisted.Observations[0].FeatureHashes) == 0 {
		t.Fatalf("persisted observations = %#v", persisted.Observations)
	}

	candidate, err = store.Observe(Context{Text: "Pull request https://github.com/idolum-ai/engram/pull/39 is ready for review."}, prompt, time.Unix(2, 0))
	if err != nil || candidate == nil {
		t.Fatalf("second Observe() candidate=%#v error=%v", candidate, err)
	}
	if candidate.Support != 2 || candidate.ConfidencePercent != 100 || !strings.Contains(candidate.Pattern, `pull/[0-9]+`) || candidate.Prompt != prompt {
		t.Fatalf("candidate = %#v", candidate)
	}
}

func TestObserveRequiresSpecificAssociation(t *testing.T) {
	t.Parallel()
	store := openTestStore(t, filepath.Join(t.TempDir(), "cues.json"))
	context := Context{Text: "Build failed with exit status 1 after the verification command."}
	if candidate, err := store.Observe(context, "Inspect the failing verification output.", time.Unix(1, 0)); err != nil || candidate != nil {
		t.Fatalf("first candidate=%#v error=%v", candidate, err)
	}
	if candidate, err := store.Observe(context, "Try a completely different recovery action.", time.Unix(2, 0)); err != nil || candidate != nil {
		t.Fatalf("conflicting candidate=%#v error=%v", candidate, err)
	}
	if candidate, err := store.Observe(context, "Inspect the failing verification output.", time.Unix(3, 0)); err != nil || candidate != nil {
		t.Fatalf("weak association candidate=%#v error=%v", candidate, err)
	}
}

func TestCandidateAcceptMatchUseAndForget(t *testing.T) {
	t.Parallel()
	store := openTestStore(t, filepath.Join(t.TempDir(), "cues.json"))
	context := Context{Text: "The checks passed for release candidate 18."}
	prompt := "Prepare the release pull request using the documented process."
	if candidate, err := store.Observe(context, prompt, time.Unix(1, 0)); err != nil || candidate != nil {
		t.Fatalf("first Observe() candidate=%#v error=%v", candidate, err)
	}
	candidate, err := store.Observe(Context{Text: "The checks passed for release candidate 19."}, prompt, time.Unix(2, 0))
	if err != nil || candidate == nil {
		t.Fatalf("second Observe() candidate=%#v error=%v", candidate, err)
	}
	if err := store.BindProposal(candidate.ID, 100, 77); err != nil {
		t.Fatal(err)
	}
	accepted, ok, err := store.Accept(candidate.ID, 100, 77, time.Unix(3, 0))
	if err != nil || !ok {
		t.Fatalf("Accept() cue=%#v ok=%v error=%v", accepted, ok, err)
	}
	matches := store.Matches(Context{Text: "The checks passed for release candidate 20."}, 2)
	if len(matches) != 1 || matches[0].CueID != accepted.ID || matches[0].Prompt != prompt || matches[0].MatchHash == "" {
		t.Fatalf("Matches() = %#v", matches)
	}
	if err := store.RecordUse(accepted.ID); err != nil {
		t.Fatal(err)
	}
	removed, found, err := store.Forget(accepted.Name)
	if err != nil || !found || removed.ID != accepted.ID {
		t.Fatalf("Forget() cue=%#v found=%v error=%v", removed, found, err)
	}
	if got := store.Matches(Context{Text: context.Text}, 2); len(got) != 0 {
		t.Fatalf("forgotten cue still matches: %#v", got)
	}
}

func TestCandidateRejectSuppressesSameAssociation(t *testing.T) {
	t.Parallel()
	store := openTestStore(t, filepath.Join(t.TempDir(), "cues.json"))
	context := Context{Text: "Pull request https://github.com/idolum-ai/engram/pull/38 is ready for review."}
	prompt := "Review this pull request and report concrete findings."
	_, _ = store.Observe(context, prompt, time.Unix(1, 0))
	candidate, _ := store.Observe(context, prompt, time.Unix(2, 0))
	if candidate == nil {
		t.Fatal("candidate was not learned")
	}
	if err := store.BindProposal(candidate.ID, 100, 77); err != nil {
		t.Fatal(err)
	}
	if rejected, err := store.Reject(candidate.ID, 100, 77); err != nil || !rejected {
		t.Fatalf("Reject() rejected=%v error=%v", rejected, err)
	}
	if candidate, err := store.Observe(context, prompt, time.Unix(3, 0)); err != nil || candidate != nil {
		t.Fatalf("suppressed association candidate=%#v error=%v", candidate, err)
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(store.path), "cues.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), prompt) {
		t.Fatalf("rejected prompt remained in state:\n%s", data)
	}
}

func TestManualCueValidationAndPrivateFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "cues.json")
	store := openTestStore(t, path)
	if _, err := store.Add("bad name", "ready", "Run the release review now.", time.Time{}); err == nil {
		t.Fatal("Add() accepted invalid name")
	}
	if _, err := store.Add("review", "(", "Run the release review now.", time.Time{}); err == nil {
		t.Fatal("Add() accepted invalid regex")
	}
	if _, err := store.Add("review", "ready", "short", time.Time{}); err == nil {
		t.Fatal("Add() accepted short prompt")
	}
	if _, err := store.Add("review", "ready", "Review this output:\n```text\nready\n```", time.Time{}); err == nil {
		t.Fatal("Add() accepted a prompt that cannot be rendered in the cue block")
	}
	if _, err := store.Add("review", `ready for review`, "Run the release review now.", time.Time{}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("cue state mode = %o, want 600", info.Mode().Perm())
	}
}

func TestOpenRejectsSymlinkedCueState(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	realPath := filepath.Join(dir, "real.json")
	if err := os.WriteFile(realPath, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(dir, "cues.json")
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(linkPath); err == nil {
		t.Fatal("Open() followed a cue state symlink")
	}
}

func TestVoiceMarkersAreNotLearned(t *testing.T) {
	t.Parallel()
	store := openTestStore(t, filepath.Join(t.TempDir(), "cues.json"))
	context := Context{Text: "The pull request is ready for review with all checks passing."}
	for _, prompt := range []string{"(transcribed) please review it", "(voice message: /tmp/private.ogg)"} {
		if candidate, err := store.Observe(context, prompt, time.Now()); err != nil || candidate != nil {
			t.Fatalf("Observe(%q) candidate=%#v error=%v", prompt, candidate, err)
		}
	}
	if got := store.Snapshot().Observations; got != 0 {
		t.Fatalf("voice observations = %d", got)
	}
}

func openTestStore(t *testing.T, path string) *Store {
	t.Helper()
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return store
}
