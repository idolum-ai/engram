package app

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestExtractVisibleURLsValidatesTrimsAndDeduplicates(t *testing.T) {
	t.Parallel()
	capture := strings.Join([]string{
		"docs: https://example.com/guide?q=tmux&mode=phone",
		"again (https://example.com/guide?q=tmux&mode=phone)",
		"upper HTTPS://EXAMPLE.ORG/status",
		"sanitize https://example.net/build?mode=fast&access_token=secret-value",
		"reject https://alice:s3cr3t@example.net/private",
		"reject ftp://example.com/file",
		"reject abchttps://example.net/hidden",
		"reject https:///missing-host",
	}, "\n")
	want := []string{
		"https://example.com/guide?q=tmux&mode=phone",
		"HTTPS://EXAMPLE.ORG/status",
		"https://example.net/build?access_token=REDACTED&mode=fast",
	}
	if got := extractVisibleURLs(capture, 4); !reflect.DeepEqual(got, want) {
		t.Fatalf("visible URLs = %#v, want %#v", got, want)
	}
}

func TestExtractVisibleURLsPreservesDistinctGitHubFormsInAppearanceOrder(t *testing.T) {
	t.Parallel()
	publicURL := "https://github.com/idolum-ai/kenogram/pull/19"
	apiURL := "https://api.github.com/repos/idolum-ai/kenogram/pulls/19"
	capture := strings.Join([]string{
		"created " + publicURL,
		"checked " + apiURL,
		"repeated " + publicURL,
	}, "\n")

	want := []string{publicURL, apiURL}
	if got := extractVisibleURLs(capture, 4); !reflect.DeepEqual(got, want) {
		t.Fatalf("visible URLs = %#v, want exact first-seen forms %#v", got, want)
	}
	if got := extractVisibleURLs(capture, 1); !reflect.DeepEqual(got, want[:1]) {
		t.Fatalf("bounded visible URLs = %#v, want first visible URL %#v", got, want[:1])
	}
}

func TestExtractVisibleURLsKeepsBalancedSuffixesAndFragmentsAndRejectsMalformedQueries(t *testing.T) {
	t.Parallel()
	wikiURL := "https://en.wikipedia.org/wiki/Function_(mathematics)"
	fragmentURL := "https://example.test/docs#deployment"
	capture := strings.Join([]string{
		"reference " + wikiURL,
		"section (" + fragmentURL + ")",
		"reject https://example.test/cb?auth=session-secret;ignored=x&mode=fast",
	}, "\n")

	want := []string{wikiURL, fragmentURL}
	if got := extractVisibleURLs(capture, 4); !reflect.DeepEqual(got, want) {
		t.Fatalf("visible URLs = %#v, want balanced and fragment URLs %#v", got, want)
	}
}

func TestExtractVisibleURLsPreservesTerminalPunctuation(t *testing.T) {
	t.Parallel()
	want := []string{
		"https://en.wikipedia.org/wiki/Yahoo!",
		"https://example.test/report.",
	}
	if got := extractVisibleURLs(strings.Join(want, "\n"), 4); !reflect.DeepEqual(got, want) {
		t.Fatalf("visible URLs = %#v, want terminal punctuation preserved %#v", got, want)
	}
}

func TestExtractVisibleURLsRedactsComponentsAndRejectsSensitiveAuthority(t *testing.T) {
	t.Parallel()
	secret := "configured-secret-value"
	capture := strings.Join([]string{
		"https://example.test/callback#auth=session-secret&view=fast",
		"https://example.test/#/callback?access_token=oauth-secret&state=x",
		"https://example.test/#/return?access_token=oauth-secret&return=%2Fdashboard",
		"https://example.test/callback?MY_TOKEN=terminal-secret&view=fast",
		"https://example.test/#/callback?BUILD_SECRET=terminal-secret&state=x",
		"https://example.test/#/TOKEN=terminal-secret",
		"https://example.test/#/route;id=1",
		"https://example.test/TOKEN=terminal-secret",
		"https://example.test/a%2Fb/" + secret,
		"https://example.test/docs#section;subsection",
		"https://example.test/docs#section;v=1",
		"https://example.test/docs#auth=oauth-secret;state=x",
		"https://example.test/docs#auth=first%3Bsecond&state=x",
		"https://example.test/artifacts/" + secret + "?mode=fast",
		"https://" + secret + ".example.test/report",
		"https://sk-ant-secret-token.example.test/report",
		"https://TOKEN=terminal-secret.example.test/report",
		"https://example.test/?" + secret + "=x",
		"https://example.test/?sk-proj-" + "1234567890abcdef=x",
		"https://example.test/#" + secret + "=x",
	}, "\n")
	want := []string{
		"https://example.test/callback#auth=REDACTED&view=fast",
		"https://example.test/#/callback?access_token=REDACTED&state=x",
		"https://example.test/#/return?access_token=REDACTED&return=%2Fdashboard",
		"https://example.test/callback?MY_TOKEN=REDACTED&view=fast",
		"https://example.test/#/callback?BUILD_SECRET=REDACTED&state=x",
		"https://example.test/#/TOKEN=REDACTED",
		"https://example.test/#/route;id=1",
		"https://example.test/TOKEN=REDACTED",
		"https://example.test/a%2Fb/REDACTED",
		"https://example.test/docs#section;subsection",
		"https://example.test/docs#section;v=1",
		"https://example.test/docs#auth=REDACTED;state=x",
		"https://example.test/docs#auth=REDACTED&state=x",
		"https://example.test/artifacts/REDACTED?mode=fast",
	}
	if got := extractVisibleURLs(capture, 20, secret); !reflect.DeepEqual(got, want) {
		t.Fatalf("visible URLs = %#v, want component-safe URLs %#v", got, want)
	}
}

