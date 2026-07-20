package cue

import (
	"encoding/json"
	"fmt"
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
	var repositoryPattern, shapePattern string
	for _, pattern := range features {
		switch {
		case strings.HasPrefix(pattern, `https`) && strings.Contains(pattern, `engram/pull/[0-9]+`):
			repositoryPattern = pattern
		case strings.HasPrefix(pattern, `https`) && strings.Contains(pattern, `/[^/\s]+/pull/[0-9]+`):
			shapePattern = pattern
		}
	}
	if repositoryPattern == "" || shapePattern == "" {
		t.Fatalf("URL features = %#v", features)
	}
	expression := regexp.MustCompile(repositoryPattern)
	if !expression.MatchString("https://github.com/idolum-ai/engram/pull/99") {
		t.Fatalf("URL feature %q did not match another PR", repositoryPattern)
	}
	if expression.MatchString("https://github.com/idolum-ai/other/pull/99") {
		t.Fatalf("URL feature %q generalized the repository", repositoryPattern)
	}
	shape := regexp.MustCompile(shapePattern)
	for _, target := range []string{
		"https://github.com/idolum-ai/engram/pull/99",
		"https://github.com/idolum-ai/grimoire/pull/6",
	} {
		if !shape.MatchString(target) {
			t.Errorf("URL shape %q did not match %q", shapePattern, target)
		}
	}
	for _, target := range []string{
		"https://github.com/another-owner/engram/pull/99",
		"https://github.com/idolum-ai/engram/issues/99",
	} {
		if shape.MatchString(target) {
			t.Errorf("URL shape %q overgeneralized to %q", shapePattern, target)
		}
	}
}

func TestObserveLetsUnsafeVariantSupportSafeLongCue(t *testing.T) {
	t.Parallel()
	store := openTestStore(t, filepath.Join(t.TempDir(), "cues.json"))
	safe := "Imagine that you had the ideal panel to review this pull request. Who would those personas be? What would be their background? And what would be their obsession? Send sub-agents with those personas to review the pull request, gather all feedback into one packet, address it, and repeat until the panel is satisfied. Then let me know and I will review the pull request."
	unsafe := "Regarding PR #28, " + safe
	observations := []struct {
		context Context
		prompt  string
	}{
		{Context{Text: "https://github.com/idolum-ai/grimoire/pull/6"}, safe},
		{Context{Text: "https://github.com/idolum-ai/engram/pull/26"}, safe},
		{Context{Text: "https://github.com/idolum-ai/engram/pull/28"}, unsafe},
	}
	var candidate *Candidate
	for index, observation := range observations {
		var err error
		candidate, err = store.Observe(observation.context, observation.prompt, time.Unix(int64(index+1), 0))
		if err != nil {
			t.Fatal(err)
		}
	}
	if candidate == nil {
		t.Fatal("long semantic cluster did not produce a candidate")
	}
	if candidate.Name != "review-panel" || candidate.Prompt != safe || candidate.Support != 3 || candidate.ConfidencePercent != 100 {
		t.Fatalf("candidate = %#v", candidate)
	}
	if !strings.Contains(candidate.Pattern, `/[^/\s]+/pull/[0-9]+`) {
		t.Fatalf("candidate pattern = %q", candidate.Pattern)
	}
	if len(candidate.Variants) != 1 || candidate.Variants[0] != safe || promptReplaySafe(unsafe) {
		t.Fatalf("candidate variants = %#v; unsafe replay=%v", candidate.Variants, promptReplaySafe(unsafe))
	}
	data, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "PR #28") {
		t.Fatalf("unsafe evidence persisted in cue state:\n%s", data)
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
	if err != nil || candidate != nil {
		t.Fatalf("second Observe() candidate=%#v error=%v", candidate, err)
	}
	candidate, err = store.Observe(Context{Text: "Pull request https://github.com/idolum-ai/engram/pull/40 is ready for review."}, prompt, time.Unix(3, 0))
	if err != nil || candidate == nil {
		t.Fatalf("third Observe() candidate=%#v error=%v", candidate, err)
	}
	if candidate.Name == "" {
		t.Fatal("candidate did not receive a compact name")
	}
	if candidate.Support != 3 || candidate.ConfidencePercent != 100 || !strings.Contains(candidate.Pattern, `pull/[0-9]+`) || candidate.Prompt != prompt {
		t.Fatalf("candidate = %#v", candidate)
	}
}

func TestObserveClustersSimilarPromptsInMemory(t *testing.T) {
	t.Parallel()
	store := openTestStore(t, filepath.Join(t.TempDir(), "cues.json"))
	prompts := []string{
		"Review this pull request and report concrete findings.",
		"Can you please review the PR and report concrete findings?",
		"Send reviewers to check this pull request and report findings.",
	}
	var candidate *Candidate
	for index, prompt := range prompts {
		context := Context{Text: fmt.Sprintf("Pull request https://github.com/idolum-ai/engram/pull/%d is ready for review.", 38+index)}
		var err error
		candidate, err = store.Observe(context, prompt, time.Unix(int64(index+1), 0))
		if err != nil {
			t.Fatal(err)
		}
		if index < 2 && candidate != nil {
			t.Fatalf("candidate appeared after %d observations: %#v", index+1, candidate)
		}
	}
	if candidate == nil || candidate.Support != 3 || candidate.ConfidencePercent != 100 || len(candidate.Variants) != 3 {
		t.Fatalf("semantic candidate = %#v", candidate)
	}
	if candidate.Prompt != prompts[0] {
		t.Fatalf("representative prompt = %q, want shortest %q", candidate.Prompt, prompts[0])
	}
	snapshot := store.Snapshot()
	snapshot.Candidates[0].Variants[0] = "mutated"
	if got := store.Snapshot().Candidates[0].Variants[0]; got == "mutated" {
		t.Fatal("Snapshot exposed persisted candidate variants")
	}
}

