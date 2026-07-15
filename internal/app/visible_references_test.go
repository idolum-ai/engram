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
		"docs: https://example.com/guide?q=tmux&mode=phone.",
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

func TestReferenceRenderingFencesPathsOnlyWhenRequestedAndKeepsLinksPlain(t *testing.T) {
	t.Parallel()
	refs := visibleReferences{
		Paths: []string{"/tmp/report.pdf"},
		URLs:  []string{"https://example.com/report"},
	}
	plain := renderReferences(refs, false, 100)
	if strings.Contains(plain, "```") || !strings.Contains(plain, "paths:\n/tmp/report.pdf") || !strings.Contains(plain, "links:\nhttps://example.com/report") {
		t.Fatalf("plain references = %q", plain)
	}
	if len(plain) > 100 {
		t.Fatalf("plain references exceeded budget: %d", len(plain))
	}

	fenced := renderReferences(refs, true, 120)
	if !strings.Contains(fenced, "paths:\n```\n/tmp/report.pdf\n```") || !strings.Contains(fenced, "links:\nhttps://example.com/report") {
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
	got := renderSnapshotReferences(capture, maxSnapshotReferenceBytes)
	if len(got) > maxSnapshotReferenceBytes || !strings.Contains(got, "paths:\n") || !strings.Contains(got, "links:\nhttps://example.com/important") {
		t.Fatalf("snapshot reference allocation = %q", got)
	}
}