func TestExtractVisibleURLsRemovesUnmatchedWrapperBeforePunctuation(t *testing.T) {
	t.Parallel()
	url := "https://example.test/docs#deployment"
	want := []string{url + "."}
	if got := extractVisibleURLs("section ("+url+").", 4); !reflect.DeepEqual(got, want) {
		t.Fatalf("visible URLs = %#v, want unmatched wrapper removed %#v", got, want)
	}
}

func TestURLRangesCannotBecomeVisiblePaths(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "artifact.pdf")
	if err := os.WriteFile(path, []byte("artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	capture := "report: https://downloads.example.test" + path
	if got := extractVisiblePaths(capture, 4); len(got) != 0 {
		t.Fatalf("URL path became local path: %#v", got)
	}
	if got := extractVisibleURLs(capture, 4); len(got) != 1 || got[0] != "https://downloads.example.test"+path {
		t.Fatalf("URL extraction = %#v", got)
	}
	credentialURL := "https://user:password@downloads.example.test" + path
	if got := extractVisiblePaths(credentialURL, 4); len(got) != 0 {
		t.Fatalf("rejected credential URL became local path: %#v", got)
	}
	if got := extractVisibleURLs(credentialURL, 4); len(got) != 0 {
		t.Fatalf("credential-bearing URL was exposed: %#v", got)
	}
}

func TestVisiblePathsIncludeOnlyDownloadableRegularFiles(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "report.txt")
	directory := filepath.Join(root, "reports")
	symlink := filepath.Join(root, "report-link.txt")
	if err := os.WriteFile(file, []byte("report"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(file, symlink); err != nil {
		t.Fatal(err)
	}
	capture := strings.Join([]string{directory, symlink, file}, "\n")
	if got := extractVisiblePaths(capture, 4); !reflect.DeepEqual(got, []string{file}) {
		t.Fatalf("visible files = %#v, want only %q", got, file)
	}
}

func TestRenderVisibleReferencesOmitsCredentialShapedPaths(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "TOKEN=terminal-secret")
	if err := os.WriteFile(path, []byte("artifact"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := renderVisibleReferences(path)
	if got != "" {
		t.Fatalf("credential-shaped path remained actionable: %q", got)
	}
}

func TestReferenceRenderingFencesPathsOnlyWhenRequestedAndKeepsLinksPlain(t *testing.T) {
	t.Parallel()
	refs := visibleReferences{
		Paths: []string{"/tmp/report.pdf"},
		URLs:  []string{"https://example.com/report"},
	}
	plain := renderReferences(refs, false, 100)
	if strings.Contains(plain, "```") || !strings.Contains(plain, "files:\n1. /tmp/report.pdf") || !strings.Contains(plain, "links:\nhttps://example.com/report") {
		t.Fatalf("plain references = %q", plain)
	}
	if len(plain) > 100 {
		t.Fatalf("plain references exceeded budget: %d", len(plain))
	}

	fenced := renderReferences(refs, true, 120)
	if !strings.Contains(fenced, "files:\n```\n1. /tmp/report.pdf\n```") || !strings.Contains(fenced, "links:\nhttps://example.com/report") {
		t.Fatalf("fenced references = %q", fenced)
	}
	linkSection := fenced[strings.Index(fenced, "links:"):]
	if strings.Contains(linkSection, "```") {
		t.Fatalf("clickable links were fenced: %q", fenced)
	}
}

func TestSnapshotReferencesReserveRoomForLinks(t *testing.T) {
	root := t.TempDir()
	paths := []string{
		filepath.Join(root, strings.Repeat("p", 140)),
		filepath.Join(root, strings.Repeat("q", 140)),
	}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte("artifact"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	capture := strings.Join(append(paths, "https://example.com/important"), "\n")
	got, files := renderSnapshotReferenceSetWithFiles(visibleReferencesForCapture(capture), maxSnapshotReferenceBytes)
	if len(got) > maxSnapshotReferenceBytes || !strings.Contains(got, "files:\n```\n1. ") || !strings.Contains(got, "links:\nhttps://example.com/important") {
		t.Fatalf("snapshot reference allocation = %q", got)
	}
	if len(files) == 0 || len(files) >= len(paths) {
		t.Fatalf("displayed file binding = %#v, want the exact budgeted subset", files)
	}
}
