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

func TestPromptLearnableExcludesConcreteReferences(t *testing.T) {
	t.Parallel()
	for _, prompt := range []string{
		"Review https://github.com/idolum-ai/engram/pull/39.",
		"Inspect /tmp/generated-report.md and summarize it.",
		"Open ~/code/project/output.txt and report its contents.",
		"merged #39",
		"merged! please continue with the remaining work",
		"the release was published; update the service",
		"continue",
		"please proceed",
	} {
		if promptLearnable(prompt) {
			t.Errorf("promptLearnable(%q) = true", prompt)
		}
	}
	if !promptLearnable("Review this pull request and report concrete findings.") {
		t.Fatal("generic review prompt was not learnable")
	}
}
