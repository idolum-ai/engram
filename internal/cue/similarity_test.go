package cue

import "testing"

func TestIntentSimilarityRecognizesLocalParaphrases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		left  string
		right string
	}{
		{"Review this pull request.", "Can you please review the PR?"},
		{"Review this pull request.", "Send reviewers to check this pull request."},
		{"Run the tests and report the findings.", "Please run tests, then report findings."},
	}
	for _, test := range cases {
		if score := intentSimilarity(makeIntentSignature(test.left), makeIntentSignature(test.right)); score < IntentSimilarityThreshold {
			t.Errorf("intentSimilarity(%q, %q) = %d, want >= %d", test.left, test.right, score, IntentSimilarityThreshold)
		}
	}
}

func TestIntentSimilarityKeepsDifferentActionsApart(t *testing.T) {
	t.Parallel()
	cases := []struct {
		left  string
		right string
	}{
		{"Review this pull request.", "Restart the installed service."},
		{"Run the tests and report the findings.", "Merge the pull request."},
		{"Download the generated report.", "Delete the stale tmux window."},
	}
	for _, test := range cases {
		if score := intentSimilarity(makeIntentSignature(test.left), makeIntentSignature(test.right)); score >= IntentSimilarityThreshold {
			t.Errorf("intentSimilarity(%q, %q) = %d, want < %d", test.left, test.right, score, IntentSimilarityThreshold)
		}
	}
}

func TestPromptReplaySafetyIsSeparateFromSemanticEvidence(t *testing.T) {
	t.Parallel()
	unsafe := []string{
		"Review https://github.com/idolum-ai/engram/pull/39.",
		"Inspect /tmp/generated-report.md and summarize it.",
		"Open ~/code/project/output.txt and report its contents.",
		"merged! please continue with the remaining work",
		"the release was published; update the service",
	}
	for _, prompt := range unsafe {
		if !promptLearnable(prompt) {
			t.Errorf("promptLearnable(%q) = false; unsafe variants should still supply evidence", prompt)
		}
		if promptReplaySafe(prompt) {
			t.Errorf("promptReplaySafe(%q) = true", prompt)
		}
	}
	for _, prompt := range []string{"merged #39", "continue", "please proceed"} {
		if promptLearnable(prompt) {
			t.Errorf("promptLearnable(%q) = true", prompt)
		}
	}
	generic := "Review this pull request and report concrete findings."
	if !promptLearnable(generic) || !promptReplaySafe(generic) {
		t.Fatal("generic review prompt was not learnable")
	}
}

func TestLongPanelPromptProducesCompactName(t *testing.T) {
	t.Parallel()
	prompt := "Then imagine if you had the ideal panel to review this pull request. Who would those personas be? What would be their background? What would be their obsession? Send sub-agents with those personas to review it, gather all feedback into one packet, address it, and repeat until the panel is satisfied. After that, let me know and I will review the pull request."
	if !promptLearnable(prompt) {
		t.Fatalf("%d-byte prompt was not learnable", len(prompt))
	}
	if got := suggestCueName(prompt); got != "review-panel" {
		t.Fatalf("suggestCueName() = %q, want review-panel", got)
	}
}

func TestCueNameIsValidForNonEnglishIntent(t *testing.T) {
	t.Parallel()
	got := suggestCueName("Analiza cuidadosamente la salida y explica el resultado completo.")
	if err := validateCueName(got); err != nil {
		t.Fatalf("suggestCueName() = %q: %v", got, err)
	}
}