func TestSemanticSimilarityDoesNotSurviveRestart(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "cues.json")
	store := openTestStore(t, path)
	context := Context{Text: "Pull request https://github.com/idolum-ai/engram/pull/38 is ready for review."}
	for index, prompt := range []string{
		"Review this pull request and report concrete findings.",
		"Can you please review the PR and report concrete findings?",
	} {
		if candidate, err := store.Observe(context, prompt, time.Unix(int64(index+1), 0)); err != nil || candidate != nil {
			t.Fatalf("pre-restart candidate=%#v error=%v", candidate, err)
		}
	}
	store = openTestStore(t, path)
	if candidate, err := store.Observe(context, "Send reviewers to check this pull request and report findings.", time.Unix(3, 0)); err != nil || candidate != nil {
		t.Fatalf("post-restart semantic candidate=%#v error=%v", candidate, err)
	}
}

func TestExactHashAssociationSurvivesRestart(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "cues.json")
	store := openTestStore(t, path)
	context := Context{Text: "Pull request https://github.com/idolum-ai/engram/pull/38 is ready for review."}
	prompt := "Review this pull request and report concrete findings."
	for index := 0; index < 2; index++ {
		if candidate, err := store.Observe(context, prompt, time.Unix(int64(index+1), 0)); err != nil || candidate != nil {
			t.Fatalf("pre-restart candidate=%#v error=%v", candidate, err)
		}
	}
	store = openTestStore(t, path)
	candidate, err := store.Observe(context, prompt, time.Unix(3, 0))
	if err != nil || candidate == nil || candidate.Support != 3 {
		t.Fatalf("post-restart exact candidate=%#v error=%v", candidate, err)
	}
}

func TestObserveAllowsOneTerminalFeatureToSupportSeveralActions(t *testing.T) {
	t.Parallel()
	store := openTestStore(t, filepath.Join(t.TempDir(), "cues.json"))
	context := Context{Text: "Pull request https://github.com/idolum-ai/engram/pull/38 is ready for review."}
	for index, prompt := range []string{
		"Explain the current architecture in plain language.",
		"Run the complete test suite and report failures.",
		"Check whether the documentation needs an update.",
		"Review this pull request and report concrete findings.",
		"Can you please review the PR and report concrete findings?",
		"Send reviewers to check this pull request and report findings.",
	} {
		candidate, err := store.Observe(context, prompt, time.Unix(int64(index+1), 0))
		if err != nil {
			t.Fatal(err)
		}
		if index == 5 && candidate == nil {
			t.Fatal("common terminal feature prevented a specific intent proposal")
		}
	}
}

func TestSemanticClusterUsesItsOwnConsistencyOverOneOffExactWording(t *testing.T) {
	t.Parallel()
	store := openTestStore(t, filepath.Join(t.TempDir(), "cues.json"))
	target := Context{Text: "Pull request https://github.com/idolum-ai/engram/pull/38 is ready for review."}
	other := Context{Text: "The local documentation build completed successfully after 38 seconds."}
	observations := []struct {
		context Context
		prompt  string
	}{
		{target, "Review this pull request and report concrete findings."},
		{target, "Can you please review the PR and report concrete findings?"},
		{other, "Review this PR and report concrete findings."},
		{target, "Reviewers should inspect this PR and report concrete findings."},
	}
	var candidate *Candidate
	for index, observation := range observations {
		var err error
		candidate, err = store.Observe(observation.context, observation.prompt, time.Unix(int64(index+1), 0))
		if err != nil {
			t.Fatal(err)
		}
	}
	if candidate == nil || candidate.Support != 3 || candidate.ConfidencePercent != 75 {
		t.Fatalf("75%% semantic candidate = %#v", candidate)
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
	if candidate, err := store.Observe(Context{Text: "The checks passed for release candidate 19."}, prompt, time.Unix(2, 0)); err != nil || candidate != nil {
		t.Fatalf("second Observe() candidate=%#v error=%v", candidate, err)
	}
	candidate, err := store.Observe(Context{Text: "The checks passed for release candidate 20."}, prompt, time.Unix(3, 0))
	if err != nil || candidate == nil {
		t.Fatalf("third Observe() candidate=%#v error=%v", candidate, err)
	}
	if err := store.BindProposal(candidate.ID, 100, 77); err != nil {
		t.Fatal(err)
	}
	accepted, ok, err := store.Accept(candidate.ID, 100, 77, time.Unix(3, 0))
	if err != nil || !ok {
		t.Fatalf("Accept() cue=%#v ok=%v error=%v", accepted, ok, err)
	}
	if accepted.Name != candidate.Name {
		t.Fatalf("accepted cue name = %q, want %q", accepted.Name, candidate.Name)
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
	_, _ = store.Observe(context, prompt, time.Unix(2, 0))
	candidate, _ := store.Observe(context, prompt, time.Unix(3, 0))
	if candidate == nil {
		t.Fatal("candidate was not learned")
	}
	if err := store.BindProposal(candidate.ID, 100, 77); err != nil {
		t.Fatal(err)
	}
	if rejected, err := store.Reject(candidate.ID, 100, 77); err != nil || !rejected {
		t.Fatalf("Reject() rejected=%v error=%v", rejected, err)
	}
	if candidate, err := store.Observe(context, prompt, time.Unix(4, 0)); err != nil || candidate != nil {
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
